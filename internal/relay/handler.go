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
			BindingID: contextInt64Ptr(c, "relayBindingID"), CredentialID: contextInt64Ptr(c, "relayCredentialID"),
			CredentialName: contextStringPtr(c, "relayCredentialName"), AttemptCount: maxInt(contextInt(c, "relayAttemptCount"), 1),
			FailoverCount: maxInt(contextInt(c, "relayFailoverCount"), 0),
			CreatedAt:     time.Now(),
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

func (h *Handler) setLogCandidate(c *gin.Context, candidate Candidate, attempts int) {
	c.Set("relayProviderID", candidate.Provider.ID)
	c.Set("relayProviderName", candidate.Provider.Name)
	c.Set("relayBindingID", candidate.Binding.ID)
	c.Set("relayCredentialID", candidate.Credential.ID)
	c.Set("relayCredentialName", candidate.Credential.Name)
	c.Set("relayAPIType", normalizeAPIType(candidate.Provider.APIFormat))
	c.Set("relayModelID", candidate.Model.ModelID)
	c.Set("relayCategory", NormalizeCategory(candidate.Model.Category))
	c.Set("relayAttemptCount", attempts)
	c.Set("relayFailoverCount", attempts-1)
}

func setUpstreamStatus(c *gin.Context, status int) { c.Set("relayUpstreamStatus", status) }

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
}

func skipProviderCandidates(candidates []Candidate, index int) int {
	providerID := candidates[index].Provider.ID
	for index+1 < len(candidates) && candidates[index+1].Provider.ID == providerID {
		index++
	}
	return index
}

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
func contextInt64Ptr(c *gin.Context, key string) *int64 {
	value := contextInt64(c, key)
	if value == 0 {
		return nil
	}
	return &value
}
func contextStringPtr(c *gin.Context, key string) *string {
	value := c.GetString(key)
	if value == "" {
		return nil
	}
	return &value
}
func maxInt(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
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
	model, candidates, err := h.svc.Candidates(request.Model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}

	if model.Category != "" && model.Category != CategoryChat && model.Category != CategoryOCR {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a chat or OCR model")
		return
	}
	if len(candidates) == 0 {
		h.writeResolveError(c, ErrModelNotFound)
		return
	}
	capabilities := DecodeCapabilities(model.Capabilities)
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
	applyCanonicalDefaults(&request, model)
	var terminalErr error
	var terminalStatus int
	attempts := 0
	for i := range candidates {
		candidate := candidates[i]
		adapter, adapterErr := adapterFor(candidate.Provider.APIFormat)
		if adapterErr != nil {
			continue
		}
		attempt := request
		attempt.Model = candidate.Binding.UpstreamModel
		forwardBody, requestErr := adapter.Request(attempt)
		if requestErr != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", requestErr.Error())
			return
		}
		attempts++
		h.setLogCandidate(c, candidate, attempts)
		var resp *http.Response
		switch normalizeAPIType(candidate.Provider.APIFormat) {
		case APIFormatOpenAI:
			resp, err = h.svc.ForwardChat(c.Request.Context(), &candidate, forwardBody)
		case APIFormatAnthropic:
			resp, err = h.svc.ForwardAnthropicMessages(c.Request.Context(), &candidate, forwardBody)
		case APIFormatOllama:
			resp, err = h.svc.ForwardOllamaChat(c.Request.Context(), &candidate, forwardBody)
		}
		if err != nil {
			terminalErr = err
			terminalStatus = 0
			setUpstreamStatus(c, 0)
			h.svc.router.Cooldown(candidate, 0, "", true)
			continue
		}
		setUpstreamStatus(c, resp.StatusCode)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			terminalStatus = resp.StatusCode
			terminalErr = nil
			retry := retryableStatus(resp.StatusCode)
			h.svc.router.Cooldown(candidate, resp.StatusCode, resp.Header.Get("Retry-After"), false)
			_, readErr := readBoundedUpstreamBody(resp.Body)
			_ = resp.Body.Close()
			if retry || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
				if readErr != nil {
					terminalErr = readErr
					terminalStatus = 0
					setUpstreamStatus(c, 0)
				}
				if resp.StatusCode == http.StatusNotFound {
					i = skipProviderCandidates(candidates, i)
				}
				continue
			}
			writeOpenAIError(c, resp.StatusCode, "upstream_error", fmt.Sprintf("upstream provider returned HTTP %d", resp.StatusCode))
			return
		}
		if request.Stream {
			defer resp.Body.Close()
			if streamErr := writeCanonicalSSE(c, adapter, resp.Body); streamErr != nil {
				h.svc.router.Cooldown(candidate, 0, "", true)
				return
			}
			h.svc.router.Success(candidate)
			return
		}
		raw, readErr := readBoundedUpstreamBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			terminalErr = readErr
			terminalStatus = 0
			setUpstreamStatus(c, 0)
			h.svc.router.Cooldown(candidate, 0, "", true)
			continue
		}
		response, responseErr := adapter.Response(raw)
		if responseErr != nil {
			terminalErr = responseErr
			terminalStatus = 0
			setUpstreamStatus(c, 0)
			h.svc.router.Cooldown(candidate, http.StatusBadGateway, "", false)
			continue
		}
		h.svc.router.Success(candidate)
		c.JSON(http.StatusOK, response)
		return
	}
	if terminalStatus != 0 {
		writeOpenAIError(c, terminalStatus, "upstream_error", fmt.Sprintf("upstream provider returned HTTP %d", terminalStatus))
		return
	}
	writeForwardError(c, terminalErr)
}

