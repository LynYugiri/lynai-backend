package sync

import (
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// Handler exposes the /sync/* API endpoints.
type Handler struct {
	svc *Service
}

// NewHandler creates a sync handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Status handles GET /sync/status.
func (h *Handler) Status(c *gin.Context) {
	userID := getUserID(c)
	status, err := h.svc.GetStatus(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, status)
}

type uploadChangesRequest struct {
	Changes []ChangeRecord `json:"changes" binding:"required"`
}

// UploadChanges handles POST /sync/changes.
func (h *Handler) UploadChanges(c *gin.Context) {
	userID := getUserID(c)
	var req uploadChangesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	result, err := h.svc.UploadChanges(userID, req.Changes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	latestSeq := int64(0)
	if len(result) > 0 {
		latestSeq = result[len(result)-1].Seq
	}
	c.JSON(http.StatusOK, gin.H{
		"changes":   result,
		"latestSeq": latestSeq,
	})
}

// GetChanges handles GET /sync/changes?since={seq}.
func (h *Handler) GetChanges(c *gin.Context) {
	userID := getUserID(c)
	sinceStr := c.DefaultQuery("since", "0")
	since, err := strconv.ParseInt(sinceStr, 10, 64)
	if err != nil || since < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since parameter"})
		return
	}

	changes, err := h.svc.GetChanges(userID, since)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	latestSeq := h.svc.GetLatestSeq(userID)
	c.JSON(http.StatusOK, gin.H{
		"changes":   changes,
		"latestSeq": latestSeq,
	})
}

// ListBlobs handles GET /sync/blobs.
func (h *Handler) ListBlobs(c *gin.Context) {
	userID := getUserID(c)
	blobs, err := h.svc.ListBlobs(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"blobs": blobs})
}

// UploadBlob handles POST /sync/blobs/:sha256.
// Body is raw bytes (<1MB enforced by middleware or client).
func (h *Handler) UploadBlob(c *gin.Context) {
	userID := getUserID(c)
	sha256 := c.Param("sha256")

	// Limit to 1MB.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "blob too large or unreadable"})
		return
	}

	if err := h.svc.SaveBlob(userID, sha256, data); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save blob"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sha256": sha256, "size": len(data)})
}

// DownloadBlob handles GET /sync/blobs/:sha256.
func (h *Handler) DownloadBlob(c *gin.Context) {
	userID := getUserID(c)
	sha256 := c.Param("sha256")

	data, err := h.svc.LoadBlob(userID, sha256)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "blob not found"})
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", data)
}

// getUserID extracts the user ID from the auth context as int64.
func getUserID(c *gin.Context) int64 {
	idStr := c.GetString("userID")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	return id
}
