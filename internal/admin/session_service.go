package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
)

var errInvalidAdminSession = errors.New("invalid admin session")

type sessionService struct {
	db         *gorm.DB
	ttl        time.Duration
	renewAfter time.Duration
}

func newSessionService(db *gorm.DB, ttl time.Duration) *sessionService {
	return &sessionService{db: db, ttl: ttl, renewAfter: ttl / 4}
}

func (s *sessionService) create(userID int64) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate admin session: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	session := database.AdminSession{TokenHash: hashSessionToken(token), UserID: userID, ExpiresAt: now.Add(s.ttl), CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&session).Error; err != nil {
		return "", fmt.Errorf("create admin session: %w", err)
	}
	return token, nil
}

func (s *sessionService) authenticate(token string) (*database.User, bool, error) {
	now := time.Now()
	var session database.AdminSession
	if err := s.db.Where("token_hash = ? AND expires_at > ?", hashSessionToken(token), now).First(&session).Error; err != nil {
		return nil, false, errInvalidAdminSession
	}
	var user database.User
	if err := s.db.First(&user, "id = ?", session.UserID).Error; err != nil || !user.IsAdmin {
		_ = s.revoke(token)
		return nil, false, errInvalidAdminSession
	}
	renewed := false
	if time.Until(session.ExpiresAt) < s.renewAfter {
		result := s.db.Model(&database.AdminSession{}).
			Where("token_hash = ? AND expires_at > ?", session.TokenHash, now).
			Updates(map[string]interface{}{"expires_at": now.Add(s.ttl), "updated_at": now})
		if result.Error != nil || result.RowsAffected == 0 {
			return nil, false, errInvalidAdminSession
		}
		renewed = true
	}
	return &user, renewed, nil
}

func (s *sessionService) revoke(token string) error {
	if token == "" {
		return nil
	}
	return s.db.Delete(&database.AdminSession{}, "token_hash = ?", hashSessionToken(token)).Error
}

func (s *sessionService) deleteExpired(now time.Time) error {
	return s.db.Where("expires_at <= ?", now).Delete(&database.AdminSession{}).Error
}

func hashSessionToken(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func userIDString(user *database.User) string {
	return strconv.FormatInt(user.ID, 10)
}
