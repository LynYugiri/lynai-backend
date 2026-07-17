package device

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

type deviceResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Platform  string     `json:"platform"`
	Protocol  uint16     `json:"protocolVersion"`
	RevokedAt *time.Time `json:"revokedAt"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Current   bool       `json:"current"`
}

func (h *Handler) Challenge(c *gin.Context) {
	userID, ok := authenticatedUser(c)
	if !ok {
		return
	}
	var req proposalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	challenge, err := h.svc.IssueChallenge(userID, c.GetString("sessionID"), req.proposal())
	if err != nil {
		if isInvalidEnrollment(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue challenge"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"challengeId": challenge.ID, "challenge": challenge.Value,
		"userId": challenge.UserID, "sessionId": challenge.SessionID,
		"expiresAt": challenge.ExpiresAt,
	})
}

type proposalRequest struct {
	DeviceID        string `json:"deviceId" binding:"required,len=52"`
	PublicKey       string `json:"publicKey" binding:"required,len=43"`
	Name            string `json:"displayName" binding:"required"`
	Platform        string `json:"platform" binding:"required,max=32"`
	ProtocolVersion uint16 `json:"protocolVersion" binding:"required"`
}

func (r proposalRequest) proposal() Proposal {
	return Proposal(r)
}

type enrollRequest struct {
	proposalRequest
	ChallengeID string `json:"challengeId" binding:"required,len=32"`
	Challenge   string `json:"challenge" binding:"required,len=43"`
	Signature   string `json:"signature" binding:"required,len=86"`
}

func (h *Handler) Enroll(c *gin.Context) {
	userID, ok := authenticatedUser(c)
	if !ok {
		return
	}
	var req enrollRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := h.svc.Enroll(userID, c.GetString("sessionID"), Enrollment{
		ChallengeID: req.ChallengeID, Challenge: req.Challenge,
		DeviceID: req.DeviceID, PublicKey: req.PublicKey, Signature: req.Signature,
		Name: req.Name, Platform: req.Platform, ProtocolVersion: req.ProtocolVersion,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidChallenge):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, ErrDeviceConflict), errors.Is(err, ErrDeviceOwned), errors.Is(err, ErrDeviceRevoked):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case isInvalidEnrollment(err):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enroll device"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"device": toResponse(result, c.GetString("sessionID"))})
}

func isInvalidEnrollment(err error) bool {
	return errors.Is(err, ErrInvalidKey) || errors.Is(err, ErrInvalidSignature) ||
		errors.Is(err, ErrInvalidDeviceID) || errors.Is(err, ErrInvalidName) ||
		errors.Is(err, ErrInvalidPlatform) || errors.Is(err, ErrInvalidProtocol)
}

func (h *Handler) List(c *gin.Context) {
	userID, ok := authenticatedUser(c)
	if !ok {
		return
	}
	devices, err := h.svc.List(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list devices"})
		return
	}
	response := make([]deviceResponse, 0, len(devices))
	for i := range devices {
		response = append(response, toResponse(&devices[i], c.GetString("sessionID")))
	}
	c.JSON(http.StatusOK, gin.H{"devices": response})
}

func (h *Handler) Current(c *gin.Context) {
	userID, ok := authenticatedUser(c)
	if !ok {
		return
	}
	result, err := h.svc.Current(userID, c.GetString("sessionID"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "current device not enrolled"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get current device"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"device": toResponse(result, c.GetString("sessionID"))})
}

type renameRequest struct {
	Name string `json:"name" binding:"required,max=64"`
}

func (h *Handler) Rename(c *gin.Context) {
	userID, ok := authenticatedUser(c)
	if !ok {
		return
	}
	var req renameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := h.svc.Rename(userID, c.Param("id"), req.Name)
	if errors.Is(err, ErrInvalidName) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rename device"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"device": toResponse(result, c.GetString("sessionID"))})
}

func (h *Handler) Revoke(c *gin.Context) {
	userID, ok := authenticatedUser(c)
	if !ok {
		return
	}
	if err := h.svc.Revoke(userID, c.Param("id")); errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke device"})
		return
	}
	c.Status(http.StatusNoContent)
}

func authenticatedUser(c *gin.Context) (int64, bool) {
	userID, err := strconv.ParseInt(c.GetString("userID"), 10, 64)
	if err != nil || c.GetString("sessionID") == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated session required"})
		return 0, false
	}
	return userID, true
}

func toResponse(value *database.UserDevice, sessionID string) deviceResponse {
	return deviceResponse{
		ID:        value.DeviceID,
		Name:      value.Name,
		Platform:  value.Platform,
		Protocol:  value.Protocol,
		RevokedAt: value.RevokedAt,
		CreatedAt: value.CreatedAt,
		UpdatedAt: value.UpdatedAt,
		Current:   value.SessionID == sessionID,
	}
}
