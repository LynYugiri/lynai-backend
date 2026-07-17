package market

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
)

const (
	pluginUploadMaxBytes   int64 = 16 << 20
	pluginManifestMaxBytes int64 = 1 << 20
)

// Handler exposes the /market/* API endpoints.
type Handler struct {
	svc *Service
}

// NewHandler creates a market handler bound to the given service.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// pluginEntryResponse serializes a Plugin in the camelCase format the Flutter
// MarketPluginEntry.fromJson expects.
type pluginEntryResponse struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Author      string   `json:"author"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	IconURL     *string  `json:"iconUrl"`
	Screenshots []string `json:"screenshots"`
	Permissions []string `json:"permissions"`
	DownloadURL string   `json:"downloadUrl"`
	SHA256      *string  `json:"sha256"`
	Category    string   `json:"category"`
	Status      string   `json:"status"`
	ReviewNote  *string  `json:"reviewNote"`
}

func toPluginResponse(p *database.Plugin) pluginEntryResponse {
	return pluginEntryResponse{
		ID:          p.ID,
		Name:        p.Name,
		Author:      p.Author,
		Description: p.Description,
		Version:     p.Version,
		IconURL:     p.IconURL,
		Screenshots: screenshotURLs(p.Screenshots),
		Permissions: permissionNames(p.Permissions),
		DownloadURL: pluginDownloadURL(p),
		SHA256:      p.SHA256,
		Category:    p.Category,
		Status:      p.Status,
		ReviewNote:  p.ReviewNote,
	}
}

func pluginDownloadURL(p *database.Plugin) string {
	if p.Status != database.PluginStatusApproved {
		return ""
	}
	return fmt.Sprintf("/market/plugins/%s/download", p.ID)
}

// ListPlugins handles GET /market/plugins.
// Query params: category, q, page (default 1), page_size (default 20).
func (h *Handler) ListPlugins(c *gin.Context) {
	category := c.Query("category")
	query := c.Query("q")
	page := parsePositiveInt(c.Query("page"), 1)
	pageSize := normalizePageSize(parsePositiveInt(c.Query("page_size"), 20))

	plugins, total, err := h.svc.ListPlugins(category, query, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list plugins"})
		return
	}

	entries := make([]pluginEntryResponse, 0, len(plugins))
	for i := range plugins {
		entries = append(entries, toPluginResponse(&plugins[i]))
	}

	hasMore := int64(page*pageSize) < total
	c.JSON(http.StatusOK, gin.H{
		"entries": entries,
		"hasMore": hasMore,
	})
}

// GetPluginDetail handles GET /market/plugins/:id.
func (h *Handler) GetPluginDetail(c *gin.Context) {
	id := c.Param("id")
	plugin, err := h.svc.GetPlugin(id)
	if err != nil {
		if err == ErrPluginNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, toPluginResponse(plugin))
}

// DownloadPlugin handles GET /market/plugins/:id/download.
func (h *Handler) DownloadPlugin(c *gin.Context) {
	id := c.Param("id")
	plugin, err := h.svc.GetPlugin(id)
	if err != nil {
		if err == ErrPluginNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	fullPath := h.svc.storage.FullPath(plugin.ZipPath)
	c.File(fullPath)

	// Best-effort increment; don't fail the download if the counter update errors.
	go h.svc.IncrementDownloadCount(id)
}

// updatesRequest is the body for POST /market/updates.
type updatesRequest struct {
	Installed []InstalledItem `json:"installed" binding:"required"`
}

// CheckUpdates handles POST /market/updates.
func (h *Handler) CheckUpdates(c *gin.Context) {
	var req updatesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates, err := h.svc.CheckUpdates(req.Installed)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"updates": updates})
}

// manifestJSON is the subset of plugin.json we extract from the uploaded ZIP.
type manifestJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Author      string   `json:"author"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	Icon        string   `json:"icon"`
	Permissions []string `json:"permissions"`
}

