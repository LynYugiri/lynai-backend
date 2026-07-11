package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
)

const (
	APIFormatOpenAI       = "openai"
	APIFormatAnthropic    = "anthropic"
	APIFormatOllama       = "ollama"
	APIFormatOpenAIImage  = "openai_image"
	APIFormatVivoImage    = "vivo_image"
	APIFormatOpenAISpeech = "openai_speech"
	APIFormatVivoLASR     = "vivo_lasr"
	APIFormatVivoOCR      = "vivo_ocr"
)

const (
	CategoryChat            = "chat"
	CategoryOCR             = "ocr"
	CategorySpeech          = "speech"
	CategoryImageGeneration = "image_generation"
)

var (
	ErrProviderNotFound  = errors.New("relay provider not found")
	ErrAmbiguousProvider = errors.New("multiple relay providers match the requested model")
	ErrUnsupportedType   = errors.New("unsupported relay api type")
	ErrInvalidModels     = errors.New("invalid relay provider models")
)

var supportedAPIFormats = map[string]struct{}{
	APIFormatOpenAI:       {},
	APIFormatAnthropic:    {},
	APIFormatOllama:       {},
	APIFormatOpenAIImage:  {},
	APIFormatVivoImage:    {},
	APIFormatOpenAISpeech: {},
	APIFormatVivoLASR:     {},
	APIFormatVivoOCR:      {},
}

// IsSupportedAPIFormat reports whether apiType can be managed by the relay.
func IsSupportedAPIFormat(apiType string) bool {
	_, ok := supportedAPIFormats[normalizeAPIType(apiType)]
	return ok
}

// SupportsCategory reports whether an API format can expose the given model category.
func SupportsCategory(apiType, category string) bool {
	category = NormalizeCategory(category)
	switch normalizeAPIType(apiType) {
	case APIFormatOpenAI, APIFormatAnthropic, APIFormatOllama:
		return category == CategoryChat || category == CategoryOCR
	case APIFormatOpenAIImage, APIFormatVivoImage:
		return category == CategoryImageGeneration
	case APIFormatOpenAISpeech, APIFormatVivoLASR:
		return category == CategorySpeech
	case APIFormatVivoOCR:
		return category == CategoryOCR
	default:
		return false
	}
}

// ModelCapabilities describes optional behavior exposed by a relay model.
type ModelCapabilities struct {
	Vision   bool `json:"vision"`
	Thinking bool `json:"thinking"`
	Tools    bool `json:"tools"`
}

