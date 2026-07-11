package relay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
)

const maxRelayBodyBytes = 8 << 20

// Handler serves authenticated relay endpoints.
type Handler struct {
	svc      *Service
	speechMu sync.Mutex
	speech   map[string]*speechSession
}

// Messages forwards an Anthropic-compatible messages request.
func (h *Handler) Messages(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes))
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is too large or unreadable")
		return
	}
	forwardBody, model, providerID, stream, err := prepareRoutedBody(body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	resolved, err := h.svc.Resolve(APIFormatAnthropic, model, providerID)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if resolved.Model.Category != "" && resolved.Model.Category != CategoryChat && resolved.Model.Category != CategoryOCR {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a chat or OCR model")
		return
	}
	forwardBody, err = ApplyModelDefaults(APIFormatAnthropic, forwardBody, resolved.Model)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
		return
	}
	resp, err := h.svc.ForwardAnthropicMessages(c.Request.Context(), &resolved.Provider, forwardBody)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	writeUpstreamResponse(c, resp, stream)
}

// OllamaChat forwards an Ollama chat request.
func (h *Handler) OllamaChat(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes))
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is too large or unreadable")
		return
	}
	forwardBody, model, providerID, stream, err := prepareRoutedBody(body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	resolved, err := h.svc.Resolve(APIFormatOllama, model, providerID)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if resolved.Model.Category != "" && resolved.Model.Category != CategoryChat && resolved.Model.Category != CategoryOCR {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a chat or OCR model")
		return
	}
	forwardBody, err = ApplyModelDefaults(APIFormatOllama, forwardBody, resolved.Model)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
		return
	}
	resp, err := h.svc.ForwardOllamaChat(c.Request.Context(), &resolved.Provider, forwardBody)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	writeUpstreamResponse(c, resp, stream)
}

// NewHandler creates a relay handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc, speech: map[string]*speechSession{}}
}

type speechSession struct {
	Provider        database.RelayProvider
	Model           database.RelayModel
	AppID           string
	UpstreamAudioID string
	TaskID          string
	CreatedAt       time.Time
}

// Chat forwards an OpenAI-compatible chat request to an admin-managed upstream.
func (h *Handler) Chat(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes))
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is too large or unreadable")
		return
	}

	forwardBody, apiType, model, providerID, stream, err := prepareForwardBody(body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	resolved, err := h.svc.Resolve(apiType, model, providerID)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}

	if resolved.Model.Category != "" && resolved.Model.Category != CategoryChat && resolved.Model.Category != CategoryOCR {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a chat or OCR model")
		return
	}
	forwardBody, err = ApplyModelDefaults(resolved.Provider.APIFormat, forwardBody, resolved.Model)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
		return
	}
	if normalizeAPIType(resolved.Provider.APIFormat) != APIFormatOpenAI {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "use the relay endpoint matching the requested api_type")
		return
	}

	resp, err := h.svc.ForwardChat(c.Request.Context(), &resolved.Provider, forwardBody)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(c, resp.Header)
	c.Status(resp.StatusCode)
	if stream || strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		streamCopy(c, resp.Body)
		return
	}
	_, _ = io.Copy(c.Writer, resp.Body)
}

// ChatV2 exposes one managed chat endpoint regardless of the upstream API
// format. The request and response payload remain native to the configured
// provider so existing protocol adapters can be reused by the client.
func (h *Handler) ChatV2(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes))
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is too large or unreadable")
		return
	}
	forwardBody, apiType, model, providerID, stream, err := prepareForwardBody(body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	resolved, err := h.svc.Resolve(apiType, model, providerID)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if resolved.Model.Category != "" && resolved.Model.Category != CategoryChat && resolved.Model.Category != CategoryOCR {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a chat or OCR model")
		return
	}
	forwardBody, err = ApplyModelDefaults(resolved.Provider.APIFormat, forwardBody, resolved.Model)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
		return
	}

	var resp *http.Response
	switch normalizeAPIType(resolved.Provider.APIFormat) {
	case APIFormatOpenAI:
		resp, err = h.svc.ForwardChat(c.Request.Context(), &resolved.Provider, forwardBody)
	case APIFormatAnthropic:
		resp, err = h.svc.ForwardAnthropicMessages(c.Request.Context(), &resolved.Provider, forwardBody)
	case APIFormatOllama:
		resp, err = h.svc.ForwardOllamaChat(c.Request.Context(), &resolved.Provider, forwardBody)
	default:
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested provider does not support chat")
		return
	}
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	writeUpstreamResponse(c, resp, stream)
}

