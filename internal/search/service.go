package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lynai/backend/internal/relay"
)

const (
	ProviderAuto    = "auto"
	ProviderTavily  = "tavily"
	ProviderSearXNG = "searxng"

	defaultMaxResults       = 5
	maxResults              = 10
	maxUpstreamResponseSize = 2 << 20
)

var (
	ErrInvalidRequest      = errors.New("invalid search request")
	ErrProviderUnavailable = errors.New("search provider is not configured")
	ErrUpstream            = errors.New("search provider request failed")
)

// Config contains server-owned search provider settings.
type Config struct {
	TavilyAPIKey  string
	TavilyOrigin  string
	SearXNGOrigin string
	Timeout       time.Duration
}

// Request is the provider-neutral web search request.
type Request struct {
	Query      string `json:"query"`
	Provider   string `json:"provider,omitempty"`
	MaxResults int    `json:"maxResults,omitempty"`
	Language   string `json:"language,omitempty"`
	TimeRange  string `json:"timeRange,omitempty"`
}

// Result is one normalized web result.
type Result struct {
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Snippet     string   `json:"snippet"`
	Score       *float64 `json:"score,omitempty"`
	PublishedAt string   `json:"publishedAt,omitempty"`
}

// Response is the provider-neutral web search response.
type Response struct {
	Provider string   `json:"provider"`
	Results  []Result `json:"results"`
}

type provider struct {
	name   string
	search func(context.Context, Request) ([]Result, error)
}

// Service dispatches searches only to server-configured providers.
type Service struct {
	providers map[string]provider
	order     []string
	timeout   time.Duration
}

// NewService validates configured origins and creates an SSRF-protected service.
func NewService(cfg Config, policy *relay.EndpointPolicy) (*Service, error) {
	if policy == nil {
		return nil, errors.New("search endpoint policy is required")
	}
	return newService(cfg, policy.HTTPClient(), policy.ValidateEndpoint)
}

func newService(cfg Config, client *http.Client, validate func(string) error) (*Service, error) {
	if cfg.Timeout <= 0 {
		return nil, errors.New("search timeout must be greater than zero")
	}
	client.Timeout = cfg.Timeout
	s := &Service{providers: make(map[string]provider), timeout: cfg.Timeout}
	if strings.TrimSpace(cfg.TavilyAPIKey) != "" {
		origin, err := configuredOrigin(cfg.TavilyOrigin, validate)
		if err != nil {
			return nil, fmt.Errorf("TAVILY_ORIGIN: %w", err)
		}
		s.providers[ProviderTavily] = provider{name: ProviderTavily, search: tavilyAdapter(client, origin, cfg.TavilyAPIKey)}
		s.order = append(s.order, ProviderTavily)
	}
	if strings.TrimSpace(cfg.SearXNGOrigin) != "" {
		origin, err := configuredOrigin(cfg.SearXNGOrigin, validate)
		if err != nil {
			return nil, fmt.Errorf("SEARXNG_ORIGIN: %w", err)
		}
		s.providers[ProviderSearXNG] = provider{name: ProviderSearXNG, search: searXNGAdapter(client, origin)}
		s.order = append(s.order, ProviderSearXNG)
	}
	return s, nil
}

// Search validates and executes a normalized web search.
func (s *Service) Search(ctx context.Context, req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req.Query = strings.TrimSpace(req.Query)
	req.Provider = strings.ToLower(strings.TrimSpace(req.Provider))
	req.Language = strings.TrimSpace(req.Language)
	req.TimeRange = strings.ToLower(strings.TrimSpace(req.TimeRange))
	if req.Provider == "" {
		req.Provider = ProviderAuto
	}
	if req.MaxResults == 0 {
		req.MaxResults = defaultMaxResults
	}
	if req.Query == "" || len([]rune(req.Query)) > 1000 || req.MaxResults < 1 || req.MaxResults > maxResults || !validLanguage(req.Language) || !validTimeRange(req.TimeRange) {
		return Response{}, ErrInvalidRequest
	}
	if req.Provider != ProviderAuto && req.Provider != ProviderTavily && req.Provider != ProviderSearXNG {
		return Response{}, ErrInvalidRequest
	}

	if req.Provider != ProviderAuto {
		p, ok := s.providers[req.Provider]
		if !ok {
			return Response{}, ErrProviderUnavailable
		}
		results, err := p.search(ctx, req)
		if err != nil {
			return Response{}, err
		}
		return Response{Provider: p.name, Results: results}, nil
	}
	if len(s.order) == 0 {
		return Response{}, ErrProviderUnavailable
	}
	for _, name := range s.order {
		p := s.providers[name]
		results, err := p.search(ctx, req)
		if err == nil {
			return Response{Provider: p.name, Results: results}, nil
		}
		if ctx.Err() != nil {
			return Response{}, ErrUpstream
		}
	}
	return Response{}, ErrUpstream
}

