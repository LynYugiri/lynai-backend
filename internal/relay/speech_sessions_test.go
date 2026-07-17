package relay

import (
	"errors"
	"testing"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func speechStoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.User{}, &database.RelayProvider{}, &database.RelayModel{}, &database.RelaySpeechSession{}); err != nil {
		t.Fatal(err)
	}
	users := []database.User{{ID: 1, Phone: "1", DisplayName: "one"}, {ID: 2, Phone: "2", DisplayName: "two"}}
	if err := db.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	provider := database.RelayProvider{ID: 1, Name: "speech", Endpoint: "https://example.com", APIKey: "x", APIFormat: APIFormatVivoLASR, Enabled: true}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	model := database.RelayModel{ID: 1, ProviderID: 1, ModelID: "speech", Category: CategorySpeech, Enabled: true}
	if err := db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSpeechSessionStoreSharedUserIsolationAndTTL(t *testing.T) {
	db := speechStoreTestDB(t)
	storeA := newSpeechSessionStore(db, time.Hour, 2, 3)
	storeB := newSpeechSessionStore(db, time.Hour, 2, 3)
	resolved := &ResolvedModel{Provider: database.RelayProvider{ID: 1}, Model: database.RelayModel{ID: 1, ModelID: "speech"}}
	if err := storeA.reserve("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1", resolved, "app"); err != nil {
		t.Fatal(err)
	}
	if _, ok := storeB.get("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1"); ok {
		t.Fatal("reservation became visible before completion")
	}
	if err := storeA.completeReservation("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1", "upstream"); err != nil {
		t.Fatal(err)
	}
	if _, ok := storeB.get("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2"); ok {
		t.Fatal("another user accessed the session")
	}
	session, ok := storeB.get("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1")
	if !ok || session.UpstreamAudioID != "upstream" {
		t.Fatalf("shared session = %#v, %v", session, ok)
	}
	if err := db.Model(&database.RelaySpeechSession{}).Where("id = ?", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, ok := storeA.get("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1"); ok {
		t.Fatal("expired session was returned")
	}
	if err := storeA.deleteExpired(time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestSpeechSessionStoreCapacityIncludesReservations(t *testing.T) {
	db := speechStoreTestDB(t)
	store := newSpeechSessionStore(db, time.Hour, 1, 2)
	resolved := &ResolvedModel{Provider: database.RelayProvider{ID: 1}, Model: database.RelayModel{ID: 1, ModelID: "speech"}}
	if err := store.reserve("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1", resolved, "app"); err != nil {
		t.Fatal(err)
	}
	if err := store.reserve("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "1", resolved, "app"); !errors.Is(err, errSpeechCapacity) {
		t.Fatalf("per-user reserve error = %v", err)
	}
	if err := store.reserve("cccccccccccccccccccccccccccccccc", "2", resolved, "app"); err != nil {
		t.Fatal(err)
	}
	store.perUserLimit = 2
	if err := store.reserve("dddddddddddddddddddddddddddddddd", "1", resolved, "app"); !errors.Is(err, errSpeechCapacity) {
		t.Fatalf("global reserve error = %v", err)
	}
	store.deleteReservation("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1")
	if err := store.reserve("dddddddddddddddddddddddddddddddd", "1", resolved, "app"); err != nil {
		t.Fatalf("reserve after release: %v", err)
	}
}

func TestNewSpeechSessionIDIsRandom(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id, err := newSpeechSessionID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 32 {
			t.Fatalf("session ID length = %d", len(id))
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate session ID %q", id)
		}
		seen[id] = struct{}{}
	}
}