// Transcribe forwards an OpenAI-compatible audio transcription request.
func (h *Handler) Transcribe(c *gin.Context) {
	if err := parseRelayMultipart(c); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid multipart request")
		return
	}
	model := strings.TrimSpace(c.Request.FormValue("model"))
	if strings.TrimSpace(c.Request.FormValue("providerId")) != "" || strings.TrimSpace(c.Request.FormValue("provider_id")) != "" || strings.TrimSpace(c.Request.FormValue("api_type")) != "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "only model may select a relay route")
		return
	}
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	globalModel, candidates, err := h.svc.Candidates(model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if globalModel.Category != CategorySpeech {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a speech-to-text model")
		return
	}
	for i := range candidates {
		candidate := candidates[i]
		if normalizeAPIType(candidate.Provider.APIFormat) != APIFormatOpenAISpeech {
			continue
		}
		h.setLogCandidate(c, candidate, i+1)
		c.Request.MultipartForm.Value["model"] = []string{candidate.Binding.UpstreamModel}
		body, contentType, cloneErr := CloneMultipartForm(c.Request.MultipartForm)
		if cloneErr != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "failed to prepare multipart request")
			return
		}
		resp, forwardErr := h.svc.ForwardMultipart(c.Request.Context(), &candidate, "/audio/transcriptions", body, contentType)
		if forwardErr != nil {
			h.svc.router.Cooldown(candidate, 0, "", true)
			continue
		}
		setUpstreamStatus(c, resp.StatusCode)
		if retryableStatus(resp.StatusCode) || resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
			h.svc.router.Cooldown(candidate, resp.StatusCode, resp.Header.Get("Retry-After"), false)
			_ = resp.Body.Close()
			continue
		}
		h.svc.router.Success(candidate)
		defer resp.Body.Close()
		writeBoundedUpstreamResponse(c, resp)
		return
	}
	writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "no routable transcription provider succeeded")
}

