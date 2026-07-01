package auth

import (
	"errors"
	"fmt"

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
func (s *Service) Register(phone, password, displayName string) (*database.User, TokenPair, error) {
	user, err := s.createUser(phone, password, displayName, false)
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
func (s *Service) CreateAdmin(phone, password, displayName string) (*database.User, error) {
	return s.createUser(phone, password, displayName, true)
}

func (s *Service) createUser(phone, password, displayName string, isAdmin bool) (*database.User, error) {
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

	user := database.User{
		ID:           s.snowflake.NextID(),
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
	var user database.User
	if err := s.db.Where("phone = ?", phone).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, TokenPair{}, ErrInvalidCredentials
		}
		return nil, TokenPair{}, fmt.Errorf("find user: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, TokenPair{}, ErrInvalidCredentials
	}

	pair, err := s.generateTokenPair(&user)
	if err != nil {
		return nil, TokenPair{}, err
	}
	return &user, pair, nil
}

// Refresh validates a refresh token and issues a new token pair (rotation).
func (s *Service) Refresh(refreshToken string) (*database.User, TokenPair, error) {
	claims, err := s.jwt.Verify(refreshToken)
	if err != nil {
		return nil, TokenPair{}, ErrInvalidRefreshToken
	}
	if claims.TokenType != TokenTypeRefresh {
		return nil, TokenPair{}, ErrInvalidRefreshToken
	}

	user, err := s.GetUserByID(claims.UserID)
	if err != nil {
		return nil, TokenPair{}, ErrInvalidRefreshToken
	}

	pair, err := s.generateTokenPair(user)
	if err != nil {
		return nil, TokenPair{}, err
	}
	return user, pair, nil
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
	access, accessExp, err := s.jwt.GenerateAccessToken(
		fmt.Sprintf("%d", user.ID), user.DisplayName, user.IsAdmin,
	)
	if err != nil {
		return TokenPair{}, fmt.Errorf("generate access token: %w", err)
	}
	refresh, refreshExp, err := s.jwt.GenerateRefreshToken(
		fmt.Sprintf("%d", user.ID), user.DisplayName, user.IsAdmin,
	)
	if err != nil {
		return TokenPair{}, fmt.Errorf("generate refresh token: %w", err)
	}
	return TokenPair{
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
	}, nil
}

// defaultDisplayName generates a display name from the phone number.
// Uses the last 4 digits: "用户1234".
func defaultDisplayName(phone string) string {
	if len(phone) < 4 {
		return "用户" + phone
	}
	return "用户" + phone[len(phone)-4:]
}