// Transcribe forwards an OpenAI-compatible audio transcription request.
func (h *Handler) Transcribe(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(maxRelayBodyBytes); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid multipart request")
		return
	}
	model := strings.TrimSpace(c.Request.FormValue("model"))
	apiType := normalizeAPIType(c.Request.FormValue("api_type"))
	providerID := relayProviderIDFromForm(c.Request)
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	resolved, err := h.svc.Resolve(apiType, model, providerID)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if resolved.Model.Category != CategorySpeech {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a speech-to-text model")
		return
	}
	if normalizeAPIType(resolved.Provider.APIFormat) != APIFormatOpenAISpeech {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested provider does not support OpenAI transcription")
		return
	}
	delete(c.Request.MultipartForm.Value, "api_type")
	delete(c.Request.MultipartForm.Value, "provider_id")
	delete(c.Request.MultipartForm.Value, "providerId")
	body, contentType, err := CloneMultipartForm(c.Request.MultipartForm)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "failed to prepare multipart request")
		return
	}
	resp, err := h.svc.ForwardMultipart(c.Request.Context(), &resolved.Provider, "/audio/transcriptions", body, contentType)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(c, resp.Header)
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}

// OCR forwards an image OCR request to a managed OCR or vision-chat upstream.
func (h *Handler) OCR(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(maxRelayBodyBytes); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid multipart request")
		return
	}
	model := strings.TrimSpace(c.Request.FormValue("model"))
	apiType := normalizeAPIType(c.Request.FormValue("api_type"))
	providerID := relayProviderIDFromForm(c.Request)
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	resolved, err := h.svc.Resolve(apiType, model, providerID)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "file is required")
		return
	}
	defer file.Close()
	image, err := io.ReadAll(file)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "failed to read file")
		return
	}
	if normalizeAPIType(resolved.Provider.APIFormat) == APIFormatVivoOCR {
		h.forwardVivoOCR(c, resolved, image)
		return
	}
	h.forwardVisionOCR(c, resolved, image)
}

// SpeechCreate starts a managed long-running speech transcription session.
func (h *Handler) SpeechCreate(c *gin.Context) {
	var body map[string]interface{}
	if err := c.BindJSON(&body); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	model := strings.TrimSpace(fmt.Sprint(body["model"]))
	apiType := normalizeAPIType(fmt.Sprint(body["api_type"]))
	providerID := relayProviderID(body)
	resolved, err := h.svc.Resolve(apiType, model, providerID)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if normalizeAPIType(resolved.Provider.APIFormat) != APIFormatVivoLASR {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "speech session is only supported for vivo_lasr")
		return
	}
	appID := relayProviderAppID(resolved.Provider, resolved.Model)
	if appID == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "vivo_lasr requires provider AppID")
		return
	}
	sessionID := strconv.FormatInt(time.Now().UnixNano(), 10)
	query := vivoSpeechQuery(resolved.Provider, appID, resolved.Model.ModelID)
	upstreamBody := map[string]interface{}{
		"audio_type":  fmt.Sprint(body["audio_type"]),
		"x-sessionId": sessionID,
		"slice_num":   body["slice_num"],
	}
	raw, ok := marshalRelayJSON(c, upstreamBody)
	if !ok {
		return
	}
	resp, err := h.svc.ForwardVivoJSON(c.Request.Context(), &resolved.Provider, "/lasr/create", query, raw)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	upstream, rawResp, err := decodeJSONResponse(resp)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", string(rawResp))
		return
	}
	upstreamAudioID, _ := nestedString(upstream, "data", "audio_id")
	if upstreamAudioID == "" {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "vivo_lasr create did not return audio_id")
		return
	}
	h.speechMu.Lock()
	h.speech[sessionID] = &speechSession{Provider: resolved.Provider, Model: resolved.Model, AppID: appID, UpstreamAudioID: upstreamAudioID, CreatedAt: time.Now()}
	h.speechMu.Unlock()
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"audio_id": sessionID}})
}

