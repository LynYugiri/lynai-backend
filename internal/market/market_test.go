package market_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/lynai/backend/internal/testutil"
)

const testPassword = "secret123"

// createPluginZip builds a minimal plugin ZIP with a plugin.json manifest
// and returns the raw bytes.
func createPluginZip(t *testing.T, id, name, version string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	w := zip.NewWriter(buf)

	manifest := map[string]interface{}{
		"id":          id,
		"name":        name,
		"version":     version,
		"author":      "test-author",
		"description": "A test plugin",
		"permissions": []string{"network", "storage"},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	f, err := w.Create("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(manifestJSON); err != nil {
		t.Fatal(err)
	}

	f2, err := w.Create("main.lua")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Write([]byte("-- test plugin\n")); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// submitPlugin uploads a plugin ZIP via multipart form and returns the response.
func submitPlugin(t *testing.T, ts *testutil.TestServer, token string, zipBytes []byte) map[string]interface{} {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("zip", "plugin.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(zipBytes); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/market/plugins/submit", body)
	testutil.SetBearer(req, token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	return result
}

func TestSubmitPlugin(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000001", testPassword)
	zipBytes := createPluginZip(t, "test-plugin", "Test Plugin", "1.0.0")
	result := submitPlugin(t, ts, token, zipBytes)

	if result["id"] != "test-plugin" {
		t.Fatalf("id = %v, want test-plugin", result["id"])
	}
	if result["name"] != "Test Plugin" {
		t.Fatalf("name = %v, want Test Plugin", result["name"])
	}
	if result["version"] != "1.0.0" {
		t.Fatalf("version = %v, want 1.0.0", result["version"])
	}
	if result["sha256"] == nil || result["sha256"] == "" {
		t.Fatal("sha256 should be set")
	}
	if result["status"] != "pending" {
		t.Fatalf("status = %v, want pending", result["status"])
	}
	if result["downloadUrl"] != "" {
		t.Fatalf("downloadUrl = %v, want empty for pending submission", result["downloadUrl"])
	}
	perms, _ := result["permissions"].([]interface{})
	if len(perms) != 2 {
		t.Fatalf("permissions len = %d, want 2", len(perms))
	}
}

func TestSubmitRejectsUnsafeManifestPaths(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000009", testPassword)
	for _, tc := range []struct {
		name    string
		id      string
		version string
	}{
		{name: "id traversal", id: "../evil", version: "1.0.0"},
		{name: "id separator", id: "evil/plugin", version: "1.0.0"},
		{name: "version traversal", id: "safe-plugin", version: "../evil"},
		{name: "version separator", id: "safe-plugin", version: "1/evil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zipBytes := createPluginZip(t, tc.id, "Unsafe", tc.version)
			body := &bytes.Buffer{}
			mw := multipart.NewWriter(body)
			part, err := mw.CreateFormFile("zip", "plugin.zip")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(zipBytes); err != nil {
				t.Fatal(err)
			}
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}

			req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/market/plugins/submit", body)
			testutil.SetBearer(req, token)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			resp := testutil.Do(t, req)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("submit unsafe manifest status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestListPluginsOnlyApproved(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000002", testPassword)
	zipBytes := createPluginZip(t, "pending-plugin", "Pending", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	// Public list should be empty — nothing approved yet
	resp, err := http.Get(ts.URL + "/market/plugins")
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()

	entries, _ := result["entries"].([]interface{})
	if len(entries) != 0 {
		t.Fatalf("entries len = %d, want 0 (no approved plugins)", len(entries))
	}
}

func TestApproveFlow(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	// Submit as regular user
	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000003", testPassword)
	zipBytes := createPluginZip(t, "approve-me", "Approve Me", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	// Admin approves
	adminToken := testutil.LoginAndGetToken(t, ts.URL, adminPhone, adminPassword)
	req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/market/plugins/approve-me/approve", nil)
	testutil.SetBearer(req, adminToken)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Now public list should contain it
	resp, err := http.Get(ts.URL + "/market/plugins")
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()
	entries, _ := result["entries"].([]interface{})
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	entry := entries[0].(map[string]interface{})
	if entry["id"] != "approve-me" {
		t.Fatalf("entry id = %v, want approve-me", entry["id"])
	}
}

func TestRejectFlow(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000004", testPassword)
	zipBytes := createPluginZip(t, "reject-me", "Reject Me", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	adminToken := testutil.LoginAndGetToken(t, ts.URL, adminPhone, adminPassword)
	req := testutil.NewJSONRequest(t, http.MethodPost, ts.URL+"/market/plugins/reject-me/reject", map[string]string{"reason": "bad plugin"})
	testutil.SetBearer(req, adminToken)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Public list should still be empty
	resp, err := http.Get(ts.URL + "/market/plugins")
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()
	entries, _ := result["entries"].([]interface{})
	if len(entries) != 0 {
		t.Fatalf("entries len = %d, want 0", len(entries))
	}
}

func TestRejectMalformedJSON(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000010", testPassword)
	zipBytes := createPluginZip(t, "reject-bad-json", "Reject Bad JSON", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	adminToken := testutil.LoginAndGetToken(t, ts.URL, adminPhone, adminPassword)
	req := testutil.NewRequest(
		t,
		http.MethodPost,
		ts.URL+"/market/plugins/reject-bad-json/reject",
		strings.NewReader("{"),
	)
	testutil.SetBearer(req, adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
}

func TestMySubmissions(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000005", testPassword)
	zipBytes := createPluginZip(t, "my-plugin", "My Plugin", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/market/submissions/mine", nil)
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()

	subs, _ := result["submissions"].([]interface{})
	if len(subs) != 1 {
		t.Fatalf("submissions len = %d, want 1", len(subs))
	}
	sub := subs[0].(map[string]interface{})
	if sub["status"] != "pending" {
		t.Fatalf("submission status = %v, want pending", sub["status"])
	}
	if sub["downloadUrl"] != "" {
		t.Fatalf("submission downloadUrl = %v, want empty while pending", sub["downloadUrl"])
	}
}

func TestDownloadPlugin(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000006", testPassword)
	zipBytes := createPluginZip(t, "download-me", "Download Me", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	// Approve it
	adminToken := testutil.LoginAndGetToken(t, ts.URL, adminPhone, adminPassword)
	req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/market/plugins/download-me/approve", nil)
	testutil.SetBearer(req, adminToken)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Download
	resp, err := http.Get(ts.URL + "/market/plugins/download-me/download")
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusOK)
	data := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()

	// Verify it's a valid ZIP
	_, err = zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("downloaded file is not a valid ZIP: %v", err)
	}
}

func TestCheckUpdates(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	// Submit and approve version 1.0.0
	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000007", testPassword)
	zipBytes := createPluginZip(t, "update-me", "Update Me", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	adminToken := testutil.LoginAndGetToken(t, ts.URL, adminPhone, adminPassword)
	req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/market/plugins/update-me/approve", nil)
	testutil.SetBearer(req, adminToken)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Submit version 2.0.0 and approve
	zipBytes2 := createPluginZip(t, "update-me", "Update Me", "2.0.0")
	submitPlugin(t, ts, token, zipBytes2)
	req = testutil.NewRequest(t, http.MethodPost, ts.URL+"/market/plugins/update-me/approve", nil)
	testutil.SetBearer(req, adminToken)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Check updates — installed is 1.0.0, market is 2.0.0
	req = testutil.NewJSONRequest(t, http.MethodPost, ts.URL+"/market/updates", map[string]interface{}{
		"installed": []map[string]string{
			{"id": "update-me", "version": "1.0.0"},
		},
	})
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()

	updates, _ := result["updates"].([]interface{})
	if len(updates) != 1 {
		t.Fatalf("updates len = %d, want 1", len(updates))
	}
	entry := updates[0].(map[string]interface{})
	if entry["version"] != "2.0.0" {
		t.Fatalf("update version = %v, want 2.0.0", entry["version"])
	}
}

func TestSubmitRequiresAuth(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	resp, err := http.Post(ts.URL+"/market/plugins/submit", "multipart/form-data", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestPendingRequiresAdmin(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000008", testPassword)
	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/market/plugins/pending", nil)
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}
