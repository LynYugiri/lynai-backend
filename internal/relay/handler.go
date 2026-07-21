package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/requestbody"
)

const (
	maxRelayBodyBytes             = 8 << 20
	maxRelayUpstreamResponseBytes = 16 << 20
	maxSpeechCreateBodyBytes      = 16 << 10
)

var errUpstreamResponseTooLarge = errors.New("upstream response is too large")

// Handler serves authenticated relay endpoints.
type Handler struct {
	svc    *Service
	logs   *LogService
	speech *speechSessionStore
}

type countingReadCloser struct {
	io.ReadCloser
	bytes int64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytes += int64(n)
	return n, err
}

// NewHandler creates a relay handler.
func NewHandler(svc *Service) *Handler {
	return NewHandlerWithConfig(svc, 2*time.Hour, 5, 500, 2*time.Minute, 45*time.Second, 30*time.Minute)
}

// NewHandlerWithConfig creates a relay handler with shared speech sessions and timeouts.
func NewHandlerWithConfig(svc *Service, speechTTL time.Duration, perUserCapacity, globalCapacity int, nonStreamTimeout, streamIdleTimeout, streamMaxDuration time.Duration) *Handler {
	svc.setTimeouts(nonStreamTimeout, streamIdleTimeout, streamMaxDuration)
	return &Handler{
		svc:    svc,
		logs:   NewLogService(svc.db),
		speech: newSpeechSessionStore(svc.db, speechTTL, perUserCapacity, globalCapacity),
	}
}

// Close stops the handler's background speech-session cleanup.
func (h *Handler) Close() {
	h.logs.Close()
}

// DeleteExpiredSessions removes expired shared speech sessions.
func (h *Handler) DeleteExpiredSessions(now time.Time) error {
	return h.speech.deleteExpired(now)
}

// LoggingMiddleware records privacy-safe metadata for relay operations.
func (h *Handler) LoggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		operation := relayOperation(c.FullPath())
		if operation == "" {
			c.Next()
			return
		}
		started := time.Now()
		requestBody := &countingReadCloser{ReadCloser: c.Request.Body}
		c.Request.Body = requestBody
		c.Next()
		userID, _ := strconv.ParseInt(c.GetString("userID"), 10, 64)
		errorType := c.GetString("relayErrorType")
		upstreamStatus := contextInt(c, "relayUpstreamStatus")
		if errorType == "" && c.Writer.Status() >= http.StatusBadRequest {
			if upstreamStatus != 0 {
				errorType = "upstream_error"
			} else {
				errorType = "request_error"
			}
		}
		entry := database.RelayRequestLog{
			UserID: userID, Username: c.GetString("username"), Operation: operation,
			Route: c.FullPath(), Protocol: "canonical", HTTPStatus: c.Writer.Status(),
			DurationMS: time.Since(started).Milliseconds(), RequestBytes: requestBody.bytes,
			ResponseBytes: maxInt64(int64(c.Writer.Size()), 0), ProviderID: contextInt64(c, "relayProviderID"),
			ProviderName: c.GetString("relayProviderName"), APIType: c.GetString("relayAPIType"),
			ModelID: c.GetString("relayModelID"), Category: c.GetString("relayCategory"),
			UpstreamStatus: upstreamStatus, ErrorType: errorType,
			CreatedAt: time.Now(),
		}
		h.logs.Enqueue(entry)
	}
}

func relayOperation(path string) string {
	switch path {
	case "/relay/chat":
		return "chat"
	case "/relay/ocr":
		return "ocr"
	case "/relay/transcribe":
		return "transcribe"
	case "/relay/speech/create":
		return "speech_create"
	case "/relay/speech/:audioId/upload":
		return "speech_upload"
	case "/relay/speech/:audioId/run":
		return "speech_run"
	case "/relay/speech/:audioId/progress":
		return "speech_progress"
	case "/relay/speech/:audioId/result":
		return "speech_result"
	case "/relay/images/generations":
		return "image_generation"
	default:
		return ""
	}
}

