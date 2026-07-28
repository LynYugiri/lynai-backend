package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// IndexObjects handles GET /sync/index/objects.
func (h *Handler) IndexObjects(c *gin.Context) {
	expected, err := strconv.ParseInt(c.Query("expectedIndexRevision"), 10, 64)
	if err != nil || expected < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expectedIndexRevision parameter"})
		return
	}
	after, err := decodeIndexCursor(c.Query("after"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid after parameter"})
		return
	}
	limit := MaxIndexPageSize
	if raw, ok := c.GetQuery("limit"); ok {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > MaxIndexPageSize {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit parameter"})
			return
		}
	}
	page, err := h.svc.ListIndexObjects(getUserID(c), c.Query("category"), after, limit, expected)
	if err != nil {
		writeManagementError(c, err)
		return
	}
	if page.NextAfter != "" {
		page.NextAfter = encodeIndexCursor(page.NextAfter)
	}
	c.JSON(http.StatusOK, page)
}

// IndexObject handles GET /sync/index/objects/:category/:objectId.
func (h *Handler) IndexObject(c *gin.Context) {
	expected, err := strconv.ParseInt(c.Query("expectedIndexRevision"), 10, 64)
	if err != nil || expected < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expectedIndexRevision parameter"})
		return
	}
	detail, err := h.svc.GetIndexObject(getUserID(c), c.Param("category"), c.Param("objectId"), expected)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "indexed object not found"})
		return
	}
	if err != nil {
		writeManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

type purgeRequest struct {
	RequestID             string        `json:"requestId,omitempty"`
	ExpectedGeneration    int64         `json:"expectedGeneration,omitempty"`
	ExpectedIndexRevision int64         `json:"expectedIndexRevision"`
	Selector              PurgeSelector `json:"selector"`
}

// PurgePreview handles POST /sync/manage/purge/preview.
func (h *Handler) PurgePreview(c *gin.Context) {
	var req purgeRequest
	if !decodeStrictJSON(c, &req) {
		return
	}
	preview, err := h.svc.PreviewPurge(getUserID(c), req.Selector, req.ExpectedIndexRevision)
	if err != nil {
		writeManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, preview)
}

// Purge handles signed POST /sync/manage/purge.
func (h *Handler) Purge(c *gin.Context) {
	body, ok := readLimitedBody(c, MaxChangesRequestBody)
	if !ok {
		return
	}
	var req purgeRequest
	if !decodeStrictBytes(c, body, &req) {
		return
	}
	digest := sha256.Sum256(body)
	signed, err := verifySignedRequest(h.svc.db, syncHeaders(c), getUserID(c), c.GetString("sessionID"), c.Request.Method, c.FullPath(), digest[:], h.now(), h.clockSkew)
	if err != nil {
		writeSigningError(c, err)
		return
	}
	if req.RequestID != signed.RequestID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body requestId does not match signed request ID"})
		return
	}
	if req.ExpectedGeneration != signed.ExpectedGeneration {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body expectedGeneration does not match signed header"})
		return
	}
	response, err := h.svc.PurgeIdempotent(getUserID(c), signed.RequestID, signed.BodyHash, c.Request.Method+" "+c.FullPath(), signed.DeviceID, req.Selector, req.ExpectedIndexRevision, signed.ExpectedGeneration)
	if err != nil {
		writeManagementError(c, err)
		return
	}
	c.Data(response.Status, response.ContentType, response.Body)
}

// PendingOperations handles GET /sync/manage/operations.
func (h *Handler) PendingOperations(c *gin.Context) {
	operations, err := h.svc.PendingOperations(getUserID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"operations": operations})
}

type ackOperationRequest struct {
	RequestID          string `json:"requestId"`
	ExpectedGeneration int64  `json:"expectedGeneration"`
	OperationID        string `json:"operationId,omitempty"`
}

// AckOperation handles signed POST /sync/manage/operations/:id/ack.
func (h *Handler) AckOperation(c *gin.Context) {
	body, ok := readLimitedBody(c, 16<<10)
	if !ok {
		return
	}
	var req ackOperationRequest
	if !decodeStrictBytes(c, body, &req) {
		return
	}
	digest := sha256.Sum256(body)
	signed, err := verifySignedRequest(h.svc.db, syncHeaders(c), getUserID(c), c.GetString("sessionID"), c.Request.Method, c.FullPath(), digest[:], h.now(), h.clockSkew)
	if err != nil {
		writeSigningError(c, err)
		return
	}
	if req.RequestID != signed.RequestID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body requestId does not match signed request ID"})
		return
	}
	if req.ExpectedGeneration != signed.ExpectedGeneration {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body expectedGeneration does not match signed header"})
		return
	}
	if req.OperationID != "" && req.OperationID != c.Param("id") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body operationId does not match path operation ID"})
		return
	}
	response, err := h.svc.AckOperationIdempotent(getUserID(c), signed.RequestID, signed.BodyHash, c.Request.Method+" "+c.FullPath(), signed.DeviceID, c.Param("id"), signed.ExpectedGeneration)
	if err != nil {
		writeManagementError(c, err)
		return
	}
	c.Data(response.Status, response.ContentType, response.Body)
}

func readLimitedBody(c *gin.Context, limit int64) ([]byte, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body is too large"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "request body is unreadable"})
		}
		return nil, false
	}
	return body, true
}

func decodeStrictJSON(c *gin.Context, value interface{}) bool {
	body, ok := readLimitedBody(c, MaxChangesRequestBody)
	if !ok {
		return false
	}
	return decodeStrictBytes(c, body, value)
}

func decodeStrictBytes(c *gin.Context, body []byte, value interface{}) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	return true
}

func writeManagementError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrInvalidSelector):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, ErrGenerationConflict):
		writeGenerationConflict(c, err)
	case errors.Is(err, ErrIndexRevisionConflict):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "index_revision_conflict"})
	case errors.Is(err, ErrReplayConflict):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "replay_conflict"})
	case errors.Is(err, ErrOperationNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

func writeGenerationConflict(c *gin.Context, err error) {
	response := gin.H{"error": err.Error(), "code": "generation_mismatch"}
	var conflict generationConflictError
	if errors.As(err, &conflict) {
		response["expectedGeneration"] = conflict.Expected
		response["currentGeneration"] = conflict.Current
	}
	c.JSON(http.StatusConflict, response)
}
