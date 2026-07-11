package relay_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/auth"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/relay"
	"github.com/lynai/backend/internal/testutil"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupRelayTest(t *testing.T, upstream http.HandlerFunc) (*testutil.TestServer, string, *gorm.DB) {
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

	upstreamServer := testutil.NewTestServerFunc(upstream)
	t.Cleanup(upstreamServer.Close)

	models, err := json.Marshal([]string{"gpt-test", "gpt-stream"})
	if err != nil {
		t.Fatalf("encode relay models: %v", err)
	}
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

	handler := relay.NewHandler(relay.NewServiceWithClient(db, testutil.NewHTTPClient()))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/relay")
	g.Use(auth.AuthMiddleware(jwtMgr), handler.LoggingMiddleware())
	g.POST("/chat", handler.Chat)
	g.POST("/v2/chat", handler.ChatV2)
	g.POST("/messages", handler.Messages)
	g.POST("/api/chat", handler.OllamaChat)
	g.POST("/transcribe", handler.Transcribe)
	g.POST("/ocr", handler.OCR)
	g.POST("/images/generations", handler.ImageGenerations)
	g.GET("/models", handler.Models)
	g.GET("/config", handler.Config)
	g.GET("/v2/config", handler.ConfigV2)
	server := testutil.NewTestServer(r)
	t.Cleanup(server.Close)

	return server, pair.AccessToken, db
}

func setupRelayEntryTest(t *testing.T, upstream http.HandlerFunc) (*testutil.TestServer, string, *gorm.DB) {
	t.Helper()
	server, token, db := setupRelayTest(t, upstream)
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("models", "").Error; err != nil {
		t.Fatalf("clear legacy models: %v", err)
	}
	maxTokens := 2048
	temperature := 0.3
	entries := []database.RelayModel{
		{
			ProviderID:     1,
			ModelID:        "gpt-rich",
			DisplayName:    "Rich Chat",
			Description:    "chat model",
			Category:       relay.CategoryChat,
			Capabilities:   relay.EncodeCapabilities(relay.ModelCapabilities{Vision: true, Tools: true}),
			AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{MaxTokens: &maxTokens, Temperature: &temperature}),
			Enabled:        true,
		},
		{
			ProviderID:     1,
			ModelID:        "whisper-test",
			DisplayName:    "Whisper Test",
			Category:       relay.CategorySpeech,
			Capabilities:   relay.EncodeCapabilities(relay.ModelCapabilities{}),
			AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{}),
			Enabled:        true,
		},
		{
			ProviderID:     1,
			ModelID:        "image-test",
			DisplayName:    "Image Test",
			Category:       relay.CategoryImageGeneration,
			Capabilities:   relay.EncodeCapabilities(relay.ModelCapabilities{}),
			AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{}),
			Enabled:        true,
		},
	}
	if err := db.Create(&entries).Error; err != nil {
		t.Fatalf("create relay models: %v", err)
	}
	return server, token, db
}

func authedRequest(t *testing.T, method, target, token, contentType string, body io.Reader) *http.Request {
	t.Helper()
	req := testutil.NewRequest(t, method, target, body)
	testutil.SetBearer(req, token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
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
	req := authedRequest(t, http.MethodGet, server.URL+"/relay/models", token, "", nil)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
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
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if seenAuth != "Bearer upstream-secret" {
		t.Fatalf("upstream auth = %q", seenAuth)
	}
	if _, ok := seenBody["api_type"]; ok {
		t.Fatal("api_type was forwarded upstream")
	}
}

func TestChatRequiresProviderIDForAmbiguousModel(t *testing.T) {
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called without provider_id")
	})
	provider := database.RelayProvider{
		Name:      "second upstream",
		Endpoint:  "https://second.invalid",
		APIKey:    "second-secret",
		APIFormat: relay.APIFormatOpenAI,
		Enabled:   true,
	}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatalf("create second provider: %v", err)
	}
	entry := database.RelayModel{
		ProviderID:     provider.ID,
		ModelID:        "gpt-rich",
		Category:       relay.CategoryChat,
		Capabilities:   relay.EncodeCapabilities(relay.ModelCapabilities{}),
		AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{}),
		Enabled:        true,
	}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatalf("create duplicate model: %v", err)
	}

	body := []byte(`{"model":"gpt-rich","api_type":"openai","messages":[{"role":"user","content":"ping"}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, raw)
	}
}

func TestChatRoutesByProviderIDAndStripsIt(t *testing.T) {
	var seenBody map[string]interface{}
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("first upstream should not be selected")
	})
	secondUpstream := testutil.NewTestServerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	})
	t.Cleanup(secondUpstream.Close)
	provider := database.RelayProvider{
		Name:      "selected upstream",
		Endpoint:  secondUpstream.URL,
		APIKey:    "selected-secret",
		APIFormat: relay.APIFormatOpenAI,
		Enabled:   true,
	}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatalf("create selected provider: %v", err)
	}
	entry := database.RelayModel{
		ProviderID:     provider.ID,
		ModelID:        "gpt-rich",
		Category:       relay.CategoryChat,
		Capabilities:   relay.EncodeCapabilities(relay.ModelCapabilities{}),
		AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{}),
		Enabled:        true,
	}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatalf("create selected model: %v", err)
	}

	body := []byte(fmt.Sprintf(`{"model":"gpt-rich","api_type":"openai","provider_id":"%d","messages":[{"role":"user","content":"ping"}]}`, provider.ID))
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if _, ok := seenBody["provider_id"]; ok {
		t.Fatal("provider_id was forwarded upstream")
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
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	raw := testutil.ReadAll(t, resp.Body)
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

	body := []byte(`{"model":"gpt-test","api_type":"unsupported","messages":[{"role":"user","content":"ping"}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
	}
}

func TestMessagesForwardsAnthropic(t *testing.T) {
	var seenKey, seenVersion string
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Fatalf("upstream path = %s, want /messages", r.URL.Path)
		}
		seenKey = r.Header.Get("x-api-key")
		seenVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"pong"}]}`))
	})
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("api_format", relay.APIFormatAnthropic).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}

	body := []byte(`{"model":"gpt-rich","messages":[{"role":"user","content":"ping"}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/messages", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if seenKey != "upstream-secret" || seenVersion == "" {
		t.Fatalf("anthropic headers = %q/%q", seenKey, seenVersion)
	}
}