func (h *Handler) setLogModel(c *gin.Context, resolved *ResolvedModel) {
	c.Set("relayProviderID", resolved.Provider.ID)
	c.Set("relayProviderName", resolved.Provider.Name)
	c.Set("relayAPIType", normalizeAPIType(resolved.Provider.APIFormat))
	c.Set("relayModelID", resolved.Model.ModelID)
	c.Set("relayCategory", NormalizeCategory(resolved.Model.Category))
}

func setUpstreamStatus(c *gin.Context, status int) { c.Set("relayUpstreamStatus", status) }

func contextInt64(c *gin.Context, key string) int64 {
	value, _ := c.Get(key)
	result, _ := value.(int64)
	return result
}
func contextInt(c *gin.Context, key string) int {
	value, _ := c.Get(key)
	result, _ := value.(int)
	return result
}
func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}

// Chat accepts and returns the provider-independent LynAI canonical protocol.
func (h *Handler) Chat(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes))
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is too large or unreadable")
		return
	}

	request, err := parseCanonicalChat(body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	resolved, err := h.svc.Resolve(request.ProviderID, request.Model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	h.setLogModel(c, resolved)

	if resolved.Model.Category != "" && resolved.Model.Category != CategoryChat && resolved.Model.Category != CategoryOCR {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a chat or OCR model")
		return
	}
	capabilities := DecodeCapabilities(resolved.Model.Capabilities)
	if requestUsesTools(request) && !capabilities.Tools {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_feature", "requested model does not support tools")
		return
	}
	if request.Reasoning.Enabled && !capabilities.Thinking {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_feature", "requested model does not support reasoning")
		return
	}
	if requestUsesImages(request) && !capabilities.Vision {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_feature", "requested model does not support image input")
		return
	}
	applyCanonicalDefaults(&request, resolved.Model)
	adapter, err := adapterFor(resolved.Provider.APIFormat)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_feature", err.Error())
		return
	}
	forwardBody, err := adapter.Request(request)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
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
	}
	if err != nil {
		writeForwardError(c, err)
		return
	}
	setUpstreamStatus(c, resp.StatusCode)
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if _, err := readBoundedUpstreamBody(resp.Body); err != nil {
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "upstream error response is too large or unreadable")
			return
		}
		writeOpenAIError(c, resp.StatusCode, "upstream_error", fmt.Sprintf("upstream provider returned HTTP %d", resp.StatusCode))
		return
	}
	if request.Stream {
		writeCanonicalSSE(c, adapter, resp.Body)
		return
	}
	raw, err := readBoundedUpstreamBody(resp.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_protocol_error", "upstream response is too large or unreadable")
		return
	}
	response, err := adapter.Response(raw)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_protocol_error", err.Error())
		return
	}
	c.JSON(http.StatusOK, response)
}

// Transcribe forwards an OpenAI-compatible audio transcription request.
func (h *Handler) Transcribe(c *gin.Context) {
	if err := parseRelayMultipart(c); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid multipart request")
		return
	}
	if hasLegacyFormRoute(c.Request) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "api_type and provider_id are not supported; use providerId")
		return
	}
	model := strings.TrimSpace(c.Request.FormValue("model"))
	providerID := relayProviderIDFromForm(c.Request)
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	resolved, err := h.svc.Resolve(providerID, model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	h.setLogModel(c, resolved)
	if resolved.Model.Category != CategorySpeech {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a speech-to-text model")
		return
	}
	if normalizeAPIType(resolved.Provider.APIFormat) != APIFormatOpenAISpeech {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested provider does not support OpenAI transcription")
		return
	}
	delete(c.Request.MultipartForm.Value, "providerId")
	body, contentType, err := CloneMultipartForm(c.Request.MultipartForm)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "failed to prepare multipart request")
		return
	}
	resp, err := h.svc.ForwardMultipart(c.Request.Context(), &resolved.Provider, "/audio/transcriptions", body, contentType)
	if err != nil {
		writeForwardError(c, err)
		return
	}
	setUpstreamStatus(c, resp.StatusCode)
	defer resp.Body.Close()
	writeBoundedUpstreamResponse(c, resp)
}

