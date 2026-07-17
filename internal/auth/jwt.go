package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// AccessTokenExpiry is the lifetime of an access token.
	// Short-lived to limit the blast radius of a stolen token.
	AccessTokenExpiry = 15 * time.Minute

	// RefreshTokenExpiry is the lifetime of a refresh token.
	// Long-lived so active users never need to re-login; only users
	// inactive for 30 days are forced to authenticate again.
	RefreshTokenExpiry = 30 * 24 * time.Hour

	TokenTypeAccess  = "access"
	TokenTypeRefresh = "refresh"
)

// Claims is the JWT payload embedded in every token.
type Claims struct {
	UserID    string `json:"uid"`
	Username  string `json:"usr"`
	IsAdmin   bool   `json:"adm"`
	TokenType string `json:"typ"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

// JWTManager signs and verifies JWT tokens.
type JWTManager struct {
	secret []byte
}

// NewJWTManager creates a manager with the given HMAC-SHA256 secret.
func NewJWTManager(secret string) *JWTManager {
	return &JWTManager{secret: []byte(secret)}
}

// GenerateAccessToken produces a short-lived access JWT.
func (m *JWTManager) GenerateAccessToken(userID, username string, isAdmin bool) (string, int64, error) {
	return m.generate(userID, username, isAdmin, "", TokenTypeAccess, AccessTokenExpiry)
}

// GenerateRefreshToken produces a long-lived refresh JWT.
func (m *JWTManager) GenerateRefreshToken(userID, username string, isAdmin bool) (string, int64, error) {
	return m.generate(userID, username, isAdmin, "", TokenTypeRefresh, RefreshTokenExpiry)
}

func (m *JWTManager) generate(userID, username string, isAdmin bool, sessionID, tokenType string, expiry time.Duration) (string, int64, error) {
	tokenID, err := randomID()
	if err != nil {
		return "", 0, err
	}
	expiresAt := time.Now().Add(expiry)
	claims := Claims{
		UserID:    userID,
		Username:  username,
		IsAdmin:   isAdmin,
		TokenType: tokenType,
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        tokenID,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(m.secret)
	if err != nil {
		return "", 0, err
	}
	return signed, expiresAt.UnixMilli(), nil
}

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Verify parses and validates a JWT string, returning its claims.
// Works for both access and refresh tokens.
func (m *JWTManager) Verify(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("unexpected signing method")
		}
		return m.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}
