package relay

import (
	"errors"
	"math/rand"
	"net/http"
	"testing"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func routerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.RelayProvider{}, &database.RelayProviderCredential{}, &database.RelayModel{}, &database.RelayModelBinding{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRouterWeightedSelectionUsesIntervalsWithoutReplacement(t *testing.T) {
	router := newRouterWithSource(routerTestDB(t), rand.NewSource(1))
	bindings := []database.RelayModelBinding{{ID: 1, Weight: 1}, {ID: 2, Weight: 3}, {ID: 3, Weight: 6}}
	order := router.bindingOrder(bindings)
	if len(order) != 3 || order[0].ID != 2 || order[1].ID != 3 || order[2].ID != 1 {
		t.Fatalf("weighted order = %#v", order)
	}
}

func TestRouterCredentialPriorityRoundRobinAndFallback(t *testing.T) {
	db := routerTestDB(t)
	provider := database.RelayProvider{ID: 1, Name: "p", Endpoint: "https://example.com", APIFormat: APIFormatOpenAI, Enabled: true}
	model := database.RelayModel{ID: 1, ModelID: "public", Category: CategoryChat, Enabled: true}
	binding := database.RelayModelBinding{ID: 1, RelayModelID: 1, ProviderID: 1, UpstreamModel: "upstream", Weight: 1, Enabled: true}
	credentials := []database.RelayProviderCredential{{ID: 1, ProviderID: 1, Name: "a", APIKey: "a", Priority: 10, Enabled: true}, {ID: 2, ProviderID: 1, Name: "b", APIKey: "b", Priority: 10, Enabled: true}, {ID: 3, ProviderID: 1, Name: "c", APIKey: "c", Priority: 1, Enabled: true}}
	for _, value := range []interface{}{&provider, &model, &binding, &credentials} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	router := newRouterWithSource(db, rand.NewSource(1))
	_, first, err := router.Candidates("public")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := router.Candidates("public")
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Credential.ID != 1 || first[1].Credential.ID != 2 || first[2].Credential.ID != 3 || second[0].Credential.ID != 2 {
		t.Fatalf("credential order = %v / %v", credentialIDs(first), credentialIDs(second))
	}
	router.Cooldown(first[0], http.StatusUnauthorized, "", false)
	router.Cooldown(first[1], http.StatusUnauthorized, "", false)
	_, fallback, err := router.Candidates("public")
	if err != nil {
		t.Fatal(err)
	}
	if fallback[0].Credential.ID != 3 {
		t.Fatalf("fallback = %v", credentialIDs(fallback))
	}
	router.ReleaseCredential(1)
	_, released, err := router.Candidates("public")
	if err != nil {
		t.Fatal(err)
	}
	if released[0].Credential.ID != 1 {
		t.Fatalf("released order = %v", credentialIDs(released))
	}
}

func TestCooldownDurationRetryAfterAndExponentialCaps(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	if got := cooldownDuration("17", now, 1, 30*time.Second, 5*time.Minute); got != 17*time.Second {
		t.Fatalf("seconds = %v", got)
	}
	date := now.Add(2 * time.Minute).Format(http.TimeFormat)
	if got := cooldownDuration(date, now, 1, 30*time.Second, 5*time.Minute); got != 2*time.Minute {
		t.Fatalf("date = %v", got)
	}
	if got := cooldownDuration("3600", now, 1, 30*time.Second, 5*time.Minute); got != 5*time.Minute {
		t.Fatalf("seconds cap = %v", got)
	}
	if got := cooldownDuration(now.Add(time.Hour).Format(http.TimeFormat), now, 1, 30*time.Second, 5*time.Minute); got != 5*time.Minute {
		t.Fatalf("date cap = %v", got)
	}
	if got := cooldownDuration("", now, 10, 30*time.Second, 5*time.Minute); got != 5*time.Minute {
		t.Fatalf("network cap = %v", got)
	}
	if got := cooldownDuration("", now, 10, time.Minute, 30*time.Minute); got != 30*time.Minute {
		t.Fatalf("429 cap = %v", got)
	}
}

func TestHandlerStreamResultDoesNotCoolDownClientDisconnect(t *testing.T) {
	db := routerTestDB(t)
	provider := database.RelayProvider{ID: 1, Name: "p", Endpoint: "https://example.com", APIFormat: APIFormatOpenAI, Enabled: true}
	model := database.RelayModel{ID: 1, ModelID: "public", Category: CategoryChat, Enabled: true}
	binding := database.RelayModelBinding{ID: 1, RelayModelID: 1, ProviderID: 1, UpstreamModel: "upstream", Weight: 1, Enabled: true}
	credential := database.RelayProviderCredential{ID: 1, ProviderID: 1, Name: "key", APIKey: "key", Enabled: true}
	for _, value := range []interface{}{&provider, &model, &binding, &credential} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	router := newRouterWithSource(db, rand.NewSource(1))
	handler := &Handler{svc: &Service{router: router}}
	_, candidates, err := router.Candidates("public")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("initial candidates = %#v, err = %v", candidates, err)
	}
	handler.recordStreamResult(candidates[0], downstreamWriteError{err: errors.New("client disconnected")})
	_, candidates, err = router.Candidates("public")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates after disconnect = %#v, err = %v", candidates, err)
	}
	handler.recordStreamResult(candidates[0], errors.New("invalid upstream event"))
	_, candidates, err = router.Candidates("public")
	if err != nil || len(candidates) != 0 {
		t.Fatalf("candidates after upstream failure = %#v, err = %v", candidates, err)
	}
}

func TestMergeProviderConfigCredentialOverridesNonEmptyFields(t *testing.T) {
	provider := ProviderConfig{AppID: "provider", ClientVersion: "1", Package: "pkg", OCRPos: "2"}
	credential := ProviderConfig{AppID: "credential", ClientVersion: "", OCRPos: "4"}
	merged := MergeProviderConfig(provider, credential)
	if merged.AppID != "credential" || merged.ClientVersion != "1" || merged.Package != "pkg" || merged.OCRPos != "4" {
		t.Fatalf("merged config = %#v", merged)
	}
}

func credentialIDs(candidates []Candidate) []int64 {
	ids := make([]int64, len(candidates))
	for i := range candidates {
		ids[i] = candidates[i].Credential.ID
	}
	return ids
}
