package device

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ChallengeTTL      = 5 * time.Minute
	ProtocolVersion   = uint16(1)
	enrollmentDomain  = "LynAI/v1/enrollment\x00"
	challengeByteSize = 32
)

var (
	ErrInvalidChallenge = errors.New("invalid or expired device challenge")
	ErrInvalidKey       = errors.New("invalid Ed25519 public key")
	ErrInvalidSignature = errors.New("invalid device signature")
	ErrInvalidDeviceID  = errors.New("invalid device ID")
	ErrInvalidName      = errors.New("invalid device name")
	ErrInvalidPlatform  = errors.New("invalid device platform")
	ErrInvalidProtocol  = errors.New("unsupported device protocol")
	ErrDeviceConflict   = errors.New("device identity conflicts with the enrolled public key")
	ErrDeviceOwned      = errors.New("device identity is already enrolled by another account")
	ErrDeviceRevoked    = errors.New("device identity is revoked")

	deviceIDPattern = regexp.MustCompile(`^[a-z2-7]{52}$`)
	platformPattern = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)
)

type Challenge struct {
	ID        string
	Value     string
	UserID    string
	SessionID string
	ExpiresAt time.Time
}

type Proposal struct {
	DeviceID        string
	PublicKey       string
	Name            string
	Platform        string
	ProtocolVersion uint16
}

type Enrollment struct {
	ChallengeID     string
	Challenge       string
	DeviceID        string
	PublicKey       string
	Signature       string
	Name            string
	Platform        string
	ProtocolVersion uint16
}

type Service struct {
	db  *gorm.DB
	now func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, now: time.Now}
}

func (s *Service) IssueChallenge(userID int64, sessionID string, proposal Proposal) (Challenge, error) {
	publicKey, err := validateProposal(proposal)
	if err != nil {
		return Challenge{}, err
	}
	raw := make([]byte, challengeByteSize)
	if _, err := rand.Read(raw); err != nil {
		return Challenge{}, fmt.Errorf("generate challenge: %w", err)
	}
	id, err := randomID()
	if err != nil {
		return Challenge{}, err
	}
	digest := sha256.Sum256(raw)
	now := s.now()
	record := database.DeviceChallenge{
		ID:            id,
		UserID:        userID,
		SessionID:     sessionID,
		DeviceID:      proposal.DeviceID,
		PublicKey:     publicKey,
		Name:          proposal.Name,
		Platform:      proposal.Platform,
		Protocol:      proposal.ProtocolVersion,
		ChallengeHash: digest[:],
		ExpiresAt:     now.Add(ChallengeTTL),
		CreatedAt:     now,
	}
	if err := s.db.Create(&record).Error; err != nil {
		return Challenge{}, fmt.Errorf("store challenge: %w", err)
	}
	return Challenge{
		ID:        id,
		Value:     base64.RawURLEncoding.EncodeToString(raw),
		UserID:    strconv.FormatInt(userID, 10),
		SessionID: sessionID,
		ExpiresAt: record.ExpiresAt,
	}, nil
}

func (s *Service) Enroll(userID int64, sessionID string, enrollment Enrollment) (*database.UserDevice, error) {
	challengeID, err := decodeBase64URL(enrollment.ChallengeID)
	if err != nil || len(challengeID) != 24 {
		return nil, ErrInvalidChallenge
	}
	challenge, err := decodeBase64URL(enrollment.Challenge)
	if err != nil || len(challenge) != challengeByteSize {
		return nil, ErrInvalidChallenge
	}
	publicKey, err := validateProposal(Proposal{
		DeviceID: enrollment.DeviceID, PublicKey: enrollment.PublicKey, Name: enrollment.Name,
		Platform: enrollment.Platform, ProtocolVersion: enrollment.ProtocolVersion,
	})
	if err != nil {
		return nil, err
	}
	signature, err := decodeBase64URL(enrollment.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, ErrInvalidSignature
	}
	message := EnrollmentMessage(
		enrollment.ProtocolVersion, enrollment.ChallengeID, challenge,
		strconv.FormatInt(userID, 10), sessionID, enrollment.DeviceID,
		publicKey, enrollment.Name, enrollment.Platform,
	)
	if !ed25519.Verify(ed25519.PublicKey(publicKey), message, signature) {
		return nil, ErrInvalidSignature
	}
	digest := sha256.Sum256(challenge)
	now := s.now()
	var enrolled database.UserDevice
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var owner database.UserDevice
		ownerResult := tx.Where("user_id <> ? AND (device_id = ? OR public_key = ?)", userID, enrollment.DeviceID, publicKey).First(&owner)
		if ownerResult.Error == nil {
			return ErrDeviceOwned
		}
		if !errors.Is(ownerResult.Error, gorm.ErrRecordNotFound) {
			return ownerResult.Error
		}
		result := tx.Model(&database.DeviceChallenge{}).
			Where(`id = ? AND user_id = ? AND session_id = ? AND device_id = ? AND public_key = ?
				AND name = ? AND platform = ? AND protocol = ? AND challenge_hash = ?
				AND consumed_at IS NULL AND expires_at > ?`,
				enrollment.ChallengeID, userID, sessionID, enrollment.DeviceID, publicKey,
				enrollment.Name, enrollment.Platform, enrollment.ProtocolVersion, digest[:], now).
			Update("consumed_at", now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrInvalidChallenge
		}

		created := database.UserDevice{
			UserID: userID, DeviceID: enrollment.DeviceID, SessionID: sessionID,
			Name: enrollment.Name, Platform: enrollment.Platform,
			Protocol: enrollment.ProtocolVersion, PublicKey: publicKey,
		}
		result = tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "device_id"}},
			DoNothing: true,
		}).Create(&created)
		if result.Error != nil {
			return result.Error
		}
		if err := tx.Where("user_id = ? AND device_id = ?", userID, enrollment.DeviceID).First(&enrolled).Error; err != nil {
			return err
		}
		if !bytes.Equal(enrolled.PublicKey, publicKey) {
			return ErrDeviceConflict
		}
		if enrolled.RevokedAt != nil {
			return ErrDeviceRevoked
		}
		if result.RowsAffected == 0 {
			updates := map[string]any{
				"session_id": sessionID, "name": enrollment.Name,
				"platform": enrollment.Platform, "protocol": enrollment.ProtocolVersion,
			}
			if err := tx.Model(&database.UserDevice{}).
				Where("user_id = ? AND device_id = ?", userID, enrollment.DeviceID).
				Updates(updates).Error; err != nil {
				return err
			}
			if err := tx.Where("user_id = ? AND device_id = ?", userID, enrollment.DeviceID).First(&enrolled).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if conflict := deviceUniqueConflict(err); conflict != nil {
			return nil, conflict
		}
		if errors.Is(err, ErrInvalidChallenge) || errors.Is(err, ErrDeviceConflict) || errors.Is(err, ErrDeviceOwned) || errors.Is(err, ErrDeviceRevoked) {
			return nil, err
		}
		return nil, fmt.Errorf("enroll device: %w", err)
	}
	return &enrolled, nil
}

