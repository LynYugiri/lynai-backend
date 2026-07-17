package auth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/lynai/backend/internal/database"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// ErrPhoneTaken is returned when a registration uses an existing phone number.
var ErrPhoneTaken = errors.New("phone number already registered")

// ErrInvalidRefreshToken is returned when a refresh token is invalid or expired.
var ErrInvalidRefreshToken = errors.New("invalid or expired refresh token")

// ErrInvalidCredentials is returned when login credentials are invalid.
var ErrInvalidCredentials = errors.New("invalid phone or password")

// ErrOTPNotSupported is returned while phone verification is reserved but disabled.
var ErrOTPNotSupported = errors.New("otp verification is not enabled")

// TokenPair holds both access and refresh tokens with their expiry times.
type TokenPair struct {
	AccessToken      string
	AccessExpiresAt  int64
	RefreshToken     string
	RefreshExpiresAt int64
}

// Service handles user registration, login, and session logic.
type Service struct {
	db        *gorm.DB
	jwt       *JWTManager
	snowflake *database.SnowflakeGenerator
}

// NewService creates an auth service with the given database, JWT manager,
// and snowflake ID generator.
func NewService(db *gorm.DB, jwt *JWTManager, snowflake *database.SnowflakeGenerator) *Service {
	return &Service{db: db, jwt: jwt, snowflake: snowflake}
}

// Register creates a new user with the given phone number and password, then returns a
// session with both tokens. If the phone is already registered, returns
// ErrPhoneTaken — the caller should use Login instead.
func (s *Service) Register(ctx context.Context, phone, password, displayName string) (*database.User, TokenPair, error) {
	user, err := s.createUser(ctx, phone, password, displayName, false)
	if err != nil {
		return nil, TokenPair{}, err
	}

	pair, err := s.generateTokenPair(user)
	if err != nil {
		return nil, TokenPair{}, err
	}
	return user, pair, nil
}

// CreateAdmin creates a new administrator account.
func (s *Service) CreateAdmin(ctx context.Context, phone, password, displayName string) (*database.User, error) {
	return s.createUser(ctx, phone, password, displayName, true)
}

func (s *Service) createUser(ctx context.Context, phone, password, displayName string, isAdmin bool) (*database.User, error) {
	var existing int64
	s.db.Model(&database.User{}).Where("phone = ?", phone).Count(&existing)
	if existing > 0 {
		return nil, ErrPhoneTaken
	}

	passwordHash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	name := displayName
	if name == "" {
		name = defaultDisplayName(phone)
	}

	id, err := s.snowflake.NextID(ctx)
	if err != nil {
		return nil, err
	}
	user := database.User{
		ID:           id,
		Phone:        phone,
		PasswordHash: passwordHash,
		DisplayName:  name,
		IsAdmin:      isAdmin,
	}
	if err := s.db.Create(&user).Error; err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return &user, nil
}

