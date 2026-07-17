package device

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/pgtest"
)

func TestPostgresConcurrentEnrollMapsDeviceOwnershipConflict(t *testing.T) {
	db := pgtest.Open(t)
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrate PostgreSQL test schema: %v", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{
		DeviceID: deriveDeviceID(publicKey), PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		Name: "Race device", Platform: "linux", ProtocolVersion: ProtocolVersion,
	}
	services := []*Service{NewService(db), NewService(db)}
	enrollments := make([]Enrollment, 2)
	for i, userID := range []int64{1, 2} {
		sessionID := "session-" + string(rune('1'+i))
		challenge, err := services[i].IssueChallenge(userID, sessionID, proposal)
		if err != nil {
			t.Fatalf("issue challenge: %v", err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(challenge.Value)
		if err != nil {
			t.Fatal(err)
		}
		message := EnrollmentMessage(ProtocolVersion, challenge.ID, raw, challenge.UserID, sessionID, proposal.DeviceID, publicKey, proposal.Name, proposal.Platform)
		enrollments[i] = Enrollment{
			ChallengeID: challenge.ID, Challenge: challenge.Value, DeviceID: proposal.DeviceID,
			PublicKey: proposal.PublicKey, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message)),
			Name: proposal.Name, Platform: proposal.Platform, ProtocolVersion: ProtocolVersion,
		}
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for i, userID := range []int64{1, 2} {
		go func() {
			ready.Done()
			<-start
			_, err := services[i].Enroll(userID, "session-"+string(rune('1'+i)), enrollments[i])
			errs <- err
		}()
	}
	ready.Wait()
	close(start)

	var successes int
	var conflict error
	for range 2 {
		err := <-errs
		if err == nil {
			successes++
			continue
		}
		conflict = err
	}
	if successes != 1 {
		t.Fatalf("successful concurrent inserts = %d, want 1", successes)
	}
	if !errors.Is(conflict, ErrDeviceOwned) {
		t.Fatalf("concurrent Enroll conflict = %v, want %v", conflict, ErrDeviceOwned)
	}
}