func deviceUniqueConflict(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return nil
	}
	switch pgErr.ConstraintName {
	case "idx_user_devices_device_id_global", "idx_user_devices_public_key_global":
		return ErrDeviceOwned
	default:
		return ErrDeviceConflict
	}
}

// EnrollmentMessage returns the exact domain-separated CBE1 bytes signed by a device.
func EnrollmentMessage(protocol uint16, challengeID string, challenge []byte, userID, sessionID, deviceID string, publicKey []byte, name, platform string) []byte {
	object := make([]byte, 0, 256)
	version := make([]byte, 2)
	binary.BigEndian.PutUint16(version, protocol)
	fields := [][]byte{
		version, []byte(challengeID), challenge, []byte(userID), []byte(sessionID),
		[]byte(deviceID), publicKey, []byte(name), []byte(platform),
	}
	for i, value := range fields {
		header := make([]byte, 6)
		binary.BigEndian.PutUint16(header[:2], uint16(i+1))
		binary.BigEndian.PutUint32(header[2:], uint32(len(value)))
		object = append(object, header...)
		object = append(object, value...)
	}
	return append([]byte(enrollmentDomain), object...)
}

func validateProposal(proposal Proposal) ([]byte, error) {
	if proposal.ProtocolVersion != ProtocolVersion {
		return nil, ErrInvalidProtocol
	}
	publicKey, err := decodeBase64URL(proposal.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrInvalidKey
	}
	if !deviceIDPattern.MatchString(proposal.DeviceID) || proposal.DeviceID != deriveDeviceID(publicKey) {
		return nil, ErrInvalidDeviceID
	}
	if !utf8.ValidString(proposal.Name) || strings.TrimSpace(proposal.Name) != proposal.Name || len([]byte(proposal.Name)) < 1 || len([]byte(proposal.Name)) > 64 {
		return nil, ErrInvalidName
	}
	if !platformPattern.MatchString(proposal.Platform) {
		return nil, ErrInvalidPlatform
	}
	return publicKey, nil
}

func deriveDeviceID(publicKey []byte) string {
	digest := sha256.Sum256(publicKey)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))
}

func randomID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeBase64URL(value string) ([]byte, error) {
	if value == "" || strings.Contains(value, "=") {
		return nil, errors.New("non-canonical base64url")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("non-canonical base64url")
	}
	return decoded, nil
}

func (s *Service) List(userID int64) ([]database.UserDevice, error) {
	var devices []database.UserDevice
	if err := s.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&devices).Error; err != nil {
		return nil, err
	}
	return devices, nil
}

func (s *Service) Current(userID int64, sessionID string) (*database.UserDevice, error) {
	var result database.UserDevice
	err := s.db.Where("user_id = ? AND session_id = ? AND revoked_at IS NULL", userID, sessionID).Order("created_at DESC").First(&result).Error
	return &result, err
}

func (s *Service) Rename(userID int64, deviceID, name string) (*database.UserDevice, error) {
	if !utf8.ValidString(name) || strings.TrimSpace(name) != name || len([]byte(name)) < 1 || len([]byte(name)) > 64 {
		return nil, ErrInvalidName
	}
	result := s.db.Model(&database.UserDevice{}).Where("device_id = ? AND user_id = ? AND revoked_at IS NULL", deviceID, userID).Update("name", name)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, gorm.ErrRecordNotFound
	}
	var updated database.UserDevice
	if err := s.db.Where("device_id = ? AND user_id = ?", deviceID, userID).First(&updated).Error; err != nil {
		return nil, err
	}
	return &updated, nil
}

func (s *Service) Revoke(userID int64, deviceID string) error {
	now := s.now()
	return s.db.Transaction(func(tx *gorm.DB) error {
		var target database.UserDevice
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("device_id = ? AND user_id = ? AND revoked_at IS NULL", deviceID, userID).
			First(&target).Error; err != nil {
			return err
		}
		if err := tx.Model(&target).Update("revoked_at", now).Error; err != nil {
			return err
		}
		return tx.Model(&database.UserSession{}).
			Where("id = ? AND user_id = ? AND revoked_at IS NULL", target.SessionID, userID).
			Update("revoked_at", now).Error
	})
}