func (h *Handler) SpeechUpload(c *gin.Context) {
	session := h.loadSpeechSession(c)
	if session == nil {
		return
	}
	if err := c.Request.ParseMultipartForm(maxRelayBodyBytes); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid multipart request")
		return
	}
	body, contentType, err := CloneMultipartForm(c.Request.MultipartForm)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "failed to prepare multipart request")
		return
	}
	query := vivoSpeechQuery(session.Provider, session.AppID, session.Model.ModelID)
	query.Set("audio_id", session.UpstreamAudioID)
	query.Set("x-sessionId", c.Param("audioId"))
	query.Set("slice_index", c.DefaultQuery("slice_index", c.Request.FormValue("slice_index")))
	resp, err := h.svc.ForwardVivoMultipart(c.Request.Context(), &session.Provider, "/lasr/upload", query, body, contentType)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	writeUpstreamResponse(c, resp, false)
}

func (h *Handler) SpeechRun(c *gin.Context) {
	session := h.loadSpeechSession(c)
	if session == nil {
		return
	}
	raw, ok := marshalRelayJSON(c, gin.H{"audio_id": session.UpstreamAudioID, "x-sessionId": c.Param("audioId")})
	if !ok {
		return
	}
	resp, err := h.svc.ForwardVivoJSON(c.Request.Context(), &session.Provider, "/lasr/run", vivoSpeechQuery(session.Provider, session.AppID, session.Model.ModelID), raw)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	payload, rawResp, err := decodeJSONResponse(resp)
	if err == nil {
		if taskID, _ := nestedString(payload, "data", "task_id"); taskID != "" {
			h.speechMu.Lock()
			session.TaskID = taskID
			h.speechMu.Unlock()
		}
	}
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), rawResp)
}

func (h *Handler) SpeechProgress(c *gin.Context) {
	h.forwardSpeechTaskJSON(c, "/lasr/progress", false)
}

func (h *Handler) SpeechResult(c *gin.Context) {
	h.forwardSpeechTaskJSON(c, "/lasr/result", true)
}

// ImageGenerations forwards an OpenAI-compatible image generation request.
func (h *Handler) ImageGenerations(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes))
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is too large or unreadable")
		return
	}
	forwardBody, apiType, model, providerID, _, err := prepareForwardBody(body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	resolved, err := h.svc.Resolve(apiType, model, providerID)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if resolved.Model.Category != CategoryImageGeneration {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not an image generation model")
		return
	}
	forwardBody, err = ApplyModelDefaults(resolved.Provider.APIFormat, forwardBody, resolved.Model)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
		return
	}
	var resp *http.Response
	if normalizeAPIType(resolved.Provider.APIFormat) == APIFormatVivoImage {
		resp, err = h.svc.ForwardVivoImage(c.Request.Context(), &resolved.Provider, forwardBody)
	} else {
		resp, err = h.svc.ForwardJSON(c.Request.Context(), &resolved.Provider, "/images/generations", forwardBody)
	}
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	if normalizeAPIType(resolved.Provider.APIFormat) == APIFormatVivoImage {
		h.writeVivoImageResponse(c, resp)
		return
	}
	copyResponseHeaders(c, resp.Header)
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}

// Models returns enabled relay models in an OpenAI-compatible list with LynAI metadata.
func (h *Handler) Models(c *gin.Context) {
	providers, err := h.svc.ListEnabled()
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to list relay models")
		return
	}
	seen := map[string]struct{}{}
	data := make([]gin.H, 0)
	for _, provider := range providers {
		apiType := normalizeAPIType(provider.APIFormat)
		entries := provider.Entries
		if len(entries) == 0 {
			legacy, err := legacyEntries(provider)
			if err != nil {
				writeOpenAIError(c, http.StatusInternalServerError, "server_error", "invalid relay provider model list")
				return
			}
			entries = legacy
		}
		for _, entry := range entries {
			key := apiType + "\x00" + entry.ModelID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			data = append(data, modelPayload(provider, entry))
		}
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": data})
}

