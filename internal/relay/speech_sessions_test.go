package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/pgtest"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func speechStoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.User{}, &database.RelayProvider{}, &database.RelayProviderCredential{}, &database.RelayModel{}, &database.RelayModelBinding{}, &database.RelaySpeechSession{}); err != nil {
		t.Fatal(err)
	}
	users := []database.User{{ID: 1, Phone: "1", DisplayName: "one"}, {ID: 2, Phone: "2", DisplayName: "two"}}
	if err := db.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	provider := database.RelayProvider{ID: 1, Name: "speech", Endpoint: "https://example.com", APIFormat: APIFormatVivoLASR, Config: EncodeProviderConfig(ProviderConfig{AppID: "app"}), Enabled: true}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	credential := database.RelayProviderCredential{ID: 1, ProviderID: 1, Name: "key", APIKey: "x", Enabled: true}
	if err := db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	model := database.RelayModel{ID: 1, ModelID: "speech", Category: CategorySpeech, Enabled: true}
	if err := db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	binding := database.RelayModelBinding{ID: 1, RelayModelID: 1, ProviderID: 1, UpstreamModel: "speech-upstream", Weight: 1, Enabled: true}
	if err := db.Create(&binding).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSpeechSessionStoreSharedUserIsolationAndTTL(t *testing.T) {
	db := speechStoreTestDB(t)
	storeA := newSpeechSessionStore(db, time.Hour, 2, 3)
	storeB := newSpeechSessionStore(db, time.Hour, 2, 3)
	candidate := Candidate{Provider: database.RelayProvider{ID: 1, Endpoint: "https://example.com", APIFormat: APIFormatVivoLASR, Config: EncodeProviderConfig(ProviderConfig{AppID: "app"})}, Credential: database.RelayProviderCredential{ID: 1, ProviderID: 1}, Binding: database.RelayModelBinding{ID: 1, ProviderID: 1, UpstreamModel: "speech-upstream"}, Model: database.RelayModel{ID: 1, ModelID: "speech"}}
	appID := "app"
	if err := storeA.reserve("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1", &candidate, &appID); err != nil {
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
	candidate := Candidate{Provider: database.RelayProvider{ID: 1, Endpoint: "https://example.com", APIFormat: APIFormatVivoLASR, Config: EncodeProviderConfig(ProviderConfig{AppID: "app"})}, Credential: database.RelayProviderCredential{ID: 1, ProviderID: 1}, Binding: database.RelayModelBinding{ID: 1, ProviderID: 1, UpstreamModel: "speech-upstream"}, Model: database.RelayModel{ID: 1, ModelID: "speech"}}
	appID := "app"
	if err := store.reserve("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1", &candidate, &appID); err != nil {
		t.Fatal(err)
	}
	if err := store.reserve("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "1", &candidate, &appID); !errors.Is(err, errSpeechCapacity) {
		t.Fatalf("per-user reserve error = %v", err)
	}
	if err := store.reserve("cccccccccccccccccccccccccccccccc", "2", &candidate, &appID); err != nil {
		t.Fatal(err)
	}
	store.perUserLimit = 2
	if err := store.reserve("dddddddddddddddddddddddddddddddd", "1", &candidate, &appID); !errors.Is(err, errSpeechCapacity) {
		t.Fatalf("global reserve error = %v", err)
	}
	store.deleteReservation("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1")
	if err := store.reserve("dddddddddddddddddddddddddddddddd", "1", &candidate, &appID); err != nil {
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

func TestPostgresSpeechReservationWaitsForCredentialReplacement(t *testing.T) {
	db := pgtest.Open(t)
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	user := database.User{ID: 1, Phone: "1", PasswordHash: "hash", DisplayName: "one"}
	provider := database.RelayProvider{ID: 1, Name: "speech", Endpoint: "https://example.com", APIFormat: APIFormatVivoLASR, Config: EncodeProviderConfig(ProviderConfig{AppID: "app"}), Enabled: true}
	credential := database.RelayProviderCredential{ID: 1, ProviderID: 1, Name: "key", APIKey: "old", Enabled: true}
	model := database.RelayModel{ID: 1, ModelID: "speech", Category: CategorySpeech, Enabled: true}
	binding := database.RelayModelBinding{ID: 1, RelayModelID: 1, ProviderID: 1, UpstreamModel: "speech-upstream", Weight: 1, Enabled: true}
	for _, value := range []interface{}{&user, &provider, &credential, &model, &binding} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Exec("SELECT id FROM relay_provider_credentials WHERE id = ? FOR UPDATE", credential.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Model(&database.RelayProviderCredential{}).Where("id = ?", credential.ID).Update("api_key", "replacement").Error; err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{Provider: provider, Credential: credential, Model: model, Binding: binding}
	appID := "app"
	reserved := make(chan error, 1)
	go func() {
		reserved <- newSpeechSessionStore(db, time.Hour, 2, 3).reserve("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1", &candidate, &appID)
	}()
	select {
	case err := <-reserved:
		t.Fatalf("reservation did not wait for credential lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := <-reserved; err != nil {
		t.Fatal(err)
	}
	if candidate.Credential.APIKey != "replacement" {
		t.Fatalf("reserved credential key = %q", candidate.Credential.APIKey)
	}
}