// ListUsers returns users in reverse creation order.
func (s *Service) ListUsers(page, pageSize int) ([]database.User, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	var total int64
	if err := s.db.Model(&database.User{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var users []database.User
	if err := s.db.Order("created_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&users).Error; err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// SetAdminRole sets or clears a user's administrator role.
func (s *Service) SetAdminRole(userID string, isAdmin bool) error {
	result := s.db.Model(&database.User{}).Where("id = ?", userID).Update("is_admin", isAdmin)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// Login verifies a phone/password pair and returns a session with both tokens.
func (s *Service) Login(phone, password string) (*database.User, TokenPair, error) {
	user, err := s.AuthenticatePassword(phone, password)
	if err != nil {
		return nil, TokenPair{}, err
	}
	pair, err := s.generateTokenPair(user)
	if err != nil {
		return nil, TokenPair{}, err
	}
	return user, pair, nil
}

// AuthenticatePassword verifies credentials without creating an App session.
func (s *Service) AuthenticatePassword(phone, password string) (*database.User, error) {
	var user database.User
	if err := s.db.Where("phone = ?", phone).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("find user: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return &user, nil
}

// Refresh validates a refresh token and issues a new token pair (rotation).
func (s *Service) Refresh(refreshToken string) (*database.User, TokenPair, error) {
	claims, err := s.jwt.Verify(refreshToken)
	if err != nil {
		return nil, TokenPair{}, ErrInvalidRefreshToken
	}
	if claims.TokenType != TokenTypeRefresh || claims.SessionID == "" || claims.ID == "" {
		return nil, TokenPair{}, ErrInvalidRefreshToken
	}

	user, err := s.GetUserByID(claims.UserID)
	if err != nil {
		return nil, TokenPair{}, ErrInvalidRefreshToken
	}

	pair, newRefreshJTI, err := s.generateTokenPairForSession(user, claims.SessionID)
	if err != nil {
		return nil, TokenPair{}, err
	}

	now := time.Now()
	result := s.db.Model(&database.UserSession{}).
		Where("id = ? AND user_id = ? AND refresh_jti = ? AND revoked_at IS NULL AND expires_at > ?", claims.SessionID, user.ID, claims.ID, now).
		Updates(map[string]interface{}{"refresh_jti": newRefreshJTI, "expires_at": time.UnixMilli(pair.RefreshExpiresAt)})
	if result.Error != nil {
		return nil, TokenPair{}, fmt.Errorf("rotate refresh token: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, TokenPair{}, ErrInvalidRefreshToken
	}
	return user, pair, nil
}

// AuthenticateAccess validates an access token against its live session and
// returns the current user record, including the current administrator role.
func (s *Service) AuthenticateAccess(token string) (*database.User, *Claims, error) {
	claims, err := s.jwt.Verify(token)
	if err != nil || claims.TokenType != TokenTypeAccess || claims.SessionID == "" || claims.ID == "" {
		return nil, nil, errors.New("invalid access token")
	}
	user, err := s.validateSession(claims, false)
	if err != nil {
		return nil, nil, err
	}
	return user, claims, nil
}

// AuthenticateRefresh validates that a refresh token is the current token for
// a live session and returns the user's current database role.
func (s *Service) AuthenticateRefresh(token string) (*database.User, *Claims, error) {
	claims, err := s.jwt.Verify(token)
	if err != nil || claims.TokenType != TokenTypeRefresh || claims.SessionID == "" || claims.ID == "" {
		return nil, nil, ErrInvalidRefreshToken
	}
	user, err := s.validateSession(claims, true)
	if err != nil {
		return nil, nil, ErrInvalidRefreshToken
	}
	return user, claims, nil
}

func (s *Service) validateSession(claims *Claims, checkRefreshJTI bool) (*database.User, error) {
	userID, err := strconv.ParseInt(claims.UserID, 10, 64)
	if err != nil {
		return nil, err
	}
	query := s.db.Where("id = ? AND user_id = ? AND revoked_at IS NULL AND expires_at > ?", claims.SessionID, userID, time.Now())
	if checkRefreshJTI {
		query = query.Where("refresh_jti = ?", claims.ID)
	}
	var session database.UserSession
	if err := query.First(&session).Error; err != nil {
		return nil, err
	}
	return s.GetUserByID(claims.UserID)
}

// RevokeSession invalidates all access and refresh tokens in one login session.
func (s *Service) RevokeSession(sessionID, userID string) error {
	if sessionID == "" || userID == "" {
		return nil
	}
	return s.db.Model(&database.UserSession{}).
		Where("id = ? AND user_id = ? AND revoked_at IS NULL", sessionID, userID).
		Update("revoked_at", time.Now()).Error
}

// RevokeToken invalidates the session identified by a signed session token.
func (s *Service) RevokeToken(token string) error {
	claims, err := s.jwt.Verify(token)
	if err != nil || claims.SessionID == "" {
		return nil
	}
	return s.RevokeSession(claims.SessionID, claims.UserID)
}

// RevokeRefreshToken idempotently revokes the session family named by a signed
// refresh token. Rotation changes the current JTI, not the session identity.
// Invalid, expired, and already-revoked tokens are successful no-ops.
func (s *Service) RevokeRefreshToken(token string) error {
	claims, err := s.jwt.Verify(token)
	if err != nil || claims.TokenType != TokenTypeRefresh || claims.SessionID == "" || claims.UserID == "" || claims.ID == "" {
		return nil
	}
	return s.RevokeSession(claims.SessionID, claims.UserID)
}

// DeleteExpiredSessions removes expired or previously revoked login sessions.
func (s *Service) DeleteExpiredSessions(now time.Time) error {
	return s.db.Where("expires_at <= ? OR revoked_at IS NOT NULL", now).Delete(&database.UserSession{}).Error
}

// GetUserByID fetches a user by primary key.
func (s *Service) GetUserByID(id string) (*database.User, error) {
	var user database.User
	if err := s.db.First(&user, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// SendOTP is reserved for future SMS integration and is disabled for now.
func (s *Service) SendOTP(phone string) error {
	_ = phone
	return ErrOTPNotSupported
}

// VerifyOTP is reserved for future SMS integration and is disabled for now.
func (s *Service) VerifyOTP(phone, code, displayName string) (*database.User, TokenPair, error) {
	_, _, _ = phone, code, displayName
	return nil, TokenPair{}, ErrOTPNotSupported
}

// HashPassword returns a bcrypt hash suitable for storage.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func (s *Service) generateTokenPair(user *database.User) (TokenPair, error) {
	sessionID, err := randomID()
	if err != nil {
		return TokenPair{}, fmt.Errorf("generate session ID: %w", err)
	}
	pair, refreshJTI, err := s.generateTokenPairForSession(user, sessionID)
	if err != nil {
		return TokenPair{}, err
	}
	session := database.UserSession{
		ID:         sessionID,
		UserID:     user.ID,
		RefreshJTI: refreshJTI,
		ExpiresAt:  time.UnixMilli(pair.RefreshExpiresAt),
	}
	if err := s.db.Create(&session).Error; err != nil {
		return TokenPair{}, fmt.Errorf("create session: %w", err)
	}
	return pair, nil
}

func (s *Service) generateTokenPairForSession(user *database.User, sessionID string) (TokenPair, string, error) {
	userID := strconv.FormatInt(user.ID, 10)
	access, accessExp, err := s.jwt.generate(userID, user.DisplayName, user.IsAdmin, sessionID, TokenTypeAccess, AccessTokenExpiry)
	if err != nil {
		return TokenPair{}, "", fmt.Errorf("generate access token: %w", err)
	}
	refresh, refreshExp, err := s.jwt.generate(userID, user.DisplayName, user.IsAdmin, sessionID, TokenTypeRefresh, RefreshTokenExpiry)
	if err != nil {
		return TokenPair{}, "", fmt.Errorf("generate refresh token: %w", err)
	}
	claims, err := s.jwt.Verify(refresh)
	if err != nil {
		return TokenPair{}, "", fmt.Errorf("read refresh token: %w", err)
	}
	return TokenPair{
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
	}, claims.ID, nil
}

// defaultDisplayName generates a display name from the phone number.
// Uses the last 4 digits: "用户1234".
func defaultDisplayName(phone string) string {
	if len(phone) < 4 {
		return "用户" + phone
	}
	return "用户" + phone[len(phone)-4:]
}
