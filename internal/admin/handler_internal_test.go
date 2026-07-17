package admin

import (
	"net/http"
	"net/http/httptest"
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

func TestReplaceRelayModelsClearsLegacyModels(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(&database.RelayProvider{}, &database.RelayModel{}); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	provider := database.RelayProvider{
		Name:      "legacy provider",
		Endpoint:  "https://example.com/v1",
		APIKey:    "secret",
		APIFormat: relay.APIFormatOpenAI,
		Models:    `["legacy-model"]`,
		Enabled:   true,
	}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}

	if err := db.Transaction(func(tx *gorm.DB) error {
		return replaceRelayModelsTx(tx, provider.ID, nil)
	}); err != nil {
		t.Fatalf("replace models: %v", err)
	}
	if err := db.First(&provider, "id = ?", provider.ID).Error; err != nil {
		t.Fatalf("reload provider: %v", err)
	}
	if provider.Models != "" {
		t.Fatalf("legacy models = %q, want empty", provider.Models)
	}

	resolved, err := relay.NewService(db).Resolve(relay.APIFormatOpenAI, "legacy-model")
	if err == nil || resolved != nil {
		t.Fatalf("legacy model resolved after deletion: resolved=%#v err=%v", resolved, err)
	}
}