func (h *Handler) writeVivoImageResponse(c *gin.Context, resp *http.Response) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to read upstream response")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		copyResponseHeaders(c, resp.Header)
		c.Status(resp.StatusCode)
		_, _ = io.Copy(c.Writer, bytes.NewReader(raw))
		return
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "invalid vivo image response")
		return
	}
	if code, ok := payload["code"].(float64); ok && code != 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": string(raw), "type": "upstream_error"}})
		return
	}
	data := make([]gin.H, 0)
	if result, ok := payload["data"].(map[string]interface{}); ok {
		if images, ok := result["images"].([]interface{}); ok {
			for _, image := range images {
				if item, ok := image.(map[string]interface{}); ok {
					if u, ok := item["url"].(string); ok && u != "" {
						data = append(data, gin.H{"url": u})
					}
				}
			}
		}
		if u, ok := result["image"].(string); ok && u != "" && len(data) == 0 {
			data = append(data, gin.H{"url": u})
		}
	}
	c.JSON(http.StatusOK, gin.H{"created": 0, "data": data})
}

func writeUpstreamResponse(c *gin.Context, resp *http.Response, stream bool) {
	copyResponseHeaders(c, resp.Header)
	c.Status(resp.StatusCode)
	if stream || strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		streamCopy(c, resp.Body)
		return
	}
	_, _ = io.Copy(c.Writer, resp.Body)
}

// Config returns the full enabled relay configuration for managed frontend providers.
func (h *Handler) Config(c *gin.Context) {
	providers, err := h.svc.ListConfig()
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to list relay config")
		return
	}
	data := make([]gin.H, 0, len(providers))
	for _, provider := range providers {
		entries := provider.Entries
		if len(entries) == 0 {
			legacy, err := legacyEntries(provider)
			if err != nil {
				writeOpenAIError(c, http.StatusInternalServerError, "server_error", "invalid relay provider model list")
				return
			}
			entries = legacy
		}
		models := make([]gin.H, 0, len(entries))
		for _, entry := range entries {
			models = append(models, modelPayload(provider, entry))
		}
		data = append(data, gin.H{
			"id":        fmt.Sprintf("%d", provider.ID),
			"name":      provider.Name,
			"endpoint":  provider.Endpoint,
			"apiType":   normalizeAPIType(provider.APIFormat),
			"enabled":   provider.Enabled,
			"models":    models,
			"updatedAt": provider.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"object": "relay_config", "data": data})
}

// ConfigV2 returns providers split by model category. It coexists with the v1
// config while clients migrate to category-scoped managed providers.
func (h *Handler) ConfigV2(c *gin.Context) {
	providers, err := h.svc.ListConfig()
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to list relay config")
		return
	}
	data := make([]gin.H, 0)
	for _, provider := range providers {
		entries := provider.Entries
		if len(entries) == 0 {
			entries, err = legacyEntries(provider)
			if err != nil {
				writeOpenAIError(c, http.StatusInternalServerError, "server_error", "invalid relay provider model list")
				return
			}
		}
		byCategory := map[string][]database.RelayModel{}
		categoryOrder := make([]string, 0, 4)
		for _, entry := range entries {
			category := NormalizeCategory(entry.Category)
			if _, exists := byCategory[category]; !exists {
				categoryOrder = append(categoryOrder, category)
			}
			byCategory[category] = append(byCategory[category], entry)
		}
		for _, category := range categoryOrder {
			models := make([]gin.H, 0, len(byCategory[category]))
			for _, entry := range byCategory[category] {
				models = append(models, modelPayload(provider, entry))
			}
			data = append(data, gin.H{
				"id":        fmt.Sprintf("%d", provider.ID),
				"name":      provider.Name,
				"category":  category,
				"apiType":   normalizeAPIType(provider.APIFormat),
				"enabled":   provider.Enabled,
				"defaults":  gin.H{},
				"models":    models,
				"updatedAt": provider.UpdatedAt,
			})
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"object":        "relay_config",
		"schemaVersion": 2,
		"data":          data,
	})
}