// ModelAdvancedParams is the server default for compatible model parameters.
type ModelAdvancedParams struct {
	MaxTokens        *int     `json:"maxTokens,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"topP,omitempty"`
	PresencePenalty  *float64 `json:"presencePenalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequencyPenalty,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	Stop             []string `json:"stop,omitempty"`
	AppID            *string  `json:"appId,omitempty"`
	User             *string  `json:"user,omitempty"`
	DebugSSE         bool     `json:"debugSse,omitempty"`
}

// ProviderConfig contains non-secret, API-specific provider settings.
type ProviderConfig struct {
	AppID            string `json:"appId,omitempty"`
	ClientVersion    string `json:"clientVersion,omitempty"`
	Package          string `json:"package,omitempty"`
	OCRPos           string `json:"ocrPos,omitempty"`
	BusinessIDPrefix string `json:"businessIdPrefix,omitempty"`
	ImageModule      string `json:"imageModule,omitempty"`
}

func EncodeProviderConfig(config ProviderConfig) string {
	raw, err := json.Marshal(config)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func DecodeProviderConfig(raw string) ProviderConfig {
	var config ProviderConfig
	_ = json.Unmarshal([]byte(raw), &config)
	return config
}

// ResolvedModel is the upstream provider plus the concrete model metadata.
type ResolvedModel struct {
	Provider database.RelayProvider
	Model    database.RelayModel
}

// Service resolves relay providers and forwards requests to upstream APIs.
type Service struct {
	db     *gorm.DB
	client *http.Client
}

// NewService creates a relay service.
func NewService(db *gorm.DB) *Service {
	return NewServiceWithClient(db, defaultHTTPClient())
}

// NewServiceWithClient 创建 relay service，并允许测试注入自定义 HTTP client。
func NewServiceWithClient(db *gorm.DB, client *http.Client) *Service {
	if client == nil {
		client = defaultHTTPClient()
	}
	return &Service{
		db:     db,
		client: client,
	}
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}

// ListEnabled returns all enabled relay providers.
func (s *Service) ListEnabled() ([]database.RelayProvider, error) {
	var providers []database.RelayProvider
	err := s.db.Preload("Entries", "enabled = ?", true).Where("enabled = ?", true).Order("created_at ASC").Find(&providers).Error
	return providers, err
}

// ListConfig returns every enabled provider and enabled model entry for client configuration.
func (s *Service) ListConfig() ([]database.RelayProvider, error) {
	return s.ListEnabled()
}

// Resolve finds an enabled provider matching apiType and model. providerID is
// optional for legacy clients, but required when multiple providers expose the
// same apiType/model pair.
func (s *Service) Resolve(apiType, model string, providerIDs ...string) (*ResolvedModel, error) {
	apiType = normalizeAPIType(apiType)
	if !IsSupportedAPIFormat(apiType) {
		return nil, ErrUnsupportedType
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, ErrProviderNotFound
	}
	providerID := ""
	if len(providerIDs) > 0 {
		providerID = strings.TrimSpace(providerIDs[0])
	}

	query := s.db.Joins("JOIN relay_providers ON relay_providers.id = relay_models.provider_id").
		Where("relay_models.enabled = ? AND relay_models.model_id = ? AND relay_providers.enabled = ? AND relay_providers.api_format = ?", true, model, true, apiType).
		Order("relay_providers.created_at ASC, relay_models.created_at ASC")
	if providerID != "" {
		query = query.Where("relay_providers.id = ?", providerID)
	}
	var entries []database.RelayModel
	if err := query.Limit(2).Find(&entries).Error; err != nil {
		return nil, err
	}
	if len(entries) > 1 && providerID == "" {
		return nil, ErrAmbiguousProvider
	}
	var resolved *ResolvedModel
	if len(entries) == 1 {
		entry := entries[0]
		var provider database.RelayProvider
		if err := s.db.First(&provider, "id = ?", entry.ProviderID).Error; err != nil {
			return nil, err
		}
		resolved = &ResolvedModel{Provider: provider, Model: entry}
		if providerID != "" {
			return resolved, nil
		}
	}

	// Legacy fallback for providers that have not been expanded into RelayModel rows yet.
	var providers []database.RelayProvider
	legacyQuery := s.db.Preload("Entries").Where("enabled = ? AND api_format = ?", true, apiType).Order("created_at ASC")
	if providerID != "" {
		legacyQuery = legacyQuery.Where("id = ?", providerID)
	}
	if err := legacyQuery.Find(&providers).Error; err != nil {
		return nil, err
	}
	for i := range providers {
		if len(providers[i].Entries) > 0 {
			continue
		}
		models, err := DecodeModels(providers[i].Models)
		if err != nil {
			return nil, err
		}
		for _, candidate := range models {
			if candidate == model {
				if resolved != nil && resolved.Provider.ID != providers[i].ID && providerID == "" {
					return nil, ErrAmbiguousProvider
				}
				resolved = &ResolvedModel{Provider: providers[i], Model: database.RelayModel{ProviderID: providers[i].ID, ModelID: model, Category: CategoryChat, Enabled: true}}
			}
		}
	}
	if resolved != nil {
		return resolved, nil
	}
	return nil, ErrProviderNotFound
}

// ForwardChat sends an OpenAI-compatible chat request to the given upstream.
func (s *Service) ForwardChat(ctx context.Context, provider *database.RelayProvider, body []byte) (*http.Response, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(provider.Endpoint), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	return s.client.Do(req)
}

// ForwardAnthropicMessages sends an Anthropic Messages request to upstream.
func (s *Service) ForwardAnthropicMessages(ctx context.Context, provider *database.RelayProvider, body []byte) (*http.Response, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(provider.Endpoint), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", provider.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	return s.client.Do(req)
}

// ForwardOllamaChat sends an Ollama chat request to upstream.
func (s *Service) ForwardOllamaChat(ctx context.Context, provider *database.RelayProvider, body []byte) (*http.Response, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(provider.Endpoint), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return s.client.Do(req)
}

// ForwardVivoImage sends a vivo image-generation request to upstream.
func (s *Service) ForwardVivoImage(ctx context.Context, provider *database.RelayProvider, body []byte) (*http.Response, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(provider.Endpoint), "/")
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	query := u.Query()
	config := DecodeProviderConfig(provider.Config)
	module := strings.TrimSpace(config.ImageModule)
	if module == "" {
		module = "aigc"
	}
	query.Set("module", module)
	now := time.Now()
	query.Set("request_id", strconv.FormatInt(now.UnixNano(), 10))
	query.Set("system_time", strconv.FormatInt(now.Unix(), 10))
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	return s.client.Do(req)
}

// ForwardVivoJSON sends a JSON request to a vivo-style upstream path with query parameters.
func (s *Service) ForwardVivoJSON(ctx context.Context, provider *database.RelayProvider, path string, query url.Values, body []byte) (*http.Response, error) {
	u, err := vivoURL(provider.Endpoint, path, query)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	return s.client.Do(req)
}

// ForwardVivoMultipart sends multipart data to a vivo-style upstream path.
func (s *Service) ForwardVivoMultipart(ctx context.Context, provider *database.RelayProvider, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u, err := vivoURL(provider.Endpoint, path, query)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	return s.client.Do(req)
}

// ForwardVivoForm sends form data to a vivo-style upstream path.
func (s *Service) ForwardVivoForm(ctx context.Context, provider *database.RelayProvider, path string, query url.Values, form url.Values) (*http.Response, error) {
	u, err := vivoURL(provider.Endpoint, path, query)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	return s.client.Do(req)
}

func vivoURL(endpoint, path string, query url.Values) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	u, err := url.Parse(base + path)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for key, values := range query {
		for _, value := range values {
			q.Add(key, value)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ForwardJSON sends a JSON request to an OpenAI-compatible upstream path.
func (s *Service) ForwardJSON(ctx context.Context, provider *database.RelayProvider, path string, body []byte) (*http.Response, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(provider.Endpoint), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	return s.client.Do(req)
}

// ForwardMultipart sends multipart data to an OpenAI-compatible upstream path.
func (s *Service) ForwardMultipart(ctx context.Context, provider *database.RelayProvider, path string, body io.Reader, contentType string) (*http.Response, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(provider.Endpoint), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	return s.client.Do(req)
}

func EncodeCapabilities(cap ModelCapabilities) string {
	raw, err := json.Marshal(cap)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func DecodeCapabilities(raw string) ModelCapabilities {
	var cap ModelCapabilities
	_ = json.Unmarshal([]byte(raw), &cap)
	return cap
}

func EncodeAdvancedParams(params ModelAdvancedParams) string {
	raw, err := json.Marshal(params)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func DecodeAdvancedParams(raw string) ModelAdvancedParams {
	var params ModelAdvancedParams
	if err := json.Unmarshal([]byte(raw), &params); err == nil {
		return params
	}
	var legacy map[string]interface{}
	if json.Unmarshal([]byte(raw), &legacy) == nil {
		delete(legacy, "stop")
		clean, _ := json.Marshal(legacy)
		_ = json.Unmarshal(clean, &params)
		var original map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &original) == nil {
			var stop string
			if json.Unmarshal(original["stop"], &stop) == nil && strings.TrimSpace(stop) != "" {
				params.Stop = []string{strings.TrimSpace(stop)}
			}
		}
	}
	return params
}

// ApplyModelDefaults adds administrator defaults without replacing client values.
func ApplyModelDefaults(apiType string, body []byte, model database.RelayModel) ([]byte, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	params := DecodeAdvancedParams(model.AdvancedParams)
	set := func(key string, value interface{}) {
		if _, exists := payload[key]; !exists && value != nil {
			payload[key] = value
		}
	}
	switch normalizeAPIType(apiType) {
	case APIFormatOpenAI:
		set("max_tokens", params.MaxTokens)
		set("temperature", params.Temperature)
		set("top_p", params.TopP)
		set("presence_penalty", params.PresencePenalty)
		set("frequency_penalty", params.FrequencyPenalty)
		set("seed", params.Seed)
		if len(params.Stop) > 0 {
			set("stop", params.Stop)
		}
		set("user", params.User)
	case APIFormatAnthropic:
		set("max_tokens", params.MaxTokens)
		set("temperature", params.Temperature)
		set("top_p", params.TopP)
		if len(params.Stop) > 0 {
			set("stop_sequences", params.Stop)
		}
	case APIFormatOllama:
		options, _ := payload["options"].(map[string]interface{})
		if options == nil {
			options = map[string]interface{}{}
		}
		if _, ok := options["num_predict"]; !ok && params.MaxTokens != nil {
			options["num_predict"] = *params.MaxTokens
		}
		if _, ok := options["temperature"]; !ok && params.Temperature != nil {
			options["temperature"] = *params.Temperature
		}
		if _, ok := options["top_p"]; !ok && params.TopP != nil {
			options["top_p"] = *params.TopP
		}
		if len(options) > 0 {
			payload["options"] = options
		}
	}
	return json.Marshal(payload)
}

func NormalizeCategory(category string) string {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case CategoryOCR:
		return CategoryOCR
	case CategorySpeech:
		return CategorySpeech
	case CategoryImageGeneration:
		return CategoryImageGeneration
	default:
		return CategoryChat
	}
}

func CloneMultipartForm(src *multipart.Form) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for key, values := range src.Value {
		for _, value := range values {
			if err := w.WriteField(key, value); err != nil {
				return nil, "", err
			}
		}
	}
	for key, files := range src.File {
		for _, fileHeader := range files {
			file, err := fileHeader.Open()
			if err != nil {
				return nil, "", err
			}
			part, err := w.CreateFormFile(key, fileHeader.Filename)
			if err != nil {
				_ = file.Close()
				return nil, "", err
			}
			_, copyErr := io.Copy(part, file)
			_ = file.Close()
			if copyErr != nil {
				return nil, "", copyErr
			}
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

// DecodeModels parses the provider model list.
func DecodeModels(raw string) ([]string, error) {
	var models []string
	if strings.TrimSpace(raw) == "" {
		return models, nil
	}
	if err := json.Unmarshal([]byte(raw), &models); err != nil {
		return nil, ErrInvalidModels
	}
	clean := models[:0]
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model != "" {
			clean = append(clean, model)
		}
	}
	return clean, nil
}

func normalizeAPIType(apiType string) string {
	apiType = strings.ToLower(strings.TrimSpace(apiType))
	if apiType == "" {
		return APIFormatOpenAI
	}
	return apiType
}
