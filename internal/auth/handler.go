package auth

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
)

// Handler exposes the /auth/* API endpoints.
type Handler struct {
	svc *Service
}

// NewHandler creates an auth handler bound to the given service.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// userResponse serializes a User in the camelCase format the Flutter client expects.
// ID is serialized as a string to prevent precision loss on the client side.
type userResponse struct {
	ID          string  `json:"id"`
	Phone       string  `json:"phone"`
	DisplayName string  `json:"displayName"`
	AvatarURL   *string `json:"avatarUrl"`
	Email       *string `json:"email"`
	IsAdmin     bool    `json:"isAdmin"`
}

type tokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

type sessionResponse struct {
	User  userResponse  `json:"user"`
	Token tokenResponse `json:"token"`
}

func toUserResponse(u *database.User) userResponse {
	return userResponse{
		ID:          strconv.FormatInt(u.ID, 10),
		Phone:       u.Phone,
		DisplayName: u.DisplayName,
		AvatarURL:   u.AvatarURL,
		Email:       u.Email,
		IsAdmin:     u.IsAdmin,
	}
}

func toTokenResponse(pair TokenPair) tokenResponse {
	return tokenResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		ExpiresAt:    pair.AccessExpiresAt,
	}
}

type registerRequest struct {
	Phone       string `json:"phone" binding:"required"`
	Password    string `json:"password" binding:"required,min=6"`
	DisplayName string `json:"displayName" binding:"omitempty,max=32"`
}

// Register handles POST /auth/register.
func (h *Handler) Register(c *gin.Context) {
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, pair, err := h.svc.Register(req.Phone, req.Password, req.DisplayName)
	if err != nil {
		if err == ErrPhoneTaken {
			c.JSON(http.StatusConflict, gin.H{"error": "phone already registered"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, sessionResponse{
		User:  toUserResponse(user),
		Token: toTokenResponse(pair),
	})
}

type loginRequest struct {
	Phone    string `json:"phone" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// Login handles POST /auth/login.
func (h *Handler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, pair, err := h.svc.Login(req.Phone, req.Password)
	if err != nil {
		if err == ErrInvalidCredentials {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid phone or password"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, sessionResponse{
		User:  toUserResponse(user),
		Token: toTokenResponse(pair),
	})
}

type sendOTPRequest struct {
	Phone string `json:"phone" binding:"required"`
}

// SendOTP handles POST /auth/send-otp.
// SMS verification is reserved but disabled until an SMS provider ships.
func (h *Handler) SendOTP(c *gin.Context) {
	var req sendOTPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.SendOTP(req.Phone); err != nil {
		if err == ErrOTPNotSupported {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "短信验证暂未启用"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send OTP"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sent": true})
}

type verifyOTPRequest struct {
	Phone       string `json:"phone" binding:"required"`
	Code        string `json:"code" binding:"required"`
	DisplayName string `json:"displayName" binding:"omitempty,max=32"`
}

// VerifyOTP handles POST /auth/verify-otp.
// SMS verification is reserved but disabled until an SMS provider ships.
func (h *Handler) VerifyOTP(c *gin.Context) {
	var req verifyOTPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, pair, err := h.svc.VerifyOTP(req.Phone, req.Code, req.DisplayName)
	if err != nil {
		if err == ErrOTPNotSupported {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "短信验证暂未启用"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, sessionResponse{
		User:  toUserResponse(user),
		Token: toTokenResponse(pair),
	})
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken" binding:"required"`
}

// Refresh handles POST /auth/refresh.
func (h *Handler) Refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, pair, err := h.svc.Refresh(req.RefreshToken)
	if err != nil {
		if err == ErrInvalidRefreshToken {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired refresh token"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, sessionResponse{
		User:  toUserResponse(user),
		Token: toTokenResponse(pair),
	})
}

// Logout handles POST /auth/logout.
func (h *Handler) Logout(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

// Me handles GET /auth/me. Requires authentication.
func (h *Handler) Me(c *gin.Context) {
	userID := c.GetString("userID")
	user, err := h.svc.GetUserByID(userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": toUserResponse(user)})
}

// extractToken pulls the bearer token from the Authorization header.
func extractToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(auth, "Bearer ")
}