// OCR forwards an image OCR request to a managed OCR or vision-chat upstream.
func (h *Handler) OCR(c *gin.Context) {
	if err := parseRelayMultipart(c); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid multipart request")
		return
	}
	if hasLegacyFormRoute(c.Request) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "api_type and provider_id are not supported; use providerId")
		return
	}
	model := strings.TrimSpace(c.Request.FormValue("model"))
	providerID := relayProviderIDFromForm(c.Request)
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	resolved, err := h.svc.Resolve(providerID, model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	h.setLogModel(c, resolved)
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
	requestbody.Limit(c, maxSpeechCreateBodyBytes)
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		if requestbody.TooLarge(err) {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is too large")
			return
		}
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	if _, ok := body["api_type"]; ok {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "api_type is not supported; use providerId")
		return
	}
	if _, ok := body["provider_id"]; ok {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "provider_id is not supported; use providerId")
		return
	}
	model := strings.TrimSpace(fmt.Sprint(body["model"]))
	providerID := relayProviderID(body)
	resolved, err := h.svc.Resolve(providerID, model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	h.setLogModel(c, resolved)
	if normalizeAPIType(resolved.Provider.APIFormat) != APIFormatVivoLASR {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "speech session is only supported for vivo_lasr")
		return
	}
	appID := relayProviderAppID(resolved.Provider, resolved.Model)
	if appID == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "vivo_lasr requires provider AppID")
		return
	}
	sessionID, err := newSpeechSessionID()
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to create speech session")
		return
	}
	if err := h.speech.reserve(sessionID, c.GetString("userID"), resolved, appID); err != nil {
		if errors.Is(err, errSpeechCapacity) {
			writeOpenAIError(c, http.StatusTooManyRequests, "capacity_error", "speech session capacity reached")
			return
		}
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to reserve speech session")
		return
	}
	reserved := true
	defer func() {
		if reserved {
			h.speech.deleteReservation(sessionID, c.GetString("userID"))
		}
	}()
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
		writeForwardError(c, err)
		return
	}
	setUpstreamStatus(c, resp.StatusCode)
	defer resp.Body.Close()
	upstream, rawResp, err := decodeJSONResponse(resp)
	if errors.Is(err, ErrUpstreamTimeout) {
		writeForwardError(c, err)
		return
	}
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", string(rawResp))
		return
	}
	upstreamAudioID, _ := nestedString(upstream, "data", "audio_id")
	if upstreamAudioID == "" {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "vivo_lasr create did not return audio_id")
		return
	}
	if err := h.speech.completeReservation(sessionID, c.GetString("userID"), upstreamAudioID); err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to save speech session")
		return
	}
	reserved = false
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"audio_id": sessionID}})
}

func (h *Handler) SpeechUpload(c *gin.Context) {
	session, ok := h.loadSpeechSession(c)
	if !ok {
		return
	}
	if err := parseRelayMultipart(c); err != nil {
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
		writeForwardError(c, err)
		return
	}
	setUpstreamStatus(c, resp.StatusCode)
	defer resp.Body.Close()
	writeUpstreamResponse(c, resp, false)
}