func TestOllamaChatForwards(t *testing.T) {
	var seenAuth string
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Fatalf("upstream path = %s, want /api/chat", r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"content":"pong"},"done":true}`))
	})
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("api_format", relay.APIFormatOllama).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}

	body := []byte(`{"model":"gpt-rich","messages":[{"role":"user","content":"ping"}],"stream":false}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/api/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if seenAuth != "" {
		t.Fatalf("ollama auth = %q, want empty", seenAuth)
	}
}

func TestConfigReturnsRelayConfig(t *testing.T) {
	server, token, _ := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {})
	req := authedRequest(t, http.MethodGet, server.URL+"/relay/config", token, "", nil)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
	if payload["object"] != "relay_config" {
		t.Fatalf("object = %v, want relay_config", payload["object"])
	}
	providers := payload["data"].([]interface{})
	provider := providers[0].(map[string]interface{})
	models := provider["models"].([]interface{})
	if len(models) != 3 {
		t.Fatalf("model count = %d, want 3", len(models))
	}
	first := models[0].(map[string]interface{})
	if first["id"] != "gpt-rich" || first["category"] != relay.CategoryChat || first["providerName"] != "test upstream" {
		t.Fatalf("unexpected first model payload: %#v", first)
	}
	capabilities := first["capabilities"].(map[string]interface{})
	if capabilities["vision"] != true || capabilities["tools"] != true {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	params := first["advancedParams"].(map[string]interface{})
	if params["maxTokens"] != float64(2048) || params["temperature"] != 0.3 {
		t.Fatalf("advancedParams = %#v", params)
	}
}

func TestConfigV2SplitsProviderByCategory(t *testing.T) {
	server, token, _ := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {})
	req := authedRequest(t, http.MethodGet, server.URL+"/relay/v2/config", token, "", nil)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
	if payload["schemaVersion"] != float64(2) {
		t.Fatalf("schemaVersion = %v, want 2", payload["schemaVersion"])
	}
	providers := payload["data"].([]interface{})
	if len(providers) != 3 {
		t.Fatalf("provider count = %d, want 3 category-scoped providers", len(providers))
	}
	categories := map[string]bool{}
	for _, raw := range providers {
		provider := raw.(map[string]interface{})
		category := provider["category"].(string)
		categories[category] = true
		models := provider["models"].([]interface{})
		if len(models) != 1 {
			t.Fatalf("category %s model count = %d, want 1", category, len(models))
		}
		model := models[0].(map[string]interface{})
		if model["category"] != category || model["providerId"] != "1" {
			t.Fatalf("unexpected model payload for %s: %#v", category, model)
		}
	}
	for _, category := range []string{relay.CategoryChat, relay.CategorySpeech, relay.CategoryImageGeneration} {
		if !categories[category] {
			t.Fatalf("missing category %s", category)
		}
	}
}

