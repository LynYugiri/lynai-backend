package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
)

const APIFormatOpenAI = "openai"

var (
	ErrProviderNotFound = errors.New("relay provider not found")
	ErrUnsupportedType  = errors.New("unsupported relay api type")
	ErrInvalidModels    = errors.New("invalid relay provider models")
)

// Service resolves relay providers and forwards requests to upstream APIs.
type Service struct {
	db     *gorm.DB
	client *http.Client
}

// NewService creates a relay service.
func NewService(db *gorm.DB) *Service {
	return &Service{
		db: db,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 60 * time.Second,
		}},
	}
}

// ListEnabled returns all enabled relay providers.
func (s *Service) ListEnabled() ([]database.RelayProvider, error) {
	var providers []database.RelayProvider
	err := s.db.Where("enabled = ?", true).Order("created_at ASC").Find(&providers).Error
	return providers, err
}

// Resolve finds an enabled provider matching apiType and model.
func (s *Service) Resolve(apiType, model string) (*database.RelayProvider, error) {
	apiType = normalizeAPIType(apiType)
	if apiType != APIFormatOpenAI {
		return nil, ErrUnsupportedType
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, ErrProviderNotFound
	}

	var providers []database.RelayProvider
	if err := s.db.Where("enabled = ? AND api_format = ?", true, apiType).Order("created_at ASC").Find(&providers).Error; err != nil {
		return nil, err
	}
	for i := range providers {
		models, err := DecodeModels(providers[i].Models)
		if err != nil {
			return nil, err
		}
		for _, candidate := range models {
			if candidate == model {
				return &providers[i], nil
			}
		}
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
