package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
)

const maxRelayBodyBytes = 8 << 20

// Handler serves authenticated relay endpoints.
type Handler struct {
	svc *Service
}

// NewHandler creates a relay handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Chat forwards an OpenAI-compatible chat request to an admin-managed upstream.
func (h *Handler) Chat(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes))
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is too large or unreadable")
		return
	}

	forwardBody, apiType, model, stream, err := prepareForwardBody(body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	resolved, err := h.svc.Resolve(apiType, model)
	if err != nil {
		if errors.Is(err, ErrUnsupportedType) {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "unsupported api_type")
			return
		}
		if errors.Is(err, ErrProviderNotFound) {
			writeOpenAIError(c, http.StatusNotFound, "not_found_error", "no relay provider is configured for the requested api_type and model")
			return
		}
		writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to resolve relay provider")
		return
	}

	if resolved.Model.Category != "" && resolved.Model.Category != CategoryChat && resolved.Model.Category != CategoryOCR {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a chat or OCR model")
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

// Transcribe forwards an OpenAI-compatible audio transcription request.
func (h *Handler) Transcribe(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(maxRelayBodyBytes); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid multipart request")
		return
	}
	model := strings.TrimSpace(c.Request.FormValue("model"))
	apiType := normalizeAPIType(c.Request.FormValue("api_type"))
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	resolved, err := h.svc.Resolve(apiType, model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if resolved.Model.Category != CategorySpeech {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not a speech-to-text model")
		return
	}
	delete(c.Request.MultipartForm.Value, "api_type")
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

// ImageGenerations forwards an OpenAI-compatible image generation request.
func (h *Handler) ImageGenerations(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes))
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is too large or unreadable")
		return
	}
	forwardBody, apiType, model, _, err := prepareForwardBody(body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	resolved, err := h.svc.Resolve(apiType, model)
	if err != nil {
		h.writeResolveError(c, err)
		return
	}
	if resolved.Model.Category != CategoryImageGeneration {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "requested model is not an image generation model")
		return
	}
	resp, err := h.svc.ForwardJSON(c.Request.Context(), &resolved.Provider, "/images/generations", forwardBody)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream provider")
		return
	}
	defer resp.Body.Close()
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
		if apiType != APIFormatOpenAI {
			continue
		}
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

func prepareForwardBody(raw []byte) ([]byte, string, string, bool, error) {
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, "", "", false, fmt.Errorf("invalid JSON body")
	}
	model, _ := body["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, "", "", false, fmt.Errorf("model is required")
	}
	apiType, _ := body["api_type"].(string)
	apiType = normalizeAPIType(apiType)
	stream, _ := body["stream"].(bool)
	delete(body, "api_type")
	forwardBody, err := json.Marshal(body)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("failed to encode request body")
	}
	return forwardBody, apiType, model, stream, nil
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
	writeOpenAIError(c, http.StatusInternalServerError, "server_error", "failed to resolve relay provider")
}

func modelPayload(provider database.RelayProvider, entry database.RelayModel) gin.H {
	return gin.H{
		"id":             entry.ModelID,
		"object":         "model",
		"api_type":       normalizeAPIType(provider.APIFormat),
		"category":       NormalizeCategory(entry.Category),
		"displayName":    entry.DisplayName,
		"description":    entry.Description,
		"capabilities":   DecodeCapabilities(entry.Capabilities),
		"advancedParams": DecodeAdvancedParams(entry.AdvancedParams),
		"enabled":        entry.Enabled && provider.Enabled,
		"providerId":     fmt.Sprintf("%d", provider.ID),
		"providerName":   provider.Name,
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
