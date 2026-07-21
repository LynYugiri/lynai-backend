package relay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
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

	provider := database.RelayProvider{
		ID:        1,
		Name:      "test upstream",
		Endpoint:  upstreamServer.URL,
		APIKey:    "upstream-secret",
		APIFormat: relay.APIFormatOpenAI,
		Enabled:   true,
	}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	entries := []database.RelayModel{
		{ProviderID: 1, ModelID: "gpt-test", Category: relay.CategoryChat, Capabilities: relay.EncodeCapabilities(relay.ModelCapabilities{Thinking: true, Tools: true}), AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{}), Enabled: true},
		{ProviderID: 1, ModelID: "gpt-stream", Category: relay.CategoryChat, Capabilities: relay.EncodeCapabilities(relay.ModelCapabilities{Thinking: true, Tools: true}), AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{}), Enabled: true},
	}
	if err := db.Create(&entries).Error; err != nil {
		t.Fatalf("create relay models: %v", err)
	}

	jwtMgr := auth.NewJWTManager("test-secret")
	authSvc := auth.NewService(db, jwtMgr, database.NewSnowflakeGenerator(0))
	_, pair, err := authSvc.Register(context.Background(), "13800000000", "secret123", "tester")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	handler := relay.NewHandler(relay.NewServiceWithClient(db, testutil.NewHTTPClient()))
	t.Cleanup(handler.Close)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/relay")
	g.Use(auth.AuthMiddleware(jwtMgr), handler.LoggingMiddleware())
	g.POST("/chat", handler.Chat)
	g.POST("/transcribe", handler.Transcribe)
	g.POST("/ocr", handler.OCR)
	g.POST("/images/generations", handler.ImageGenerations)
	g.POST("/speech/create", handler.SpeechCreate)
	g.POST("/speech/:audioId/upload", handler.SpeechUpload)
	g.POST("/speech/:audioId/run", handler.SpeechRun)
	g.GET("/speech/:audioId/progress", handler.SpeechProgress)
	g.GET("/speech/:audioId/result", handler.SpeechResult)
	g.GET("/config", handler.Config)
	server := testutil.NewTestServer(r)
	t.Cleanup(server.Close)

	return server, pair.AccessToken, db
}

func setupRelayEntryTest(t *testing.T, upstream http.HandlerFunc) (*testutil.TestServer, string, *gorm.DB) {
	t.Helper()
	server, token, db := setupRelayTest(t, upstream)
	if err := db.Where("provider_id = ?", 1).Delete(&database.RelayModel{}).Error; err != nil {
		t.Fatalf("clear models: %v", err)
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

func TestSpeechCreateBodyLimit(t *testing.T) {
	server, token, _ := setupRelayTest(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("oversized speech create reached upstream")
	})
	body := strings.NewReader(`{"model":"gpt-test","padding":"` + strings.Repeat("x", 20<<10) + `"}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/speech/create", token, "application/json", body)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("speech create status = %d, want 413", resp.StatusCode)
	}
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

func TestChatCanonicalOpenAIForwardsAndNormalizes(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"pong","reasoning_content":"think"}}]}`))
	})

	body := []byte(`{"providerId":"1","model":"gpt-test","messages":[{"role":"system","content":[{"type":"text","text":"rules"}]},{"role":"user","content":[{"type":"text","text":"ping"}]}],"stream":false,"reasoning":{"enabled":true}}`)
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
	if seenBody["model"] != "gpt-test" || seenBody["stream"] != false {
		t.Fatalf("upstream body = %#v", seenBody)
	}
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
	message := payload["message"].(map[string]interface{})
	if message["content"] != "pong" || message["reasoning"] != "think" || payload["finishReason"] != "stop" {
		t.Fatalf("canonical response = %#v", payload)
	}
}