// SubmitPlugin handles POST /market/plugins/submit.
// Multipart form: field "zip" (the plugin ZIP file).
// The manifest is read from plugin.json inside the ZIP.
func (h *Handler) SubmitPlugin(c *gin.Context) {
	userIDStr := c.GetString("userID")
	userID, _ := strconv.ParseInt(userIDStr, 10, 64)

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, pluginUploadMaxBytes)
	err := c.Request.ParseMultipartForm(1 << 20)
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "plugin upload exceeds 16 MiB limit"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart upload"})
		return
	}

	file, err := c.FormFile("zip")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "zip file is required"})
		return
	}

	if !strings.HasSuffix(file.Filename, ".zip") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file must be a .zip"})
		return
	}

	src, err := file.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot read uploaded file"})
		return
	}
	defer src.Close()

	tempPath, err := h.svc.storage.StagePluginZip(src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to stage plugin file"})
		return
	}
	defer h.svc.storage.DeleteTemp(tempPath)

	manifest, sha, err := extractManifest(tempPath)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot read plugin.json from ZIP: %v", err)})
		return
	}
	if manifest.ID == "" || manifest.Name == "" || manifest.Version == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plugin.json must contain id, name, and version"})
		return
	}
	if !isSafePluginID(manifest.ID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plugin id contains invalid characters"})
		return
	}
	if !isSafePluginVersion(manifest.Version) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plugin version contains invalid characters"})
		return
	}

	plugin, err := h.svc.UpsertSubmission(manifest, tempPath, &sha, userID)
	if err != nil {
		if errors.Is(err, ErrPluginNotOwned) {
			c.JSON(http.StatusForbidden, gin.H{"error": "plugin id belongs to another submitter"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, toPluginResponse(plugin))
}

// MySubmissions handles GET /market/submissions/mine.
func (h *Handler) MySubmissions(c *gin.Context) {
	userIDStr := c.GetString("userID")
	userID, _ := strconv.ParseInt(userIDStr, 10, 64)
	plugins, err := h.svc.ListBySubmitter(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	entries := make([]pluginEntryResponse, 0, len(plugins))
	for i := range plugins {
		entries = append(entries, toPluginResponse(&plugins[i]))
	}
	c.JSON(http.StatusOK, gin.H{"submissions": entries})
}

// ListPending handles GET /market/plugins/pending (admin only).
func (h *Handler) ListPending(c *gin.Context) {
	plugins, err := h.svc.ListPending()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	entries := make([]pluginEntryResponse, 0, len(plugins))
	for i := range plugins {
		entries = append(entries, toPluginResponse(&plugins[i]))
	}
	c.JSON(http.StatusOK, gin.H{"entries": entries})
}

// ApprovePlugin handles POST /market/plugins/:id/approve (admin only).
func (h *Handler) ApprovePlugin(c *gin.Context) {
	id := c.Param("id")
	reviewerIDStr := c.GetString("userID")
	reviewerID, _ := strconv.ParseInt(reviewerIDStr, 10, 64)
	if err := h.svc.Approve(id, reviewerID); err != nil {
		if err == ErrPluginNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found or not pending"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "approved"})
}

type rejectRequest struct {
	Reason string `json:"reason"`
}

// RejectPlugin handles POST /market/plugins/:id/reject (admin only).
func (h *Handler) RejectPlugin(c *gin.Context) {
	id := c.Param("id")
	reviewerIDStr := c.GetString("userID")
	reviewerID, _ := strconv.ParseInt(reviewerIDStr, 10, 64)
	var req rejectRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.svc.Reject(id, reviewerID, req.Reason); err != nil {
		if err == ErrPluginNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found or not pending"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "rejected", "reason": req.Reason})
}

// extractManifest reads plugin.json and hashes a staged ZIP archive.
func extractManifest(path string) (*manifestJSON, string, error) {
	zipFile, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open zip: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, zipFile); err != nil {
		zipFile.Close()
		return nil, "", fmt.Errorf("hash zip: %w", err)
	}
	if err := zipFile.Close(); err != nil {
		return nil, "", fmt.Errorf("close zip: %w", err)
	}

	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, "", fmt.Errorf("open zip: %w", err)
	}
	defer reader.Close()

	for _, f := range reader.File {
		if f.Name == "plugin.json" || strings.HasSuffix(f.Name, "/plugin.json") {
			if f.UncompressedSize64 > uint64(pluginManifestMaxBytes) {
				return nil, "", fmt.Errorf("plugin.json exceeds 1 MiB limit")
			}
			rc, err := f.Open()
			if err != nil {
				return nil, "", fmt.Errorf("open plugin.json: %w", err)
			}
			defer rc.Close()
			data, err := io.ReadAll(io.LimitReader(rc, pluginManifestMaxBytes+1))
			if err != nil {
				return nil, "", fmt.Errorf("read plugin.json: %w", err)
			}
			if int64(len(data)) > pluginManifestMaxBytes {
				return nil, "", fmt.Errorf("plugin.json exceeds 1 MiB limit")
			}
			var m manifestJSON
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, "", fmt.Errorf("parse plugin.json: %w", err)
			}
			return &m, hex.EncodeToString(hash.Sum(nil)), nil
		}
	}
	return nil, "", fmt.Errorf("plugin.json not found in ZIP")
}

func parsePositiveInt(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return fallback
		}
		n = n*10 + int(ch-'0')
	}
	if n < 1 {
		return fallback
	}
	return n
}

func isSafePluginID(id string) bool {
	if id == "" || strings.Contains(id, "..") {
		return false
	}
	for _, ch := range id {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return false
	}
	return true
}

func isSafePluginVersion(version string) bool {
	if version == "" || strings.Contains(version, "..") {
		return false
	}
	for _, ch := range version {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' || ch == '+' {
			continue
		}
		return false
	}
	return true
}