// OCR forwards an image OCR request to a managed OCR or vision-chat upstream.
func (h *Handler) OCR(c *gin.Context) {
	if err := parseRelayMultipart(c); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid multipart request")
		return
	}
	model := strings.TrimSpace(c.Request.FormValue("model"))
	if strings.TrimSpace(c.Request.FormValue("providerId")) != "" || strings.TrimSpace(c.Request.FormValue("provider_id")) != "" || strings.TrimSpace(c.Request.FormValue("api_type")) != "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "only model may select a relay route")
		return
	}
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	globalModel, candidates, err := h.svc.Candidates(model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if globalModel.Category != CategoryOCR {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not an OCR model")
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
	h.forwardOCRCandidates(c, candidates, image)
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
	model := strings.TrimSpace(fmt.Sprint(body["model"]))
	if _, exists := body["providerId"]; exists {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "only model may select a relay route")
		return
	}
	if _, exists := body["provider_id"]; exists {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "only model may select a relay route")
		return
	}
	if _, exists := body["api_type"]; exists {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "only model may select a relay route")
		return
	}
	globalModel, candidates, err := h.svc.Candidates(model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if globalModel.Category != CategorySpeech {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a speech model")
		return
	}
	filtered := candidates[:0]
	for _, candidate := range candidates {
		if normalizeAPIType(candidate.Provider.APIFormat) == APIFormatVivoLASR {
			filtered = append(filtered, candidate)
		}
	}
	if len(filtered) == 0 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "speech session is only supported for vivo_lasr")
		return
	}
	sessionID, err := newSpeechSessionID()
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to create speech session")
		return
	}
	for i, candidate := range filtered {
		appID := candidateAppID(candidate)
		if appID == "" {
			continue
		}
		h.setLogCandidate(c, candidate, i+1)
		if err := h.speech.reserve(sessionID, c.GetString("userID"), &candidate, &appID); err != nil {
			if errors.Is(err, errSpeechCapacity) {
				writeOpenAIError(c, http.StatusTooManyRequests, "capacity_error", "speech session capacity reached")
				return
			}
			writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to reserve speech session")
			return
		}
		query := vivoSpeechQuery(candidate.Config, appID, candidate.Binding.UpstreamModel)
		raw, ok := marshalRelayJSON(c, map[string]interface{}{"audio_type": fmt.Sprint(body["audio_type"]), "x-sessionId": sessionID, "slice_num": body["slice_num"]})
		if !ok {
			h.speech.deleteReservation(sessionID, c.GetString("userID"))
			return
		}
		resp, forwardErr := h.svc.ForwardVivoJSON(c.Request.Context(), &candidate, "/lasr/create", query, raw)
		if forwardErr != nil {
			h.speech.deleteReservation(sessionID, c.GetString("userID"))
			writeForwardError(c, forwardErr)
			return
		}
		setUpstreamStatus(c, resp.StatusCode)
		rawResp, readErr := readBoundedUpstreamBody(resp.Body)
		_ = resp.Body.Close()
		if retryableStatus(resp.StatusCode) || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			h.svc.router.Cooldown(candidate, resp.StatusCode, resp.Header.Get("Retry-After"), false)
			h.speech.deleteReservation(sessionID, c.GetString("userID"))
			continue
		}
		if readErr != nil {
			h.speech.deleteReservation(sessionID, c.GetString("userID"))
			writeForwardError(c, readErr)
			return
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			h.speech.deleteReservation(sessionID, c.GetString("userID"))
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", string(rawResp))
			return
		}
		var upstream map[string]interface{}
		if err := json.Unmarshal(rawResp, &upstream); err != nil {
			h.speech.deleteReservation(sessionID, c.GetString("userID"))
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", string(rawResp))
			return
		}
		upstreamAudioID, _ := nestedString(upstream, "data", "audio_id")
		if upstreamAudioID == "" {
			h.speech.deleteReservation(sessionID, c.GetString("userID"))
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "vivo_lasr create did not return audio_id")
			return
		}
		if err := h.speech.completeReservation(sessionID, c.GetString("userID"), upstreamAudioID); err != nil {
			writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to save speech session")
			return
		}
		h.svc.router.Success(candidate)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"audio_id": sessionID}})
		return
	}
	writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "no vivo_lasr provider succeeded")
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
	query := vivoSpeechQuery(session.Candidate.Config, session.AppID, session.Candidate.Binding.UpstreamModel)
	query.Set("audio_id", session.UpstreamAudioID)
	query.Set("x-sessionId", c.Param("audioId"))
	query.Set("slice_index", c.DefaultQuery("slice_index", c.Request.FormValue("slice_index")))
	resp, err := h.svc.ForwardVivoMultipart(c.Request.Context(), &session.Candidate, "/lasr/upload", query, body, contentType)
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
	resp, err := h.svc.ForwardVivoJSON(c.Request.Context(), &session.Candidate, "/lasr/run", vivoSpeechQuery(session.Candidate.Config, session.AppID, session.Candidate.Binding.UpstreamModel), raw)
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
	forwardBody, modelID, _, err := prepareRoutedBody(body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model, candidates, err := h.svc.Candidates(modelID)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if model.Category != CategoryImageGeneration {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not an image generation model")
		return
	}
	for i := range candidates {
		candidate := candidates[i]
		apiType := normalizeAPIType(candidate.Provider.APIFormat)
		if apiType != APIFormatVivoImage && apiType != APIFormatOpenAIImage {
			continue
		}
		h.setLogCandidate(c, candidate, i+1)
		attemptBody, mapErr := replaceJSONModel(forwardBody, candidate.Binding.UpstreamModel)
		if mapErr != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
			return
		}
		attemptBody, mapErr = ApplyModelDefaults(candidate.Provider.APIFormat, attemptBody, model)
		if mapErr != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
			return
		}
		var resp *http.Response
		if apiType == APIFormatVivoImage {
			resp, err = h.svc.ForwardVivoImage(c.Request.Context(), &candidate, attemptBody)
		} else {
			resp, err = h.svc.ForwardJSON(c.Request.Context(), &candidate, "/images/generations", attemptBody)
		}
		if err != nil {
			writeForwardError(c, err)
			return
		}
		setUpstreamStatus(c, resp.StatusCode)
		if retryableStatus(resp.StatusCode) || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			h.svc.router.Cooldown(candidate, resp.StatusCode, resp.Header.Get("Retry-After"), false)
			_ = resp.Body.Close()
			continue
		}
		h.svc.router.Success(candidate)
		defer resp.Body.Close()
		if apiType == APIFormatVivoImage {
			h.writeVivoImageResponse(c, resp)
		} else {
			writeBoundedUpstreamResponse(c, resp)
		}
		return
	}
	writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "no image provider succeeded")
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

