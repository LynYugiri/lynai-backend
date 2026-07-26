package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/relay"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type testCredentialReleaser struct{ released int64 }

func (r *testCredentialReleaser) ReleaseCredential(id int64) { r.released = id }

func TestOpaqueAdminSessionConcurrentRenewal(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:admin-session?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&database.User{}, &database.AdminSession{}); err != nil {
		t.Fatal(err)
	}
	user := database.User{ID: 1, Phone: "1", DisplayName: "admin", IsAdmin: true}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	sessions := newSessionService(db, time.Hour)
	token, err := sessions.create(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&database.AdminSession{}).Where("token_hash = ?", hashSessionToken(token)).Update("expires_at", time.Now().Add(time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := sessions.authenticate(token)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent authenticate: %v", err)
		}
	}
	if _, _, err := sessions.authenticate(token); err != nil {
		t.Fatalf("stable token invalid after renewal: %v", err)
	}
}

func TestSetAdminCookieSecurityAttributes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/admin", nil)
	context.Request.Header.Set("X-Forwarded-Proto", "https")

	setAdminCookie(context, CookieName, "token", 60)
	cookie := recorder.Header().Get("Set-Cookie")
	for _, attribute := range []string{"HttpOnly", "SameSite=Lax", "Secure", "Path=/admin"} {
		if !strings.Contains(cookie, attribute) {
			t.Fatalf("cookie %q missing %s", cookie, attribute)
		}
	}
}

func TestAdminCookieRenewsWithDatabaseSession(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.User{}, &database.AdminSession{}); err != nil {
		t.Fatal(err)
	}
	user := database.User{ID: 1, Phone: "1", DisplayName: "admin", IsAdmin: true}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	sessions := newSessionService(db, time.Hour)
	token, err := sessions.create(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&database.AdminSession{}).Where("token_hash = ?", hashSessionToken(token)).Update("expires_at", time.Now().Add(time.Minute)).Error; err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	handler := &Handler{sessions: sessions, sessionTTL: time.Hour}
	router := gin.New()
	router.GET("/admin", handler.adminCookieMiddleware(), func(c *gin.Context) { c.Status(http.StatusOK) })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	request.AddCookie(&http.Cookie{Name: CookieName, Value: token})
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	cookies := recorder.Result().Cookies()
	found := false
	for _, cookie := range cookies {
		if cookie.Name == CookieName && cookie.Value == token && cookie.MaxAge == int(time.Hour.Seconds()) {
			found = true
		}
	}
	if !found {
		t.Fatalf("renewed admin cookie missing from %q", recorder.Header().Values("Set-Cookie"))
	}
}

