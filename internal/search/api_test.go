package search_test

import (
	"net/http"
	"testing"

	"github.com/lynai/backend/internal/testutil"
)

func TestWebSearchRequiresAuthentication(t *testing.T) {
	_, _, server, cleanup := testutil.SetupTest()
	defer cleanup()

	resp := testutil.PostJSON(t, server.URL+"/search/web", map[string]any{"query": "test"})
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)

	token := testutil.RegisterAndGetToken(t, server.URL, "13800008881", "password123")
	req := testutil.NewJSONRequest(t, http.MethodPost, server.URL+"/search/web", map[string]any{"query": "test"})
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusServiceUnavailable)
}