// Config returns the schema v4 flat global model configuration.
func (h *Handler) Config(c *gin.Context) {
	models, err := h.svc.ListConfig()
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to list relay config")
		return
	}
	data := make([]gin.H, 0, len(models))
	for _, model := range models {
		payload := modelPayload(model)
		var formats []string
		_ = h.svc.db.Table("relay_model_bindings b").Select("DISTINCT p.api_format").Joins("JOIN relay_providers p ON p.id = b.provider_id").Joins("JOIN relay_provider_credentials c ON c.provider_id = p.id").Where("b.relay_model_id = ? AND b.enabled = ? AND p.enabled = ? AND c.enabled = ?", model.ID, true, true, true).Pluck("p.api_format", &formats).Error
		if NormalizeCategory(model.Category) == CategorySpeech && len(formats) > 0 {
			allLASR := true
			for _, format := range formats {
				if normalizeAPIType(format) != APIFormatVivoLASR {
					allLASR = false
				}
			}
			if allLASR {
				payload["workflow"] = APIFormatVivoLASR
			}
		}
		data = append(data, payload)
	}
	c.JSON(http.StatusOK, gin.H{
		"object":        "relay_config",
		"schemaVersion": 4,
		"data":          data,
	})
}

func (h *Handler) forwardOCRCandidates(c *gin.Context, candidates []Candidate, image []byte) {
	for i := range candidates {
		candidate := candidates[i]
		h.setLogCandidate(c, candidate, i+1)
		var resp *http.Response
		var payload map[string]interface{}
		var raw []byte
		var err error
		if normalizeAPIType(candidate.Provider.APIFormat) == APIFormatVivoOCR {
			resp, payload, raw, err = h.doVivoOCR(c, candidate, image)
		} else {
			resp, payload, raw, err = h.doVisionOCR(c, candidate, image)
		}
		if err != nil && resp == nil {
			h.svc.router.Cooldown(candidate, 0, "", true)
			continue
		}
		setUpstreamStatus(c, resp.StatusCode)
		if retryableStatus(resp.StatusCode) || resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
			h.svc.router.Cooldown(candidate, resp.StatusCode, resp.Header.Get("Retry-After"), false)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", string(raw))
			return
		}
		if err != nil {
			h.svc.router.Cooldown(candidate, 0, "", true)
			continue
		}
		h.svc.router.Success(candidate)
		if normalizeAPIType(candidate.Provider.APIFormat) == APIFormatVivoOCR {
			c.JSON(http.StatusOK, gin.H{"text": extractVivoOCRText(payload), "raw": payload})
		} else {
			c.JSON(http.StatusOK, gin.H{"text": extractChatText(normalizeAPIType(candidate.Provider.APIFormat), payload), "raw": payload})
		}
		return
	}
	writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "no OCR provider succeeded")
}

func (h *Handler) doVivoOCR(c *gin.Context, candidate Candidate, image []byte) (*http.Response, map[string]interface{}, []byte, error) {
	appID := candidateAppID(candidate)
	if appID == "" {
		return nil, nil, nil, errors.New("vivo_ocr requires AppID")
	}
	query := url.Values{}
	query.Set("requestId", strconv.FormatInt(time.Now().UnixNano(), 10))
	form := url.Values{}
	form.Set("image", base64.StdEncoding.EncodeToString(image))
	config := candidate.Config
	form.Set("pos", defaultProviderValue(config.OCRPos, "2"))
	form.Set("businessid", defaultProviderValue(config.BusinessIDPrefix, "aigc")+appID)
	resp, err := h.svc.ForwardVivoForm(c.Request.Context(), &candidate, "", query, form)
	if err != nil {
		return nil, nil, nil, err
	}
	raw, err := readBoundedUpstreamBody(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil, raw, err
	}
	var payload map[string]interface{}
	err = json.Unmarshal(raw, &payload)
	return resp, payload, raw, err
}

