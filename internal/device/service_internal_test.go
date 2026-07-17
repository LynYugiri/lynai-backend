package device

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lynai/backend/internal/database"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestChallengeStorageBindingAndExpiry(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.UserDevice{}, &database.DeviceChallenge{}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	svc := NewService(db)
	svc.now = func() time.Time { return now }
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{
		DeviceID: deriveDeviceID(publicKey), PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		Name: "Bound device", Platform: "linux", ProtocolVersion: ProtocolVersion,
	}
	challenge, err := svc.IssueChallenge(42, "session-1", proposal)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(challenge.Value)
	if err != nil {
		t.Fatal(err)
	}
	var stored database.DeviceChallenge
	if err := db.First(&stored, "id = ?", challenge.ID).Error; err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if string(stored.ChallengeHash) != string(digest[:]) || stored.DeviceID != proposal.DeviceID || stored.Name != proposal.Name {
		t.Fatalf("stored challenge binding = %+v", stored)
	}
	if string(stored.ChallengeHash) == string(raw) {
		t.Fatal("raw challenge was stored")
	}
	if !stored.ExpiresAt.Equal(now.Add(ChallengeTTL)) {
		t.Fatalf("expiresAt = %v", stored.ExpiresAt)
	}
}

func TestExpiredChallengeIsRejected(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.UserDevice{}, &database.DeviceChallenge{}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	svc := NewService(db)
	svc.now = func() time.Time { return now }
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{
		DeviceID: deriveDeviceID(publicKey), PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		Name: "Expired device", Platform: "linux", ProtocolVersion: ProtocolVersion,
	}
	challenge, err := svc.IssueChallenge(42, "session-1", proposal)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(challenge.Value)
	message := EnrollmentMessage(1, challenge.ID, raw, "42", "session-1", proposal.DeviceID, publicKey, proposal.Name, proposal.Platform)
	svc.now = func() time.Time { return now.Add(ChallengeTTL) }
	_, err = svc.Enroll(42, "session-1", Enrollment{
		ChallengeID: challenge.ID, Challenge: challenge.Value, DeviceID: proposal.DeviceID,
		PublicKey: proposal.PublicKey, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message)),
		Name: proposal.Name, Platform: proposal.Platform, ProtocolVersion: ProtocolVersion,
	})
	if !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("expired enrollment error = %v", err)
	}
}

func TestDeviceIDUsesFullSHA256(t *testing.T) {
	publicKey := make([]byte, ed25519.PublicKeySize)
	digest := sha256.Sum256(publicKey)
	expected := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))
	if got := deriveDeviceID(publicKey); got != expected || len(got) != 52 {
		t.Fatalf("device ID = %q", got)
	}
}

func TestPostgresDeviceUniqueConflictMapping(t *testing.T) {
	for _, tc := range []struct {
		constraint string
		want       error
	}{
		{constraint: "idx_user_devices_device_id_global", want: ErrDeviceOwned},
		{constraint: "idx_user_devices_public_key_global", want: ErrDeviceOwned},
		{constraint: "idx_user_devices_user_public_key", want: ErrDeviceConflict},
	} {
		err := fmt.Errorf("create device: %w", &pgconn.PgError{Code: "23505", ConstraintName: tc.constraint})
		if got := deviceUniqueConflict(err); !errors.Is(got, tc.want) {
			t.Fatalf("constraint %q mapped to %v, want %v", tc.constraint, got, tc.want)
		}
	}
	if got := deviceUniqueConflict(errors.New("sqlite unique constraint")); got != nil {
		t.Fatalf("non-PostgreSQL error mapped to %v", got)
	}
}