func (h *Handler) forwardVivoOCR(c *gin.Context, resolved *ResolvedModel, image []byte) {
	appID := relayProviderAppID(resolved.Provider, resolved.Model)
	if appID == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "vivo_ocr requires provider AppID")
		return
	}
	query := url.Values{}
	query.Set("requestId", strconv.FormatInt(time.Now().UnixNano(), 10))
	form := url.Values{}
	form.Set("image", base64.StdEncoding.EncodeToString(image))
	config := DecodeProviderConfig(resolved.Provider.Config)
	form.Set("pos", defaultProviderValue(config.OCRPos, "2"))
	form.Set("businessid", defaultProviderValue(config.BusinessIDPrefix, "aigc")+appID)
	resp, err := h.svc.ForwardVivoForm(c.Request.Context(), &resolved.Provider, "", query, form)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	payload, raw, err := decodeJSONResponse(resp)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", string(raw))
		return
	}
	c.JSON(http.StatusOK, gin.H{"text": extractVivoOCRText(payload), "raw": payload})
}

func (h *Handler) forwardVisionOCR(c *gin.Context, resolved *ResolvedModel, image []byte) {
	encoded := base64.StdEncoding.EncodeToString(image)
	mime := "image/png"
	apiType := normalizeAPIType(resolved.Provider.APIFormat)
	prompt := "请识别图片中的文字，只返回识别到的文字内容。"
	var body []byte
	var resp *http.Response
	var err error
	var ok bool
	switch apiType {
	case APIFormatAnthropic:
		body, ok = marshalRelayJSON(c, gin.H{"model": resolved.Model.ModelID, "stream": false, "messages": []gin.H{{"role": "user", "content": []gin.H{{"type": "text", "text": prompt}, {"type": "image", "source": gin.H{"type": "base64", "media_type": mime, "data": encoded}}}}}})
		if !ok {
			return
		}
		resp, err = h.svc.ForwardAnthropicMessages(c.Request.Context(), &resolved.Provider, body)
	case APIFormatOllama:
		body, ok = marshalRelayJSON(c, gin.H{"model": resolved.Model.ModelID, "stream": false, "messages": []gin.H{{"role": "user", "content": prompt, "images": []string{encoded}}}})
		if !ok {
			return
		}
		resp, err = h.svc.ForwardOllamaChat(c.Request.Context(), &resolved.Provider, body)
	default:
		body, ok = marshalRelayJSON(c, gin.H{"model": resolved.Model.ModelID, "stream": false, "messages": []gin.H{{"role": "user", "content": []gin.H{{"type": "text", "text": prompt}, {"type": "image_url", "image_url": gin.H{"url": "data:" + mime + ";base64," + encoded}}}}}})
		if !ok {
			return
		}
		resp, err = h.svc.ForwardChat(c.Request.Context(), &resolved.Provider, body)
	}
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	payload, raw, err := decodeJSONResponse(resp)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", string(raw))
		return
	}
	c.JSON(http.StatusOK, gin.H{"text": extractChatText(apiType, payload), "raw": payload})
}

func (h *Handler) loadSpeechSession(c *gin.Context) *speechSession {
	h.speechMu.Lock()
	defer h.speechMu.Unlock()
	session := h.speech[c.Param("audioId")]
	if session == nil {
		writeOpenAIError(c, http.StatusNotFound, "not_found_error", "speech session not found")
	}
	return session
}

func (h *Handler) forwardSpeechTaskJSON(c *gin.Context, path string, normalize bool) {
	session := h.loadSpeechSession(c)
	if session == nil {
		return
	}
	if session.TaskID == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "speech task has not been started")
		return
	}
	raw, ok := marshalRelayJSON(c, gin.H{"task_id": session.TaskID, "x-sessionId": c.Param("audioId")})
	if !ok {
		return
	}
	resp, err := h.svc.ForwardVivoJSON(c.Request.Context(), &session.Provider, path, vivoSpeechQuery(session.Provider, session.AppID, session.Model.ModelID), raw)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
	payload, rawResp, err := decodeJSONResponse(resp)
	if err != nil || !normalize {
		c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), rawResp)
		return
	}
	c.JSON(resp.StatusCode, gin.H{"text": extractVivoSpeechText(payload), "raw": payload})
}

