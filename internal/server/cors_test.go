package server_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lynai/backend/internal/testutil"
)

func TestCORSAllowsExpectedGenerationHeader(t *testing.T) {
	_, _, server, cleanup := testutil.SetupTest()
	defer cleanup()
	req := testutil.NewRequest(t, http.MethodOptions, server.URL+"/sync/changes", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type, x-lynai-expected-generation")
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "X-LynAI-Expected-Generation") {
		t.Fatalf("allow headers = %q", resp.Header.Get("Access-Control-Allow-Headers"))
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("allow origin = %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), http.MethodPost) {
		t.Fatalf("allow methods = %q", resp.Header.Get("Access-Control-Allow-Methods"))
	}
}