func TestChatV2RoutesAnthropicAndStripsRelayFields(t *testing.T) {
	var seenBody map[string]interface{}
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Fatalf("upstream path = %s, want /messages", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"pong"}]}`))
	})
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("api_format", relay.APIFormatAnthropic).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}

	body := []byte(`{"model":"gpt-rich","api_type":"anthropic","provider_id":"1","messages":[{"role":"user","content":"ping"}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/v2/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if _, ok := seenBody["api_type"]; ok {
		t.Fatal("api_type was forwarded upstream")
	}
	if _, ok := seenBody["provider_id"]; ok {
		t.Fatal("provider_id was forwarded upstream")
	}
	if seenBody["model"] != "gpt-rich" {
		t.Fatalf("upstream model = %v", seenBody["model"])
	}
	if seenBody["max_tokens"] != float64(2048) || seenBody["temperature"] != 0.3 {
		t.Fatalf("model defaults were not forwarded: %#v", seenBody)
	}
}

func TestConfigReturnsVivoAppID(t *testing.T) {
	appID := "vivo-app-id"
	server, token, db := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Updates(map[string]interface{}{
		"models":     "",
		"api_format": relay.APIFormatVivoOCR,
	}).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}
	entry := database.RelayModel{
		ProviderID:     1,
		ModelID:        "general_recognition",
		DisplayName:    "VIVO OCR",
		Category:       relay.CategoryOCR,
		Capabilities:   relay.EncodeCapabilities(relay.ModelCapabilities{}),
		AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{User: &appID}),
		Enabled:        true,
	}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatalf("create relay model: %v", err)
	}

	req := authedRequest(t, http.MethodGet, server.URL+"/relay/config", token, "", nil)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
	providers := payload["data"].([]interface{})
	provider := providers[0].(map[string]interface{})
	models := provider["models"].([]interface{})
	model := models[0].(map[string]interface{})
	params := model["advancedParams"].(map[string]interface{})
	if params["appId"] != appID {
		t.Fatalf("advancedParams.appId = %v, want %s", params["appId"], appID)
	}
	if _, ok := params["user"]; ok {
		t.Fatalf("advancedParams leaked user for AppID: %#v", params)
	}
}

func TestApplyModelDefaultsPreservesClientValues(t *testing.T) {
	maxTokens := 2048
	temperature := 0.3
	topP := 0.8
	model := database.RelayModel{AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{
		MaxTokens: &maxTokens, Temperature: &temperature, TopP: &topP, Stop: []string{"END", "STOP"},
	})}
	raw, err := relay.ApplyModelDefaults(relay.APIFormatOpenAI, []byte(`{"model":"gpt-test","temperature":0.9}`), model)
	if err != nil {
		t.Fatalf("apply defaults: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode defaults: %v", err)
	}
	if body["temperature"] != 0.9 || body["max_tokens"] != float64(2048) || body["top_p"] != 0.8 {
		t.Fatalf("defaults = %#v", body)
	}
	stop := body["stop"].([]interface{})
	if len(stop) != 2 || stop[0] != "END" {
		t.Fatalf("stop = %#v", stop)
	}
}

func TestApplyOllamaDefaultsAndLegacyStop(t *testing.T) {
	params := relay.DecodeAdvancedParams(`{"maxTokens":512,"temperature":0.4,"stop":"END"}`)
	if len(params.Stop) != 1 || params.Stop[0] != "END" {
		t.Fatalf("legacy stop = %#v", params.Stop)
	}
	model := database.RelayModel{AdvancedParams: relay.EncodeAdvancedParams(params)}
	raw, err := relay.ApplyModelDefaults(relay.APIFormatOllama, []byte(`{"model":"qwen","options":{"temperature":0.7}}`), model)
	if err != nil {
		t.Fatalf("apply defaults: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode defaults: %v", err)
	}
	options := body["options"].(map[string]interface{})
	if options["temperature"] != 0.7 || options["num_predict"] != float64(512) {
		t.Fatalf("options = %#v", options)
	}
}

