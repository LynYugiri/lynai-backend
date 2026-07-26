package sync

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestSyncRequestFixedVector(t *testing.T) {
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	bodyHash, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	privateKey := ed25519.NewKeyFromSeed(seed)
	message := SyncRequestMessage(1, "42", "session-vector-1", "kzdvvj2umnduyauf35o36k6kw462mujvra46tn3uqgzovmihocga",
		1700000000123, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYX", "POST", "/sync/changes", bodyHash, 1)
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))

	const expectedMessage = "4c796e41492f76312f73796e632d72657175657374000001000000020001000200000002343200030000001073657373696f6e2d766563746f722d310004000000346b7a6476766a32756d6e64757961756633356f33366b366b773436326d756a7672613436746e337571677a6f766d69686f6367610005000000080000018bcfe5687b00060000002041414543417751464267634943516f4c4441304f4478415245684d5546525958000700000004504f535400080000000d2f73796e632f6368616e676573000900000020000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f000a000000080000000000000001"
	const expectedSignature = "-Q019gyA3ngLx2w19PfpFX7FqV6X04RJrECgf2x5NJMaKj5A4h_JOcqk3ga52-EPwZFsHJas15Osk8w4ygQ-Dg"
	if got := hex.EncodeToString(message); got != expectedMessage {
		t.Fatalf("message = %s", got)
	}
	if signature != expectedSignature {
		t.Fatalf("signature = %s", signature)
	}
}

func TestVerifySignedRequestDistinguishesDeviceState(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.UserDevice{}); err != nil {
		t.Fatal(err)
	}
	activeKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	revokedKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	revokedAt := now
	for _, device := range []database.UserDevice{
		{UserID: 42, DeviceID: "active", SessionID: "session-a", Name: "active", Platform: "linux", Protocol: 1, PublicKey: activeKey},
		{UserID: 42, DeviceID: "revoked", SessionID: "session-a", Name: "revoked", Platform: "linux", Protocol: 1, PublicKey: revokedKey, RevokedAt: &revokedAt},
	} {
		if err := db.Create(&device).Error; err != nil {
			t.Fatal(err)
		}
	}
	bodyHash := make([]byte, 32)
	headers := map[string]string{
		"protocol": "1", "timestamp": strconv.FormatInt(now.UnixMilli(), 10),
		"requestID": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "bodyHash": hex.EncodeToString(bodyHash),
		"signature":          base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
		"expectedGeneration": "1",
	}
	for _, tc := range []struct {
		name, deviceID, sessionID string
		want                      error
	}{
		{name: "unknown", deviceID: "missing", sessionID: "session-a", want: ErrUnknownDevice},
		{name: "revoked", deviceID: "revoked", sessionID: "session-a", want: ErrRevokedDevice},
		{name: "session mismatch", deviceID: "active", sessionID: "session-b", want: ErrDeviceSessionMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers["deviceID"] = tc.deviceID
			_, err := verifySignedRequest(db, headers, 42, tc.sessionID, "POST", "/sync/changes", bodyHash, now, time.Minute)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}