func relayProviderAppID(provider database.RelayProvider, model database.RelayModel) string {
	if appID := strings.TrimSpace(DecodeProviderConfig(provider.Config).AppID); appID != "" {
		return appID
	}
	params := DecodeAdvancedParams(model.AdvancedParams)
	if params.AppID != nil {
		if appID := strings.TrimSpace(*params.AppID); appID != "" {
			return appID
		}
	}
	if params.User == nil {
		return ""
	}
	return strings.TrimSpace(*params.User)
}

func vivoSpeechQuery(provider database.RelayProvider, appID, engineID string) url.Values {
	config := DecodeProviderConfig(provider.Config)
	query := url.Values{}
	query.Set("client_version", defaultProviderValue(config.ClientVersion, "1.0.0"))
	query.Set("package", defaultProviderValue(config.Package, "lynai"))
	query.Set("user_id", strings.ToLower((appID + strings.Repeat("0", 32))[:32]))
	query.Set("system_time", strconv.FormatInt(time.Now().UnixMilli(), 10))
	query.Set("engineid", engineID)
	query.Set("requestId", strconv.FormatInt(time.Now().UnixNano(), 10))
	return query
}

func defaultProviderValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func decodeJSONResponse(resp *http.Response) (map[string]interface{}, []byte, error) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, raw, err
	}
	var payload map[string]interface{}
	err = json.Unmarshal(raw, &payload)
	return payload, raw, err
}

func nestedString(payload map[string]interface{}, path ...string) (string, bool) {
	var current interface{} = payload
	for _, key := range path {
		m, ok := current.(map[string]interface{})
		if !ok {
			return "", false
		}
		current = m[key]
	}
	value, ok := current.(string)
	return value, ok
}

func marshalRelayJSON(c *gin.Context, payload interface{}) ([]byte, bool) {
	raw, err := json.Marshal(payload)
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to encode relay request")
		return nil, false
	}
	return raw, true
}

