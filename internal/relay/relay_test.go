package relay_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/auth"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/relay"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupRelayTest(t *testing.T, upstream http.HandlerFunc) (*httptest.Server, string, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.AutoMigrate(database.AllModels()...); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)

	models, _ := json.Marshal([]string{"gpt-test", "gpt-stream"})
	if err := db.Create(&database.RelayProvider{
		ID:        1,
		Name:      "test upstream",
		Endpoint:  upstreamServer.URL,
		APIKey:    "upstream-secret",
		APIFormat: relay.APIFormatOpenAI,
		Models:    string(models),
		Enabled:   true,
	}).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}

	jwtMgr := auth.NewJWTManager("test-secret")
	authSvc := auth.NewService(db, jwtMgr, database.NewSnowflakeGenerator(0))
	_, pair, err := authSvc.Register("13800000000", "secret123", "tester")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	handler := relay.NewHandler(relay.NewService(db))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/relay")
	g.Use(auth.AuthMiddleware(jwtMgr))
	g.POST("/chat", handler.Chat)
	g.GET("/models", handler.Models)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	return server, pair.AccessToken, db
}

func TestModelsRequiresAuth(t *testing.T) {
	server, _, _ := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {})
	resp, err := http.Get(server.URL + "/relay/models")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestModelsReturnsAPIType(t *testing.T) {
	server, token, _ := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {})
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/relay/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data := payload["data"].([]interface{})
	if len(data) != 2 {
		t.Fatalf("model count = %d, want 2", len(data))
	}
	first := data[0].(map[string]interface{})
	if first["api_type"] != "openai" {
		t.Fatalf("api_type = %v, want openai", first["api_type"])
	}
}

func TestChatStripsAPITypeAndForwards(t *testing.T) {
	var seenAuth string
	var seenBody map[string]interface{}
	server, token, _ := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("upstream path = %s, want /chat/completions", r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"content":"pong"}}]}`))
	})

	body := []byte(`{"model":"gpt-test","api_type":"openai","messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/relay/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if seenAuth != "Bearer upstream-secret" {
		t.Fatalf("upstream auth = %q", seenAuth)
	}
	if _, ok := seenBody["api_type"]; ok {
		t.Fatal("api_type was forwarded upstream")
	}
}

func TestChatStreamsSSE(t *testing.T) {
	server, token, _ := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: one\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	})

	body := []byte(`{"model":"gpt-stream","api_type":"openai","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/relay/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if got := string(raw); !strings.Contains(got, "data: one") || !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("stream body = %q", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestChatRejectsUnsupportedAPIType(t *testing.T) {
	server, token, _ := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called")
	})

	body := []byte(`{"model":"gpt-test","api_type":"anthropic","messages":[{"role":"user","content":"ping"}]}`)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/relay/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
	}
}
