package search

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestHandlerStrictRequestAndSecretSafeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "api key top-secret-key rejected", http.StatusUnauthorized)
	}))
	defer upstream.Close()
	svc := testService(t, Config{TavilyAPIKey: "top-secret-key", TavilyOrigin: upstream.URL, Timeout: time.Second}, upstream.Client())
	router := gin.New()
	router.POST("/search/web", NewHandler(svc).Web)

	for _, body := range []string{
		`{"query":"test","upstream":"https://attacker.example"}`,
		`{"query":"test"}{"query":"again"}`,
		`{"query":"test","max_results":5}`,
		`{"query":"test","time_range":"month"}`,
	} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/search/web", strings.NewReader(body))
		router.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d", body, recorder.Code)
		}
	}

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/search/web", strings.NewReader(`{"query":"test","provider":"tavily"}`))
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), "top-secret-key") || strings.Contains(recorder.Body.String(), "rejected") {
		t.Fatalf("upstream error response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerRequestBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := testService(t, Config{Timeout: time.Second}, &http.Client{})
	router := gin.New()
	router.POST("/search/web", NewHandler(svc).Web)
	body := append([]byte(`{"query":"`), bytes.Repeat([]byte("x"), maxRequestBodySize)...)
	body = append(body, []byte(`"}`)...)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/search/web", bytes.NewReader(body)))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