func extractChatText(apiType string, payload map[string]interface{}) string {
	switch apiType {
	case APIFormatAnthropic:
		if content, ok := payload["content"].([]interface{}); ok {
			parts := make([]string, 0, len(content))
			for _, raw := range content {
				if item, ok := raw.(map[string]interface{}); ok {
					if text, ok := item["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
			return strings.Join(parts, "")
		}
	case APIFormatOllama:
		if message, ok := payload["message"].(map[string]interface{}); ok {
			if text, ok := message["content"].(string); ok {
				return text
			}
		}
	default:
		if choices, ok := payload["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if message, ok := choice["message"].(map[string]interface{}); ok {
					if text, ok := message["content"].(string); ok {
						return text
					}
				}
			}
		}
	}
	return ""
}

func extractVivoOCRText(payload map[string]interface{}) string {
	if data, ok := payload["data"].(map[string]interface{}); ok {
		if text, ok := data["text"].(string); ok {
			return text
		}
	}
	return extractVivoSpeechText(payload)
}

func extractVivoSpeechText(payload map[string]interface{}) string {
	data, ok := payload["data"].(map[string]interface{})
	if !ok {
		return ""
	}
	result, ok := data["result"].([]interface{})
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(result))
	for _, raw := range result {
		if item, ok := raw.(map[string]interface{}); ok {
			if text, ok := item["onebest"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "")
}

func prepareForwardBody(raw []byte) ([]byte, string, string, string, bool, error) {
	forwardBody, model, providerID, stream, apiType, err := prepareRoutedBodyWithAPIType(raw)
	return forwardBody, apiType, model, providerID, stream, err
}

func prepareRoutedBody(raw []byte) ([]byte, string, string, bool, error) {
	forwardBody, model, providerID, stream, _, err := prepareRoutedBodyWithAPIType(raw)
	if err != nil {
		return nil, "", "", false, err
	}
	return forwardBody, model, providerID, stream, nil
}

func prepareRoutedBodyWithAPIType(raw []byte) ([]byte, string, string, bool, string, error) {
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, "", "", false, "", fmt.Errorf("invalid JSON body")
	}
	model, _ := body["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, "", "", false, "", fmt.Errorf("model is required")
	}
	apiType, _ := body["api_type"].(string)
	apiType = normalizeAPIType(apiType)
	providerID := relayProviderID(body)
	stream, _ := body["stream"].(bool)
	delete(body, "api_type")
	delete(body, "provider_id")
	delete(body, "providerId")
	forwardBody, err := json.Marshal(body)
	if err != nil {
		return nil, "", "", false, "", fmt.Errorf("failed to encode request body")
	}
	return forwardBody, model, providerID, stream, apiType, nil
}

func relayProviderID(body map[string]interface{}) string {
	if value := strings.TrimSpace(fmt.Sprint(body["provider_id"])); value != "" && value != "<nil>" {
		return value
	}
	value := strings.TrimSpace(fmt.Sprint(body["providerId"]))
	if value == "<nil>" {
		return ""
	}
	return value
}

func relayProviderIDFromForm(request *http.Request) string {
	if value := strings.TrimSpace(request.FormValue("provider_id")); value != "" {
		return value
	}
	return strings.TrimSpace(request.FormValue("providerId"))
}

func writeOpenAIError(c *gin.Context, status int, typ, message string) {
	c.JSON(status, gin.H{"error": gin.H{"message": message, "type": typ}})
}

func (h *Handler) writeResolveError(c *gin.Context, err error) {
	if errors.Is(err, ErrUnsupportedType) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "unsupported api_type")
		return
	}
	if errors.Is(err, ErrProviderNotFound) {
		writeOpenAIError(c, http.StatusNotFound, "not_found_error", "no relay provider is configured for the requested api_type and model")
		return
	}
	if errors.Is(err, ErrAmbiguousProvider) {
		writeOpenAIError(c, http.StatusConflict, "ambiguous_route", "multiple relay providers expose the requested model; provider_id is required")
		return
	}
	writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to resolve relay provider")
}

func modelPayload(provider database.RelayProvider, entry database.RelayModel) gin.H {
	advancedParams := DecodeAdvancedParams(entry.AdvancedParams)
	if isVivoAPIFormat(provider.APIFormat) {
		appID := relayProviderAppID(provider, entry)
		if appID != "" {
			advancedParams.AppID = &appID
		}
		if advancedParams.User != nil && advancedParams.AppID != nil && *advancedParams.User == *advancedParams.AppID {
			advancedParams.User = nil
		}
	}
	return gin.H{
		"id":             entry.ModelID,
		"object":         "model",
		"api_type":       normalizeAPIType(provider.APIFormat),
		"category":       NormalizeCategory(entry.Category),
		"displayName":    entry.DisplayName,
		"description":    entry.Description,
		"capabilities":   DecodeCapabilities(entry.Capabilities),
		"advancedParams": advancedParams,
		"enabled":        entry.Enabled && provider.Enabled,
		"providerId":     fmt.Sprintf("%d", provider.ID),
		"providerName":   provider.Name,
	}
}

func isVivoAPIFormat(apiFormat string) bool {
	switch normalizeAPIType(apiFormat) {
	case APIFormatVivoOCR, APIFormatVivoLASR:
		return true
	default:
		return false
	}
}

func legacyEntries(provider database.RelayProvider) ([]database.RelayModel, error) {
	models, err := DecodeModels(provider.Models)
	if err != nil {
		return nil, err
	}
	entries := make([]database.RelayModel, 0, len(models))
	for _, model := range models {
		entries = append(entries, database.RelayModel{ProviderID: provider.ID, ModelID: model, Category: CategoryChat, Enabled: true})
	}
	return entries, nil
}

func copyResponseHeaders(c *gin.Context, headers http.Header) {
	for key, values := range headers {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func streamCopy(c *gin.Context, r io.Reader) {
	buf := make([]byte, 32*1024)
	flusher, _ := c.Writer.(http.Flusher)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = c.Writer.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