func (h *Handler) SpeechRun(c *gin.Context) {
	session, ok := h.loadSpeechSession(c)
	if !ok {
		return
	}
	raw, ok := marshalRelayJSON(c, gin.H{"audio_id": session.UpstreamAudioID, "x-sessionId": c.Param("audioId")})
	if !ok {
		return
	}
	resp, err := h.svc.ForwardVivoJSON(c.Request.Context(), &session.Provider, "/lasr/run", vivoSpeechQuery(session.Provider, session.AppID, session.Model.ModelID), raw)
	if err != nil {
		writeForwardError(c, err)
		return
	}
	setUpstreamStatus(c, resp.StatusCode)
	defer resp.Body.Close()
	payload, rawResp, err := decodeJSONResponse(resp)
	if errors.Is(err, ErrUpstreamTimeout) {
		writeForwardError(c, err)
		return
	}
	if errors.Is(err, errUpstreamResponseTooLarge) {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "upstream response is too large")
		return
	}
	if err == nil {
		if taskID, _ := nestedString(payload, "data", "task_id"); taskID != "" {
			h.speech.setTaskID(c.Param("audioId"), c.GetString("userID"), taskID)
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
	forwardBody, model, providerID, _, err := prepareRoutedBody(body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	resolved, err := h.svc.Resolve(providerID, model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	h.setLogModel(c, resolved)
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
		writeForwardError(c, err)
		return
	}
	setUpstreamStatus(c, resp.StatusCode)
	defer resp.Body.Close()
	if normalizeAPIType(resolved.Provider.APIFormat) == APIFormatVivoImage {
		h.writeVivoImageResponse(c, resp)
		return
	}
	writeBoundedUpstreamResponse(c, resp)
}

func (h *Handler) writeVivoImageResponse(c *gin.Context, resp *http.Response) {
	raw, err := readBoundedUpstreamBody(resp.Body)
	if err != nil {
		if errors.Is(err, ErrUpstreamTimeout) {
			writeForwardError(c, err)
			return
		}
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "upstream response is too large or unreadable")
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
	if stream || strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		copyResponseHeaders(c, resp.Header)
		c.Status(resp.StatusCode)
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		streamCopy(c, resp.Body)
		return
	}
	writeBoundedUpstreamResponse(c, resp)
}

// Config returns schema v3 provider-to-model configuration without upstream details.
func (h *Handler) Config(c *gin.Context) {
	providers, err := h.svc.ListConfig()
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to list relay config")
		return
	}
	data := make([]gin.H, 0, len(providers))
	for _, provider := range providers {
		models := make([]gin.H, 0, len(provider.Entries))
		for _, entry := range provider.Entries {
			models = append(models, modelPayload(provider, entry))
		}
		data = append(data, gin.H{
			"providerId": fmt.Sprintf("%d", provider.ID),
			"name":       provider.Name,
			"models":     models,
			"updatedAt":  provider.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"object":        "relay_config",
		"schemaVersion": 3,
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
		writeForwardError(c, err)
		return
	}
	setUpstreamStatus(c, resp.StatusCode)
	defer resp.Body.Close()
	payload, raw, err := decodeJSONResponse(resp)
	if errors.Is(err, ErrUpstreamTimeout) {
		writeForwardError(c, err)
		return
	}
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
		writeForwardError(c, err)
		return
	}
	setUpstreamStatus(c, resp.StatusCode)
	defer resp.Body.Close()
	payload, raw, err := decodeJSONResponse(resp)
	if errors.Is(err, ErrUpstreamTimeout) {
		writeForwardError(c, err)
		return
	}
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", string(raw))
		return
	}
	c.JSON(http.StatusOK, gin.H{"text": extractChatText(apiType, payload), "raw": payload})
}

func (h *Handler) loadSpeechSession(c *gin.Context) (speechSession, bool) {
	session, ok := h.speech.get(c.Param("audioId"), c.GetString("userID"))
	if !ok {
		writeOpenAIError(c, http.StatusNotFound, "not_found_error", "speech session not found")
		return speechSession{}, false
	}
	h.setLogModel(c, &ResolvedModel{Provider: session.Provider, Model: session.Model})
	return session, true
}

func (h *Handler) forwardSpeechTaskJSON(c *gin.Context, path string, normalize bool) {
	session, ok := h.loadSpeechSession(c)
	if !ok {
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
		writeForwardError(c, err)
		return
	}
	setUpstreamStatus(c, resp.StatusCode)
	defer resp.Body.Close()
	payload, rawResp, err := decodeJSONResponse(resp)
	if errors.Is(err, ErrUpstreamTimeout) {
		writeForwardError(c, err)
		return
	}
	if errors.Is(err, errUpstreamResponseTooLarge) {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "upstream response is too large")
		return
	}
	if err != nil || !normalize {
		c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), rawResp)
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && validVivoSpeechResult(payload) {
		h.speech.delete(c.Param("audioId"), c.GetString("userID"))
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
	raw, err := readBoundedUpstreamBody(resp.Body)
	if err != nil {
		return nil, raw, err
	}
	var payload map[string]interface{}
	err = json.Unmarshal(raw, &payload)
	return payload, raw, err
}

func parseRelayMultipart(c *gin.Context) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes)
	return c.Request.ParseMultipartForm(maxRelayBodyBytes)
}

func readBoundedUpstreamBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxRelayUpstreamResponseBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || isNetTimeout(err) {
			return raw, ErrUpstreamTimeout
		}
		return raw, err
	}
	if len(raw) > maxRelayUpstreamResponseBytes {
		return nil, errUpstreamResponseTooLarge
	}
	return raw, nil
}