func TestChatCanonicalSSE(t *testing.T) {
	server, token, _ := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"one\",\"reasoning_content\":\"why\"}}]}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\ndata: [DONE]\n\n"))
		flusher.Flush()
	})

	body := []byte(`{"providerId":"1","model":"gpt-stream","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	raw := testutil.ReadAll(t, resp.Body)
	if got := string(raw); !strings.Contains(got, `"content":"one"`) || !strings.Contains(got, `"reasoning":"why"`) || !strings.Contains(got, `"done":true`) {
		t.Fatalf("stream body = %q", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestChatCanonicalOpenAITools(t *testing.T) {
	var seenBody map[string]interface{}
	server, token, _ := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Shanghai\"}"}}]}}]}`))
	})
	body := []byte(`{
		"providerId":"1",
		"model":"gpt-test",
		"messages":[{"role":"user","content":[{"type":"text","text":"weather?"}]}],
		"tools":[{"name":"weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
		"toolChoice":"auto"
	}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, testutil.ReadAll(t, resp.Body))
	}
	tools, ok := seenBody["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("upstream tools = %#v", seenBody["tools"])
	}
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
	calls := payload["message"].(map[string]interface{})["toolCalls"].([]interface{})
	call := calls[0].(map[string]interface{})
	if call["id"] != "call-1" || call["name"] != "weather" || call["arguments"].(map[string]interface{})["city"] != "Shanghai" {
		t.Fatalf("canonical tool call = %#v", call)
	}
}

func TestRemovedRelayRoutesReturnNotFound(t *testing.T) {
	server, token, _ := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("removed route reached upstream")
	})
	for _, target := range []string{"/relay/v2/config", "/relay/v2/chat", "/relay/messages", "/relay/api/chat", "/relay/models"} {
		req := authedRequest(t, http.MethodGet, server.URL+target, token, "", nil)
		resp := testutil.Do(t, req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", target, resp.StatusCode)
		}
	}
}

func TestChatRejectsLegacyAPIType(t *testing.T) {
	server, token, _ := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called")
	})

	body := []byte(`{"providerId":"1","model":"gpt-test","api_type":"openai","messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
	}
}

