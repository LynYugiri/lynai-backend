package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Handler exposes the /sync/* API endpoints.
type Handler struct {
	svc       *Service
	clockSkew time.Duration
	now       func() time.Time
}

// NewHandler creates a sync handler.
func NewHandler(svc *Service) *Handler {
	return NewHandlerWithClockSkew(svc, 5*time.Minute)
}

// NewHandlerWithClockSkew creates a handler with the allowed signature timestamp skew.
func NewHandlerWithClockSkew(svc *Service, clockSkew time.Duration) *Handler {
	return &Handler{svc: svc, clockSkew: clockSkew, now: time.Now}
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
	RequestID          string          `json:"requestId,omitempty"`
	ExpectedGeneration int64           `json:"expectedGeneration"`
	Changes            *[]ChangeRecord `json:"changes"`
}

// UploadChanges handles POST /sync/changes.
func (h *Handler) UploadChanges(c *gin.Context) {
	userID := getUserID(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxChangesRequestBody)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body is too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "request body is unreadable"})
		return
	}
	var req uploadChangesRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Changes == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "changes field is required"})
		return
	}

	digest := sha256.Sum256(body)
	signed, err := verifySignedRequest(h.svc.db, syncHeaders(c), userID, c.GetString("sessionID"), c.Request.Method, c.FullPath(), digest[:], h.now(), h.clockSkew)
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
	response, err := h.svc.UploadChangesIdempotent(userID, signed.RequestID, signed.BodyHash, c.Request.Method+" "+c.FullPath(), signed.DeviceID, signed.ExpectedGeneration, *req.Changes)
	if err != nil {
		writeUploadError(c, err)
		return
	}
	c.Data(response.Status, response.ContentType, response.Body)
}

func writeSigningError(c *gin.Context, err error) {
	code := "invalid_signed_request"
	switch {
	case errors.Is(err, ErrSignatureRequired):
		code = "signature_required"
	case errors.Is(err, ErrUnknownDevice):
		code = "unknown_device"
	case errors.Is(err, ErrRevokedDevice):
		code = "revoked_device"
	case errors.Is(err, ErrDeviceSessionMismatch):
		code = "device_session_mismatch"
	}
	c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error(), "code": code})
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

func syncHeaders(c *gin.Context) map[string]string {
	return map[string]string{
		"protocol": c.GetHeader("X-LynAI-Protocol"), "deviceID": c.GetHeader("X-LynAI-Device-ID"),
		"timestamp": c.GetHeader("X-LynAI-Timestamp"), "requestID": c.GetHeader("X-LynAI-Request-ID"),
		"bodyHash": c.GetHeader("X-LynAI-Body-SHA256"), "signature": c.GetHeader("X-LynAI-Signature"),
		"expectedGeneration": c.GetHeader("X-LynAI-Expected-Generation"),
	}
}

func writeUploadError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrSyncLimit):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
	case errors.Is(err, ErrInvalidChange):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, ErrGenerationConflict):
		writeGenerationConflict(c, err)
	case errors.Is(err, ErrChangeConflict), errors.Is(err, ErrReplayConflict):
		code := "change_conflict"
		if errors.Is(err, ErrReplayConflict) {
			code = "replay_conflict"
		}
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": code})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

// GetChanges handles GET /sync/changes?since={seq}.
func (h *Handler) GetChanges(c *gin.Context) {
	userID := getUserID(c)
	expectedGeneration, ok := parseExpectedGeneration(c)
	if !ok {
		return
	}
	sinceStr := c.DefaultQuery("since", "0")
	since, err := strconv.ParseInt(sinceStr, 10, 64)
	if err != nil || since < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since parameter"})
		return
	}
	limit := MaxChangesPageSize
	if limitStr, ok := c.GetQuery("limit"); ok {
		value, err := strconv.Atoi(limitStr)
		if err != nil || value < 1 || value > MaxChangesPageSize {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit parameter"})
			return
		}
		limit = value
	}

	page, err := h.svc.GetChangesPage(userID, since, limit)
	if expectedGeneration > 0 && page.Generation > 0 && page.Generation != expectedGeneration {
		writeGenerationConflict(c, generationConflictError{Expected: expectedGeneration, Current: page.Generation})
		return
	}
	if err != nil {
		if errors.Is(err, ErrFutureCursor) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "future_cursor", "generation": page.Generation, "currentGeneration": page.Generation, "latestSeq": page.GlobalLatestSeq, "indexRevision": page.IndexRevision, "minAvailableSeq": page.MinAvailableSeq})
			return
		}
		if errors.Is(err, ErrStaleCursor) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "stale_cursor", "generation": page.Generation, "currentGeneration": page.Generation, "latestSeq": page.GlobalLatestSeq, "indexRevision": page.IndexRevision, "minAvailableSeq": page.MinAvailableSeq})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	nextSince := since
	if len(page.Changes) > 0 {
		nextSince = page.Changes[len(page.Changes)-1].Seq
	}
	response := gin.H{
		"changes":         page.Changes,
		"latestSeq":       nextSince,
		"globalLatestSeq": page.GlobalLatestSeq,
		"generation":      page.Generation,
		"indexRevision":   page.IndexRevision,
		"minAvailableSeq": page.MinAvailableSeq,
	}
	response["hasMore"] = page.HasMore
	response["nextSince"] = nextSince
	c.JSON(http.StatusOK, response)
}

// ListBlobs handles GET /sync/blobs.
func (h *Handler) ListBlobs(c *gin.Context) {
	userID := getUserID(c)
	if !h.requireExpectedGeneration(c, userID) {
		return
	}
	after, err := strconv.ParseUint(c.DefaultQuery("after", "0"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid after parameter"})
		return
	}
	limit := MaxBlobsPageSize
	if limitStr, ok := c.GetQuery("limit"); ok {
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit < 1 || limit > MaxBlobsPageSize {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit parameter"})
			return
		}
	}
	blobs, nextAfter, hasMore, err := h.svc.ListBlobs(userID, after, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"blobs": blobs, "nextAfter": nextAfter, "hasMore": hasMore,
		"truncated": hasMore, "returnedCount": len(blobs), "pageSize": limit,
	})
}

// UploadBlob handles POST /sync/blobs/:sha256.
// Body is streamed with a 64 MiB limit.
func (h *Handler) UploadBlob(c *gin.Context) {
	userID := getUserID(c)
	sha256 := c.Param("sha256")
	if !sha256Pattern.MatchString(sha256) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sha256 parameter"})
		return
	}

	digest, _ := hex.DecodeString(sha256)
	signed, err := verifySignedRequest(h.svc.db, syncHeaders(c), userID, c.GetString("sessionID"), c.Request.Method, c.FullPath(), digest, h.now(), h.clockSkew)
	if err != nil {
		writeSigningError(c, err)
		return
	}
	operation := c.Request.Method + " " + c.FullPath()
	if replay, found, err := h.svc.CheckReplay(userID, signed.RequestID, signed.BodyHash, operation, signed.ExpectedGeneration); err != nil {
		if errors.Is(err, ErrGenerationConflict) {
			writeGenerationConflict(c, err)
			return
		}
		if errors.Is(err, ErrReplayConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "replay_conflict"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save blob"})
		return
	} else if found {
		c.Data(replay.Status, replay.ContentType, replay.Body)
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBlobBytes)
	prepared, err := h.svc.PrepareBlob(userID, sha256, c.Request.Body)
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "blob is too large"})
			return
		}
		if errors.Is(err, ErrBlobHashMismatch) {
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrBlobHashMismatch.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save blob"})
		return
	}
	defer prepared.Close()
	response, err := h.svc.SavePreparedBlobIdempotent(userID, prepared, signed.RequestID, signed.BodyHash, operation, signed.ExpectedGeneration)
	if err != nil {
		if errors.Is(err, ErrGenerationConflict) {
			writeGenerationConflict(c, err)
			return
		}
		if errors.Is(err, ErrReplayConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "replay_conflict"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save blob"})
		return
	}
	c.Data(response.Status, response.ContentType, response.Body)
}

// DownloadBlob handles GET /sync/blobs/:sha256.
func (h *Handler) DownloadBlob(c *gin.Context) {
	userID := getUserID(c)
	if !h.requireExpectedGeneration(c, userID) {
		return
	}
	sha256 := c.Param("sha256")
	if !sha256Pattern.MatchString(sha256) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sha256 parameter"})
		return
	}

	data, err := h.svc.LoadBlob(userID, sha256)
	if err != nil {
		if errors.Is(err, ErrBlobNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "blob not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load blob"})
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", data)
}

func parseExpectedGeneration(c *gin.Context) (int64, bool) {
	raw := c.GetHeader("X-LynAI-Expected-Generation")
	if raw == "" {
		return 0, true
	}
	generation, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || generation < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expected generation"})
		return 0, false
	}
	return generation, true
}

func (h *Handler) requireExpectedGeneration(c *gin.Context, userID int64) bool {
	expected, ok := parseExpectedGeneration(c)
	if !ok {
		return false
	}
	if expected == 0 {
		return true
	}
	status, err := h.svc.GetStatus(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return false
	}
	if status.Generation != expected {
		writeGenerationConflict(c, generationConflictError{Expected: expected, Current: status.Generation})
		return false
	}
	return true
}

// getUserID extracts the user ID from the auth context as int64.
func getUserID(c *gin.Context) int64 {
	idStr := c.GetString("userID")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	return id
}