func writeBoundedUpstreamResponse(c *gin.Context, resp *http.Response) {
	raw, err := readBoundedUpstreamBody(resp.Body)
	if err != nil {
		if errors.Is(err, ErrUpstreamTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeOpenAIError(c, http.StatusGatewayTimeout, "upstream_timeout", "upstream provider timed out")
			return
		}
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "upstream response is too large or unreadable")
		return
	}
	copyResponseHeaders(c, resp.Header)
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), raw)
}

func newSpeechSessionID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
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

func validVivoSpeechResult(payload map[string]interface{}) bool {
	if code, ok := payload["code"].(float64); ok && code != 0 {
		return false
	}
	data, ok := payload["data"].(map[string]interface{})
	if !ok {
		return false
	}
	result, ok := data["result"].([]interface{})
	if !ok {
		return false
	}
	for _, raw := range result {
		item, ok := raw.(map[string]interface{})
		if !ok {
			return false
		}
		if _, ok := item["onebest"].(string); !ok {
			return false
		}
	}
	return true
}

func prepareRoutedBody(raw []byte) ([]byte, string, string, bool, error) {
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, "", "", false, fmt.Errorf("invalid JSON body")
	}
	if _, ok := body["api_type"]; ok {
		return nil, "", "", false, fmt.Errorf("api_type is not supported; use providerId")
	}
	if _, ok := body["provider_id"]; ok {
		return nil, "", "", false, fmt.Errorf("provider_id is not supported; use providerId")
	}
	model, _ := body["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, "", "", false, fmt.Errorf("model is required")
	}
	providerID := relayProviderID(body)
	if providerID == "" {
		return nil, "", "", false, fmt.Errorf("providerId is required")
	}
	stream, _ := body["stream"].(bool)
	delete(body, "providerId")
	forwardBody, err := json.Marshal(body)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("failed to encode request body")
	}
	return forwardBody, model, providerID, stream, nil
}

func relayProviderID(body map[string]interface{}) string {
	value := strings.TrimSpace(fmt.Sprint(body["providerId"]))
	if value == "<nil>" {
		return ""
	}
	return value
}

func relayProviderIDFromForm(request *http.Request) string {
	return strings.TrimSpace(request.FormValue("providerId"))
}

func hasLegacyFormRoute(request *http.Request) bool {
	return strings.TrimSpace(request.FormValue("api_type")) != "" || strings.TrimSpace(request.FormValue("provider_id")) != ""
}

func writeOpenAIError(c *gin.Context, status int, typ, message string) {
	c.Set("relayErrorType", typ)
	c.JSON(status, gin.H{"error": gin.H{"message": message, "type": typ}})
}

func writeForwardError(c *gin.Context, err error) {
	if errors.Is(err, ErrUpstreamTimeout) {
		writeOpenAIError(c, http.StatusGatewayTimeout, "upstream_timeout", "upstream provider timed out")
		return
	}
	writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
}

func (h *Handler) writeResolveError(c *gin.Context, err error) {
	if errors.Is(err, ErrProviderNotFound) {
		writeOpenAIError(c, http.StatusNotFound, "not_found_error", "no enabled relay model matches providerId and model")
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
	payload := gin.H{
		"id":             entry.ModelID,
		"category":       NormalizeCategory(entry.Category),
		"displayName":    entry.DisplayName,
		"description":    entry.Description,
		"capabilities":   DecodeCapabilities(entry.Capabilities),
		"advancedParams": advancedParams,
		"enabled":        entry.Enabled && provider.Enabled,
	}
	if normalizeAPIType(provider.APIFormat) == APIFormatVivoLASR {
		payload["workflow"] = APIFormatVivoLASR
	}
	return payload
}

func isVivoAPIFormat(apiFormat string) bool {
	switch normalizeAPIType(apiFormat) {
	case APIFormatVivoOCR, APIFormatVivoLASR:
		return true
	default:
		return false
	}
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
			if errors.Is(err, ErrUpstreamTimeout) {
				c.Set("relayErrorType", "upstream_timeout")
			}
			return
		}
	}
}