func TestChatCanonicalAnthropic(t *testing.T) {
	var seenKey, seenVersion string
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Fatalf("upstream path = %s, want /messages", r.URL.Path)
		}
		seenKey = r.Header.Get("x-api-key")
		seenVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stop_reason":"tool_use","content":[{"type":"thinking","thinking":"why"},{"type":"text","text":"pong"},{"type":"tool_use","id":"call-a","name":"weather","input":{"city":"Shanghai"}}]}`))
	})
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("api_format", relay.APIFormatAnthropic).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}

	body := []byte(`{"providerId":"1","model":"gpt-rich","messages":[{"role":"system","content":[{"type":"text","text":"rules"}]},{"role":"user","content":[{"type":"text","text":"ping"}]}],"tools":[{"name":"weather","parameters":{"type":"object"}}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if seenKey != "upstream-secret" || seenVersion == "" {
		t.Fatalf("anthropic headers = %q/%q", seenKey, seenVersion)
	}
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
	message := payload["message"].(map[string]interface{})
	if message["content"] != "pong" || message["toolCalls"].([]interface{})[0].(map[string]interface{})["name"] != "weather" {
		t.Fatalf("canonical response = %#v", payload)
	}
}

func TestChatCanonicalOllama(t *testing.T) {
	var seenAuth string
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Fatalf("upstream path = %s, want /api/chat", r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"content":"pong","thinking":"why","tool_calls":[{"function":{"name":"weather","arguments":{"city":"Shanghai"}}}]},"done":true,"done_reason":"stop"}`))
	})
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("api_format", relay.APIFormatOllama).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}

	body := []byte(`{"providerId":"1","model":"gpt-rich","messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}],"tools":[{"name":"weather","parameters":{"type":"object"}}],"stream":false}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if seenAuth != "" {
		t.Fatalf("ollama auth = %q, want empty", seenAuth)
	}
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
	if payload["message"].(map[string]interface{})["toolCalls"].([]interface{})[0].(map[string]interface{})["name"] != "weather" {
		t.Fatalf("canonical Ollama response = %#v", payload)
	}
}

func TestChatRejectsImageWhenModelDoesNotSupportVision(t *testing.T) {
	server, token, _ := setupRelayTest(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream should not be called")
	})
	body := []byte(`{"providerId":"1","model":"gpt-test","messages":[{"role":"user","content":[{"type":"inputFile","file":{"name":"pixel.png","mimeType":"image/png","dataBase64":"aW1hZ2U="}}]}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, testutil.ReadAll(t, resp.Body))
	}
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
	errorPayload := payload["error"].(map[string]interface{})
	if errorPayload["type"] != "unsupported_feature" {
		t.Fatalf("error = %#v", errorPayload)
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
	if payload["schemaVersion"] != float64(3) || provider["providerId"] != "1" {
		t.Fatalf("config envelope = %#v", payload)
	}
	if _, ok := provider["endpoint"]; ok {
		t.Fatal("config leaked endpoint")
	}
	if _, ok := provider["apiType"]; ok {
		t.Fatal("config leaked apiType")
	}
	models := provider["models"].([]interface{})
	if len(models) != 3 {
		t.Fatalf("model count = %d, want 3", len(models))
	}
	first := models[0].(map[string]interface{})
	if first["id"] != "gpt-rich" || first["category"] != relay.CategoryChat {
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
	missingCapabilities := models[1].(map[string]interface{})["capabilities"].(map[string]interface{})
	if missingCapabilities["vision"] != false || missingCapabilities["thinking"] != false || missingCapabilities["tools"] != false {
		t.Fatalf("missing capabilities did not default false: %#v", missingCapabilities)
	}
}

func TestConfigReturnsVivoAppID(t *testing.T) {
	appID := "vivo-app-id"
	server, token, db := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := db.Where("provider_id = ?", 1).Delete(&database.RelayModel{}).Error; err != nil {
		t.Fatalf("delete models: %v", err)
	}
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("api_format", relay.APIFormatVivoOCR).Error; err != nil {
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

func TestConfigReturnsVivoLASRWorkflow(t *testing.T) {
	server, token, db := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := db.Where("provider_id = ?", 1).Delete(&database.RelayModel{}).Error; err != nil {
		t.Fatalf("delete models: %v", err)
	}
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("api_format", relay.APIFormatVivoLASR).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}
	entry := database.RelayModel{
		ProviderID:     1,
		ModelID:        "vivo-lasr",
		DisplayName:    "VIVO LASR",
		Category:       relay.CategorySpeech,
		Capabilities:   relay.EncodeCapabilities(relay.ModelCapabilities{}),
		AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{}),
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
	if model["workflow"] != relay.APIFormatVivoLASR {
		t.Fatalf("workflow = %v, want %s", model["workflow"], relay.APIFormatVivoLASR)
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
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"pong"}}]}`))
	})
	body := []byte(`{"providerId":"1","model":"gpt-test","messages":[{"role":"user","content":[{"type":"text","text":"private prompt"}]}]}`)
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
	if entry.Operation != "chat" || entry.Protocol != "canonical" || entry.HTTPStatus != http.StatusOK || entry.UpstreamStatus != http.StatusOK {
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
		{UserID: 1, Username: "alice", Operation: "chat", Route: "/relay/chat", Protocol: "canonical", HTTPStatus: 200, DurationMS: 100, RequestBytes: 10, ResponseBytes: 20, CreatedAt: now.Add(-time.Hour)},
		{UserID: 1, Username: "alice", Operation: "chat", Route: "/relay/chat", Protocol: "canonical", HTTPStatus: 500, DurationMS: 300, RequestBytes: 30, ResponseBytes: 40, CreatedAt: now.Add(-2 * time.Hour)},
		{UserID: 2, Username: "bob", Operation: "ocr", Route: "/relay/ocr", Protocol: "canonical", HTTPStatus: 200, DurationMS: 50, CreatedAt: now.Add(-8 * 24 * time.Hour)},
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

func TestConfigReturnsRichPayloadFromRelayModels(t *testing.T) {
	server, token, _ := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {})
	req := authedRequest(t, http.MethodGet, server.URL+"/relay/config", token, "", nil)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	var payload map[string]interface{}
	testutil.DecodeJSON(t, resp, &payload)
	provider := payload["data"].([]interface{})[0].(map[string]interface{})
	models := provider["models"].([]interface{})
	first := models[0].(map[string]interface{})
	if first["displayName"] != "Rich Chat" {
		t.Fatalf("unexpected model payload: %#v", first)
	}
}

func TestChatRejectsSpeechModel(t *testing.T) {
	server, token, _ := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called")
	})
	body := []byte(`{"providerId":"1","model":"whisper-test","messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
	}
}