func (h *Handler) doVisionOCR(c *gin.Context, candidate Candidate, image []byte) (*http.Response, map[string]interface{}, []byte, error) {
	encoded := base64.StdEncoding.EncodeToString(image)
	mime := "image/png"
	apiType := normalizeAPIType(candidate.Provider.APIFormat)
	prompt := "请识别图片中的文字，只返回识别到的文字内容。"
	var body []byte
	var resp *http.Response
	var err error
	var ok bool
	switch apiType {
	case APIFormatAnthropic:
		body, ok = marshalRelayJSON(c, gin.H{"model": candidate.Binding.UpstreamModel, "stream": false, "messages": []gin.H{{"role": "user", "content": []gin.H{{"type": "text", "text": prompt}, {"type": "image", "source": gin.H{"type": "base64", "media_type": mime, "data": encoded}}}}}})
		if !ok {
			return nil, nil, nil, errors.New("encode OCR request")
		}
		resp, err = h.svc.ForwardAnthropicMessages(c.Request.Context(), &candidate, body)
	case APIFormatOllama:
		body, ok = marshalRelayJSON(c, gin.H{"model": candidate.Binding.UpstreamModel, "stream": false, "messages": []gin.H{{"role": "user", "content": prompt, "images": []string{encoded}}}})
		if !ok {
			return nil, nil, nil, errors.New("encode OCR request")
		}
		resp, err = h.svc.ForwardOllamaChat(c.Request.Context(), &candidate, body)
	default:
		body, ok = marshalRelayJSON(c, gin.H{"model": candidate.Binding.UpstreamModel, "stream": false, "messages": []gin.H{{"role": "user", "content": []gin.H{{"type": "text", "text": prompt}, {"type": "image_url", "image_url": gin.H{"url": "data:" + mime + ";base64," + encoded}}}}}})
		if !ok {
			return nil, nil, nil, errors.New("encode OCR request")
		}
		resp, err = h.svc.ForwardChat(c.Request.Context(), &candidate, body)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	raw, err := readBoundedUpstreamBody(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil, raw, err
	}
	var payload map[string]interface{}
	err = json.Unmarshal(raw, &payload)
	return resp, payload, raw, err
}

func (h *Handler) loadSpeechSession(c *gin.Context) (speechSession, bool) {
	session, ok := h.speech.get(c.Param("audioId"), c.GetString("userID"))
	if !ok {
		writeOpenAIError(c, http.StatusNotFound, "not_found_error", "speech session not found")
		return speechSession{}, false
	}
	h.setLogCandidate(c, session.Candidate, 1)
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
	resp, err := h.svc.ForwardVivoJSON(c.Request.Context(), &session.Candidate, path, vivoSpeechQuery(session.Candidate.Config, session.AppID, session.Candidate.Binding.UpstreamModel), raw)
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

func candidateAppID(candidate Candidate) string {
	if appID := strings.TrimSpace(candidate.Config.AppID); appID != "" {
		return appID
	}
	params := DecodeAdvancedParams(candidate.Model.AdvancedParams)
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

func vivoSpeechQuery(config ProviderConfig, appID, engineID string) url.Values {
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

func prepareRoutedBody(raw []byte) ([]byte, string, bool, error) {
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, "", false, fmt.Errorf("invalid JSON body")
	}
	if _, exists := body["providerId"]; exists {
		return nil, "", false, fmt.Errorf("only model may select a relay route")
	}
	if _, exists := body["provider_id"]; exists {
		return nil, "", false, fmt.Errorf("only model may select a relay route")
	}
	if _, exists := body["api_type"]; exists {
		return nil, "", false, fmt.Errorf("only model may select a relay route")
	}
	model, _ := body["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, "", false, fmt.Errorf("model is required")
	}
	stream, _ := body["stream"].(bool)
	forwardBody, err := json.Marshal(body)
	if err != nil {
		return nil, "", false, fmt.Errorf("failed to encode request body")
	}
	return forwardBody, model, stream, nil
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
	if errors.Is(err, ErrModelNotFound) {
		writeOpenAIError(c, http.StatusNotFound, "not_found_error", "no enabled relay model matches model")
		return
	}
	writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to resolve relay provider")
}

func modelPayload(entry database.RelayModel) gin.H {
	advancedParams := DecodeAdvancedParams(entry.AdvancedParams)
	advancedParams.AppID = nil
	payload := gin.H{
		"id":             entry.ModelID,
		"category":       NormalizeCategory(entry.Category),
		"displayName":    entry.DisplayName,
		"description":    entry.Description,
		"capabilities":   DecodeCapabilities(entry.Capabilities),
		"advancedParams": advancedParams,
		"enabled":        entry.Enabled,
	}
	return payload
}

func replaceJSONModel(raw []byte, model string) ([]byte, error) {
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	body["model"] = model
	return json.Marshal(body)
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
