package search

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTavilyAdapterNormalizesRequestAndResponse(t *testing.T) {
	var seenKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/search" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		seenKey = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Example","url":"https://example.com/a","content":"Snippet","score":0.8,"published_date":"2026-07-01"}]}`))
	}))
	defer upstream.Close()

	svc := testService(t, Config{TavilyAPIKey: "top-secret-key", TavilyOrigin: upstream.URL, Timeout: time.Second}, upstream.Client())
	response, err := svc.Search(context.Background(), Request{Query: "  lynai search  ", Provider: ProviderTavily, MaxResults: 3, Language: "en", TimeRange: "MONTH"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !strings.Contains(seenKey, `"api_key":"top-secret-key"`) || !strings.Contains(seenKey, `"query":"lynai search"`) || !strings.Contains(seenKey, `"max_results":3`) || !strings.Contains(seenKey, `"time_range":"month"`) || strings.Contains(seenKey, `"language"`) {
		t.Fatalf("Tavily request = %s", seenKey)
	}
	if response.Provider != ProviderTavily || len(response.Results) != 1 || response.Results[0].Snippet != "Snippet" || response.Results[0].Score == nil {
		t.Fatalf("Search() response = %#v", response)
	}
}

func TestSearXNGAdapterNormalizesAndCapsResults(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "test query" || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("pageno") != "1" || r.URL.Query().Get("language") != "zh-CN" || r.URL.Query().Get("time_range") != "year" {
			t.Fatalf("query = %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"results":[{"title":"One","url":"https://one.example","content":"First"},{"title":"Bad","url":"file:///etc/passwd","content":"skip"},{"title":"Two","url":"https://two.example","content":"Second"}]}`))
	}))
	defer upstream.Close()

	svc := testService(t, Config{SearXNGOrigin: upstream.URL, Timeout: time.Second}, upstream.Client())
	response, err := svc.Search(context.Background(), Request{Query: "test query", MaxResults: 2, Language: "zh-CN", TimeRange: "year"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if response.Provider != ProviderSearXNG || len(response.Results) != 2 || response.Results[1].Title != "Two" {
		t.Fatalf("Search() response = %#v", response)
	}
}

func TestAutoFallsBackButExplicitProviderDoesNot(t *testing.T) {
	tavily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret upstream detail", http.StatusUnauthorized)
	}))
	defer tavily.Close()
	searxng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"Fallback","url":"https://example.com","content":"ok"}]}`))
	}))
	defer searxng.Close()

	client := tavily.Client()
	svc := testService(t, Config{
		TavilyAPIKey: "secret", TavilyOrigin: tavily.URL,
		SearXNGOrigin: searxng.URL, Timeout: time.Second,
	}, client)
	response, err := svc.Search(context.Background(), Request{Query: "query"})
	if err != nil || response.Provider != ProviderSearXNG {
		t.Fatalf("auto Search() = %#v, %v", response, err)
	}
	if _, err := svc.Search(context.Background(), Request{Query: "query", Provider: ProviderTavily}); !errors.Is(err, ErrUpstream) {
		t.Fatalf("explicit Tavily error = %v", err)
	}
}

func TestSearchValidationAndUnavailableProviders(t *testing.T) {
	svc := testService(t, Config{Timeout: time.Second}, &http.Client{})
	for _, req := range []Request{
		{},
		{Query: "query", Provider: "https://attacker.example"},
		{Query: "query", MaxResults: 11},
		{Query: "query", Language: "not a tag!"},
		{Query: "query", TimeRange: "week"},
		{Query: strings.Repeat("x", 1001)},
	} {
		if _, err := svc.Search(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Search(%#v) error = %v", req, err)
		}
	}
	if _, err := svc.Search(context.Background(), Request{Query: "query"}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("unconfigured Search() error = %v", err)
	}
	if _, err := svc.Search(context.Background(), Request{Query: "query", Provider: ProviderTavily}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("unconfigured Tavily error = %v", err)
	}
}

func TestConfiguredOriginAndResponseLimits(t *testing.T) {
	validated := ""
	_, err := newService(Config{TavilyAPIKey: "secret", TavilyOrigin: "https://example.com/base", Timeout: time.Second}, &http.Client{}, func(origin string) error {
		validated = origin
		return nil
	})
	if err == nil || validated != "" {
		t.Fatalf("path origin accepted: validated %q, error %v", validated, err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxUpstreamResponseSize+1)))
	}))
	defer upstream.Close()
	svc := testService(t, Config{SearXNGOrigin: upstream.URL, Timeout: time.Second}, upstream.Client())
	if _, err := svc.Search(context.Background(), Request{Query: "query"}); !errors.Is(err, ErrUpstream) {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	destinationCalled := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalled = true
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer origin.Close()
	client := origin.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect blocked") }
	svc := testService(t, Config{SearXNGOrigin: origin.URL, Timeout: time.Second}, client)
	if _, err := svc.Search(context.Background(), Request{Query: "query"}); !errors.Is(err, ErrUpstream) {
		t.Fatalf("redirect error = %v", err)
	}
	if destinationCalled {
		t.Fatal("redirect destination was called")
	}
}

func TestAutoFallbackSharesTotalTimeout(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer first.Close()
	secondCalled := false
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalled = true
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer second.Close()
	svc := testService(t, Config{
		TavilyAPIKey: "secret", TavilyOrigin: first.URL,
		SearXNGOrigin: second.URL, Timeout: 20 * time.Millisecond,
	}, first.Client())
	if _, err := svc.Search(context.Background(), Request{Query: "query"}); !errors.Is(err, ErrUpstream) {
		t.Fatalf("Search() error = %v", err)
	}
	if secondCalled {
		t.Fatal("fallback started after the total timeout")
	}
}

func testService(t *testing.T, cfg Config, client *http.Client) *Service {
	t.Helper()
	svc, err := newService(cfg, client, func(string) error { return nil })
	if err != nil {
		t.Fatalf("newService() error = %v", err)
	}
	return svc
}