func TestTranscribeForwardsSpeechModel(t *testing.T) {
	var seenAuth, seenModel, seenProviderID string
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Fatalf("upstream path = %s, want /audio/transcriptions", r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse upstream multipart: %v", err)
		}
		seenModel = r.FormValue("model")
		seenProviderID = r.FormValue("providerId")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	})
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("api_format", relay.APIFormatOpenAISpeech).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}

	body, contentType := multipartBody(t, map[string]string{"providerId": "1", "model": "whisper-test", "response_format": "json"})
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/transcribe", token, contentType, body)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if seenAuth != "Bearer upstream-secret" || seenModel != "whisper-test" || seenProviderID != "" {
		t.Fatalf("auth/model/providerId = %q/%q/%q", seenAuth, seenModel, seenProviderID)
	}
}

func TestMultipartRequestRejectsTotalSizeOverLimit(t *testing.T) {
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called for oversized multipart request")
	})
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Update("api_format", relay.APIFormatOpenAISpeech).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}

	body, contentType := multipartBodyWithFile(t, map[string]string{"providerId": "1", "model": "whisper-test"}, bytes.Repeat([]byte("a"), (8<<20)+1))
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/transcribe", token, contentType, body)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
	}
}

func TestNonStreamingUpstreamResponseRejectsOversize(t *testing.T) {
	server, token, _ := setupRelayTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte("a"), (16<<20)+1))
	})
	body := []byte(`{"providerId":"1","model":"gpt-test","messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/chat", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, raw)
	}
}

func TestSpeechSessionIsUserScopedAndMalformedResultCanRetry(t *testing.T) {
	var mu sync.Mutex
	paths := make([]string, 0, 5)
	resultCalls := 0
	server, token, db := setupRelayEntryTest(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/lasr/create":
			_, _ = w.Write([]byte(`{"data":{"audio_id":"upstream-audio"}}`))
		case "/lasr/run":
			_, _ = w.Write([]byte(`{"data":{"task_id":"upstream-task"}}`))
		case "/lasr/result":
			resultCalls++
			if resultCalls == 1 {
				_, _ = w.Write([]byte(`{"data":`))
				return
			}
			if resultCalls == 2 {
				_, _ = w.Write([]byte(`{"data":{"result":"malformed"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"result":[{"onebest":"hello"}]}}`))
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	})
	if err := db.Model(&database.RelayProvider{}).Where("id = ?", 1).Updates(map[string]interface{}{
		"api_format": relay.APIFormatVivoLASR,
		"config":     relay.EncodeProviderConfig(relay.ProviderConfig{AppID: "test-app"}),
	}).Error; err != nil {
		t.Fatalf("update provider: %v", err)
	}
	if err := db.Model(&database.RelayModel{}).Where("model_id = ?", "whisper-test").Update("enabled", false).Error; err != nil {
		t.Fatalf("disable old speech model: %v", err)
	}
	model := database.RelayModel{ProviderID: 1, ModelID: "vivo-speech", Category: relay.CategorySpeech, Enabled: true}
	if err := db.Create(&model).Error; err != nil {
		t.Fatalf("create speech model: %v", err)
	}

	createBody := bytes.NewBufferString(`{"providerId":"1","model":"vivo-speech","audio_type":"wav","slice_num":1}`)
	createReq := authedRequest(t, http.MethodPost, server.URL+"/relay/speech/create", token, "application/json", createBody)
	createResp := testutil.Do(t, createReq)
	defer createResp.Body.Close()
	var created struct {
		Data struct {
			AudioID string `json:"audio_id"`
		} `json:"data"`
	}
	testutil.DecodeJSON(t, createResp, &created)
	if createResp.StatusCode != http.StatusOK || len(created.Data.AudioID) != 32 {
		t.Fatalf("create status/session = %d/%q", createResp.StatusCode, created.Data.AudioID)
	}

	jwtMgr := auth.NewJWTManager("test-secret")
	otherToken, _, err := jwtMgr.GenerateAccessToken("999999", "other", false)
	if err != nil {
		t.Fatalf("create other user token: %v", err)
	}
	otherReq := authedRequest(t, http.MethodGet, server.URL+"/relay/speech/"+created.Data.AudioID+"/progress", otherToken, "", nil)
	otherResp := testutil.Do(t, otherReq)
	otherResp.Body.Close()
	if otherResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user status = %d, want 404", otherResp.StatusCode)
	}

	runReq := authedRequest(t, http.MethodPost, server.URL+"/relay/speech/"+created.Data.AudioID+"/run", token, "application/json", nil)
	runResp := testutil.Do(t, runReq)
	runResp.Body.Close()
	if runResp.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d, want 200", runResp.StatusCode)
	}
	resultURL := server.URL + "/relay/speech/" + created.Data.AudioID + "/result"
	resultReq := authedRequest(t, http.MethodGet, resultURL, token, "", nil)
	resultResp := testutil.Do(t, resultReq)
	resultResp.Body.Close()
	if resultResp.StatusCode != http.StatusOK {
		t.Fatalf("result status = %d, want 200", resultResp.StatusCode)
	}
	secondReq := authedRequest(t, http.MethodGet, resultURL, token, "", nil)
	secondResp := testutil.Do(t, secondReq)
	secondResp.Body.Close()
	if secondResp.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", secondResp.StatusCode)
	}
	thirdReq := authedRequest(t, http.MethodGet, resultURL, token, "", nil)
	thirdResp := testutil.Do(t, thirdReq)
	thirdResp.Body.Close()
	if thirdResp.StatusCode != http.StatusOK {
		t.Fatalf("valid result status = %d, want 200", thirdResp.StatusCode)
	}
	fourthReq := authedRequest(t, http.MethodGet, resultURL, token, "", nil)
	fourthResp := testutil.Do(t, fourthReq)
	fourthResp.Body.Close()
	if fourthResp.StatusCode != http.StatusNotFound {
		t.Fatalf("completed session status = %d, want 404", fourthResp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 5 {
		t.Fatalf("upstream calls = %v, want create/run/result/result/result", paths)
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
	body := []byte(`{"providerId":"1","model":"image-test","prompt":"cat"}`)
	req := authedRequest(t, http.MethodPost, server.URL+"/relay/images/generations", token, "application/json", bytes.NewReader(body))
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := testutil.ReadAll(t, resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if _, ok := seenBody["providerId"]; ok {
		t.Fatal("providerId was forwarded upstream")
	}
	if seenBody["model"] != "image-test" || seenBody["prompt"] != "cat" {
		t.Fatalf("upstream body = %#v", seenBody)
	}
}

func multipartBody(t *testing.T, fields map[string]string) (*bytes.Buffer, string) {
	return multipartBodyWithFile(t, fields, []byte("audio"))
}

func multipartBodyWithFile(t *testing.T, fields map[string]string, file []byte) (*bytes.Buffer, string) {
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
	if _, err := part.Write(file); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, w.FormDataContentType()
}
