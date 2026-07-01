package market

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
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
	pageSize := parsePositiveInt(c.Query("page_size"), 20)

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

	file, err := c.FormFile("zip")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "zip file is required"})
		return
	}

	if !strings.HasSuffix(file.Filename, ".zip") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file must be a .zip"})
		return
	}

	// Read the uploaded file into memory. Plugin ZIPs are small (<10 MB typical).
	src, err := file.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot read uploaded file"})
		return
	}
	defer src.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, src); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot read uploaded file"})
		return
	}
	zipBytes := buf.Bytes()

	manifest, err := extractManifest(zipBytes)
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

	sha := sha256Hex(zipBytes)
	relPath, err := h.svc.storage.SavePluginZip(manifest.ID, manifest.Version, zipBytes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store plugin file"})
		return
	}

	plugin, err := h.svc.UpsertSubmission(manifest, relPath, &sha, userID)
	if err != nil {
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
	_ = c.ShouldBindJSON(&req)

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

// extractManifest reads plugin.json from within a ZIP archive.
func extractManifest(zipBytes []byte) (*manifestJSON, error) {
	reader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}

	for _, f := range reader.File {
		if f.Name == "plugin.json" || strings.HasSuffix(f.Name, "/plugin.json") {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("open plugin.json: %w", err)
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return nil, fmt.Errorf("read plugin.json: %w", err)
			}
			var m manifestJSON
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("parse plugin.json: %w", err)
			}
			return &m, nil
		}
	}
	return nil, fmt.Errorf("plugin.json not found in ZIP")
}

// sha256Hex returns the hex-encoded SHA-256 digest of the given bytes.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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
