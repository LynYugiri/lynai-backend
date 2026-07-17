package sync

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
)

const syncRequestDomain = "LynAI/v1/sync-request\x00"

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`)

var (
	ErrSignatureRequired    = errors.New("device signature is required")
	ErrInvalidSignedRequest = errors.New("invalid signed sync request")
	ErrUnknownDevice        = errors.New("unknown or revoked device")
)

type signedRequest struct {
	RequestID string
	DeviceID  string
	BodyHash  []byte
}

func verifySignedRequest(db *gorm.DB, headers map[string]string, userID int64, sessionID, method, target string, bodyHash []byte, now time.Time, clockSkew time.Duration) (signedRequest, error) {
	if headers["protocol"] == "" && headers["requestID"] == "" && headers["timestamp"] == "" && headers["bodyHash"] == "" && headers["signature"] == "" && headers["deviceID"] == "" {
		return signedRequest{}, ErrSignatureRequired
	}
	if headers["protocol"] != "1" || !requestIDPattern.MatchString(headers["requestID"]) {
		return signedRequest{}, ErrInvalidSignedRequest
	}
	timestampMS, err := strconv.ParseInt(headers["timestamp"], 10, 64)
	if err != nil {
		return signedRequest{}, ErrInvalidSignedRequest
	}
	timestamp := time.UnixMilli(timestampMS)
	if timestamp.Before(now.Add(-clockSkew)) || timestamp.After(now.Add(clockSkew)) {
		return signedRequest{}, ErrInvalidSignedRequest
	}
	claimedHash, err := hex.DecodeString(headers["bodyHash"])
	if err != nil || len(claimedHash) != 32 || hex.EncodeToString(claimedHash) != headers["bodyHash"] || !equalBytes(claimedHash, bodyHash) {
		return signedRequest{}, ErrInvalidSignedRequest
	}
	signature, err := decodeCanonicalBase64URL(headers["signature"])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return signedRequest{}, ErrInvalidSignedRequest
	}
	var device database.UserDevice
	if err := db.Where("user_id = ? AND device_id = ? AND session_id = ? AND revoked_at IS NULL", userID, headers["deviceID"], sessionID).First(&device).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return signedRequest{}, ErrUnknownDevice
		}
		return signedRequest{}, err
	}
	message := SyncRequestMessage(1, strconv.FormatInt(userID, 10), sessionID, device.DeviceID, timestampMS, headers["requestID"], method, target, bodyHash)
	if !ed25519.Verify(ed25519.PublicKey(device.PublicKey), message, signature) {
		return signedRequest{}, ErrInvalidSignedRequest
	}
	return signedRequest{RequestID: headers["requestID"], DeviceID: device.DeviceID, BodyHash: bodyHash}, nil
}

// SyncRequestMessage returns the exact CBE1 bytes signed for a sync request.
func SyncRequestMessage(protocol uint16, userID, sessionID, deviceID string, timestampMS int64, requestID, method, target string, bodyHash []byte) []byte {
	version := make([]byte, 2)
	binary.BigEndian.PutUint16(version, protocol)
	timestamp := make([]byte, 8)
	binary.BigEndian.PutUint64(timestamp, uint64(timestampMS))
	fields := [][]byte{version, []byte(userID), []byte(sessionID), []byte(deviceID), timestamp, []byte(requestID), []byte(method), []byte(target), bodyHash}
	message := []byte(syncRequestDomain)
	for i, value := range fields {
		header := make([]byte, 6)
		binary.BigEndian.PutUint16(header[:2], uint16(i+1))
		binary.BigEndian.PutUint32(header[2:], uint32(len(value)))
		message = append(message, header...)
		message = append(message, value...)
	}
	return message
}

func decodeCanonicalBase64URL(value string) ([]byte, error) {
	if value == "" || strings.Contains(value, "=") {
		return nil, errors.New("non-canonical base64url")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("non-canonical base64url")
	}
	return decoded, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