func TestRelayCredentialBlankKeyPreservesAndActiveSessionBlocksReplacement(t *testing.T) {
	db := relayAdminTestDB(t)
	provider, credential, model, binding := relayAdminFixture(t, db, relay.APIFormatOpenAISpeech, relay.CategorySpeech)
	handler := &Handler{db: db}

	context, recorder := relayAdminPostContext("/admin/relay/credentials/1/edit", url.Values{"name": {"Primary"}, "priority": {"2"}, "enabled": {"on"}})
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	handler.UpdateRelayCredential(context)
	var saved database.RelayProviderCredential
	if err := db.First(&saved, credential.ID).Error; err != nil || saved.APIKey != "secret" {
		t.Fatalf("blank edit changed key: credential=%+v err=%v", saved, err)
	}

	session := database.RelaySpeechSession{ID: strings.Repeat("a", 32), UserID: 1, ProviderID: provider.ID, BindingID: binding.ID, CredentialID: credential.ID, ModelID: model.ModelID, UpstreamModel: binding.UpstreamModel, Endpoint: provider.Endpoint, APIFormat: provider.APIFormat, ConfigSnapshot: "{}", AppID: "app", UpstreamAudioID: "audio", TaskID: "task", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	context, recorder = relayAdminPostContext("/admin/relay/credentials/1/edit", url.Values{"name": {"Primary"}, "apiKey": {"replacement"}, "priority": {"2"}, "enabled": {"on"}})
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	handler.UpdateRelayCredential(context)
	if !strings.Contains(recorder.Header().Get("Location"), "API+Key") {
		t.Fatalf("active replacement redirect = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	db.First(&saved, credential.ID)
	if saved.APIKey != "secret" {
		t.Fatalf("blocked replacement persisted: %q", saved.APIKey)
	}
}

func TestRelayCredentialDeleteBlockedToggleAllowedAndRelease(t *testing.T) {
	db := relayAdminTestDB(t)
	provider, credential, model, binding := relayAdminFixture(t, db, relay.APIFormatOpenAISpeech, relay.CategorySpeech)
	releaser := &testCredentialReleaser{}
	handler := &Handler{db: db, credentialReleaser: releaser}
	session := database.RelaySpeechSession{ID: strings.Repeat("b", 32), UserID: 1, ProviderID: provider.ID, BindingID: binding.ID, CredentialID: credential.ID, ModelID: model.ModelID, UpstreamModel: binding.UpstreamModel, Endpoint: provider.Endpoint, APIFormat: provider.APIFormat, ConfigSnapshot: "{}", AppID: "app", UpstreamAudioID: "audio", TaskID: "task", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}

	context, _ := relayAdminPostContext("/delete", nil)
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	handler.DeleteRelayCredential(context)
	var count int64
	db.Model(&database.RelayProviderCredential{}).Where("id = ?", credential.ID).Count(&count)
	if count != 1 {
		t.Fatal("active credential was deleted")
	}
	context, _ = relayAdminPostContext("/toggle", nil)
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	handler.ToggleRelayCredential(context)
	var saved database.RelayProviderCredential
	db.First(&saved, credential.ID)
	if saved.Enabled {
		t.Fatal("active credential was not allowed to disable")
	}
	context, _ = relayAdminPostContext("/release", nil)
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	handler.ReleaseRelayCredential(context)
	if releaser.released != credential.ID {
		t.Fatalf("released id = %d", releaser.released)
	}
}

func TestRelayBindingValidation(t *testing.T) {
	db := relayAdminTestDB(t)
	handler := &Handler{db: db}
	imageProvider := database.RelayProvider{Name: "image", Endpoint: "https://example.com", APIFormat: relay.APIFormatVivoImage, Enabled: true}
	if err := db.Create(&imageProvider).Error; err != nil {
		t.Fatal(err)
	}
	model := database.RelayModel{ModelID: "chat", Category: relay.CategoryChat, Enabled: true}
	if err := db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	context, _ := relayAdminPostContext("/binding", url.Values{"providerId": {"1"}, "upstreamModel": {"x"}, "weight": {"0"}, "enabled": {"on"}})
	if _, err := handler.relayBindingFromForm(context, model, database.RelayModelBinding{}); err == nil || !strings.Contains(err.Error(), "分类") {
		t.Fatalf("category validation error = %v", err)
	}

	openAI, _, speechModel, _ := relayAdminFixture(t, db, relay.APIFormatOpenAISpeech, relay.CategorySpeech)
	vivo := database.RelayProvider{Name: "vivo", Endpoint: "https://example.com", APIFormat: relay.APIFormatVivoLASR, Enabled: true}
	if err := db.Create(&vivo).Error; err != nil {
		t.Fatal(err)
	}
	context, _ = relayAdminPostContext("/binding", url.Values{"providerId": {"3"}, "upstreamModel": {"vivo"}, "weight": {"1"}, "enabled": {"on"}})
	if _, err := handler.relayBindingFromForm(context, speechModel, database.RelayModelBinding{}); err == nil || !strings.Contains(err.Error(), "不能混用") {
		t.Fatalf("speech mix validation error = %v (openAI=%d)", err, openAI.ID)
	}
}

func relayAdminTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.RelayProvider{}, &database.RelayProviderCredential{}, &database.RelayModel{}, &database.RelayModelBinding{}, &database.RelaySpeechSession{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func relayAdminFixture(t *testing.T, db *gorm.DB, apiFormat, category string) (database.RelayProvider, database.RelayProviderCredential, database.RelayModel, database.RelayModelBinding) {
	t.Helper()
	provider := database.RelayProvider{Name: apiFormat, Endpoint: "https://example.com", APIFormat: apiFormat, Enabled: true}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	credential := database.RelayProviderCredential{ProviderID: provider.ID, Name: "Primary", APIKey: "secret", Enabled: true}
	if err := db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	model := database.RelayModel{ModelID: apiFormat + "-model", Category: category, Enabled: true}
	if err := db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	binding := database.RelayModelBinding{RelayModelID: model.ID, ProviderID: provider.ID, UpstreamModel: "upstream", Weight: 1, Enabled: true}
	if err := db.Create(&binding).Error; err != nil {
		t.Fatal(err)
	}
	return provider, credential, model, binding
}

func relayAdminPostContext(path string, values url.Values) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	context.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return context, recorder
}