func configuredOrigin(raw string, validate func(string) error) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Host == "" {
		return nil, errors.New("must be an absolute HTTP(S) origin")
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("must contain only scheme, host, and optional port")
	}
	u.Path = ""
	if err := validate(u.String()); err != nil {
		return nil, err
	}
	return u, nil
}

func tavilyAdapter(client *http.Client, origin *url.URL, apiKey string) func(context.Context, Request) ([]Result, error) {
	return func(ctx context.Context, req Request) ([]Result, error) {
		payload := map[string]any{
			"api_key": apiKey, "query": req.Query, "max_results": req.MaxResults,
			"include_answer": false, "include_raw_content": false,
		}
		if req.TimeRange != "" {
			payload["time_range"] = req.TimeRange
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, ErrUpstream
		}
		target := *origin
		target.Path = "/search"
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
		if err != nil {
			return nil, ErrUpstream
		}
		httpReq.Header.Set("Content-Type", "application/json")
		var responsePayload struct {
			Results []struct {
				Title         string  `json:"title"`
				URL           string  `json:"url"`
				Content       string  `json:"content"`
				Score         float64 `json:"score"`
				PublishedDate string  `json:"published_date"`
			} `json:"results"`
		}
		if err := doJSON(client, httpReq, &responsePayload); err != nil {
			return nil, err
		}
		results := make([]Result, 0, min(len(responsePayload.Results), req.MaxResults))
		for _, item := range responsePayload.Results {
			score := item.Score
			if result, ok := normalizeResult(item.Title, item.URL, item.Content, item.PublishedDate, &score); ok {
				results = append(results, result)
				if len(results) == req.MaxResults {
					break
				}
			}
		}
		return results, nil
	}
}

func searXNGAdapter(client *http.Client, origin *url.URL) func(context.Context, Request) ([]Result, error) {
	return func(ctx context.Context, req Request) ([]Result, error) {
		target := *origin
		target.Path = "/search"
		query := target.Query()
		query.Set("q", req.Query)
		query.Set("format", "json")
		query.Set("pageno", "1")
		if req.Language != "" {
			query.Set("language", req.Language)
		}
		if req.TimeRange != "" {
			query.Set("time_range", req.TimeRange)
		}
		target.RawQuery = query.Encode()
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, ErrUpstream
		}
		var payload struct {
			Results []struct {
				Title         string   `json:"title"`
				URL           string   `json:"url"`
				Content       string   `json:"content"`
				Score         *float64 `json:"score"`
				PublishedDate string   `json:"publishedDate"`
			} `json:"results"`
		}
		if err := doJSON(client, httpReq, &payload); err != nil {
			return nil, err
		}
		results := make([]Result, 0, min(len(payload.Results), req.MaxResults))
		for _, item := range payload.Results {
			if result, ok := normalizeResult(item.Title, item.URL, item.Content, item.PublishedDate, item.Score); ok {
				results = append(results, result)
				if len(results) == req.MaxResults {
					break
				}
			}
		}
		return results, nil
	}
}

func validLanguage(language string) bool {
	if language == "" {
		return true
	}
	if len(language) > 64 {
		return false
	}
	parts := strings.Split(language, "-")
	if len(parts[0]) < 2 || len(parts[0]) > 8 || !asciiLetters(parts[0]) {
		return false
	}
	for _, part := range parts[1:] {
		if len(part) < 1 || len(part) > 8 || !asciiAlphanumeric(part) {
			return false
		}
	}
	return true
}

func validTimeRange(timeRange string) bool {
	return timeRange == "" || timeRange == "day" || timeRange == "month" || timeRange == "year"
}

func asciiLetters(value string) bool {
	for _, char := range value {
		if char < 'A' || char > 'Z' {
			if char < 'a' || char > 'z' {
				return false
			}
		}
	}
	return true
}

func asciiAlphanumeric(value string) bool {
	for _, char := range value {
		if char >= '0' && char <= '9' {
			continue
		}
		if char >= 'A' && char <= 'Z' {
			continue
		}
		if char < 'a' || char > 'z' {
			return false
		}
	}
	return true
}

func doJSON(client *http.Client, req *http.Request, target any) error {
	resp, err := client.Do(req)
	if err != nil {
		return ErrUpstream
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ErrUpstream
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamResponseSize+1))
	if err != nil || len(body) > maxUpstreamResponseSize {
		return ErrUpstream
	}
	if err := json.Unmarshal(body, target); err != nil {
		return ErrUpstream
	}
	return nil
}

func normalizeResult(title, rawURL, snippet, publishedAt string, score *float64) (Result, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || len(rawURL) > 4096 {
		return Result{}, false
	}
	return Result{
		Title:       truncate(strings.TrimSpace(title), 500),
		URL:         u.String(),
		Snippet:     truncate(strings.TrimSpace(snippet), 4000),
		Score:       score,
		PublishedAt: truncate(strings.TrimSpace(publishedAt), 100),
	}, true
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}