func TestRelayLoggingRecordsUserAndSkipsConfig(t *testing.T) {
	server, token, db := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	})
	body := []byte(`{"model":"gpt-test","api_type":"openai","messages":[{"role":"user","content":"private prompt"}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	resp.Body.Close()

	configReq := authedRequest(t, http.MethodGet, server.URL+"/relay/config", token, "", nil)
	configResp := testutil.Do(t, configReq)
	configResp.Body.Close()

	var logs []database.RelayRequestLog
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		logs = nil
		if err := db.Find(&logs).Error; err != nil {
			t.Fatalf("list logs: %v", err)
		}
		if len(logs) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(logs) != 1 {
		t.Fatalf("log count = %d, want 1", len(logs))
	}
	entry := logs[0]
	if entry.Username != "tester" || entry.UserID == 0 || entry.ProviderName != "test upstream" || entry.ModelID != "gpt-test" {
		t.Fatalf("unexpected log identity: %#v", entry)
	}
	if entry.Operation != "chat" || entry.Protocol != "v1" || entry.HTTPStatus != http.StatusOK || entry.UpstreamStatus != http.StatusOK {
		t.Fatalf("unexpected call metadata: %#v", entry)
	}
	if entry.RequestBytes != int64(len(body)) {
		t.Fatalf("request bytes = %d, want %d", entry.RequestBytes, len(body))
	}
}

func TestRelayLogDashboardAndRetention(t *testing.T) {
	_, _, db := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {})
	logs := relay.NewLogService(db)
	now := time.Now()
	entries := []database.RelayRequestLog{
		{UserID: 1, Username: "alice", Operation: "chat", Route: "/relay/v2/chat", Protocol: "v2", HTTPStatus: 200, DurationMS: 100, RequestBytes: 10, ResponseBytes: 20, CreatedAt: now.Add(-time.Hour)},
		{UserID: 1, Username: "alice", Operation: "chat", Route: "/relay/v2/chat", Protocol: "v2", HTTPStatus: 500, DurationMS: 300, RequestBytes: 30, ResponseBytes: 40, CreatedAt: now.Add(-2 * time.Hour)},
		{UserID: 2, Username: "bob", Operation: "ocr", Route: "/relay/v2/ocr", Protocol: "v2", HTTPStatus: 200, DurationMS: 50, CreatedAt: now.Add(-8 * 24 * time.Hour)},
	}
	if err := db.Create(&entries).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}
	dashboard, err := logs.Dashboard("7d", now)
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if dashboard.Summary.Total != 2 || dashboard.Summary.Success != 1 || dashboard.Summary.Failed != 1 || len(dashboard.Users) != 1 || dashboard.Users[0].Username != "alice" {
		t.Fatalf("unexpected dashboard: %#v", dashboard)
	}
	if len(dashboard.Trend) == 0 {
		t.Fatal("dashboard trend is empty")
	}
	if err := logs.DeleteExpired(now); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	var count int64
	db.Model(&database.RelayRequestLog{}).Count(&count)
	if count != 2 {
		t.Fatalf("retained logs = %d, want 2", count)
	}
}

func TestModelsReturnsRichPayloadFromRelayModels(t *testing.T) {
	server, token, _ := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {})
	req := authedRequest(t, http.MethodGet, server.URL+"/relay/models", token, "", nil)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
	models := payload["data"].([]interface{})
	first := models[0].(map[string]interface{})
	if first["displayName"] != "Rich Chat" || first["api_type"] != relay.APIFormatOpenAI || first["providerId"] != "1" {
		t.Fatalf("unexpected model payload: %#v", first)
	}
}

func TestChatRejectsSpeechModel(t *testing.T) {
	server, token, _ := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called")
	})
	body := []byte(`{"model":"whisper-test","api_type":"openai","messages":[{"role":"user","content":"ping"}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
	}
}

func TestTranscribeForwardsSpeechModel(t *testing.T) {
	var seenAuth, seenModel, seenAPIType string
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Fatalf("upstream path = %s, want /audio/transcriptions", r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse upstream multipart: %v", err)
		}
		seenModel = r.FormValue("model")
		seenAPIType = r.FormValue("api_type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	})
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("api_format", relay.APIFormatOpenAISpeech).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}

	body, contentType := multipartBody(t, map[string]string{"model": "whisper-test", "api_type": "openai_speech", "response_format": "json"})
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/transcribe", token, contentType, body)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if seenAuth != "Bearer upstream-secret" || seenModel != "whisper-test" || seenAPIType != "" {
		t.Fatalf("auth/model/api_type = %q/%q/%q", seenAuth, seenModel, seenAPIType)
	}
}

func TestImageGenerationsForwardsImageModel(t *testing.T) {
	var seenBody map[string]interface{}
	server, token, _ := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/generations" {
			t.Fatalf("upstream path = %s, want /images/generations", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://example.com/a.png"}]}`))
	})
	body := []byte(`{"model":"image-test","api_type":"openai","prompt":"cat"}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/images/generations", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if _, ok := seenBody["api_type"]; ok {
		t.Fatal("api_type was forwarded upstream")
	}
	if seenBody["model"] != "image-test" || seenBody["prompt"] != "cat" {
		t.Fatalf("upstream body = %#v", seenBody)
	}
}

func multipartBody(t *testing.T, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for key, value := range fields {
		if err := w.WriteField(key, value); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	part, err := w.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err := part.Write([]byte("audio")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, w.FormDataContentType()
}
