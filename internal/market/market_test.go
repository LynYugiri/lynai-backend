package market_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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
	manifestJSON, _ := json.Marshal(manifest)

	f, err := w.Create("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(manifestJSON)

	f2, err := w.Create("main.lua")
	if err != nil {
		t.Fatal(err)
	}
	f2.Write([]byte("-- test plugin\n"))

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// registerAndLogin registers a user and returns their Bearer token.
func registerAndLogin(t *testing.T, ts *httptest.Server, phone string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"phone":    phone,
		"password": testPassword,
	})
	resp, _ := http.Post(ts.URL+"/auth/register", "application/json", bytes.NewReader(body))
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	return result["token"].(map[string]interface{})["accessToken"].(string)
}

// loginAdmin logs in as the seeded admin and returns the Bearer token.
func loginAdmin(t *testing.T, ts *httptest.Server, adminPhone, adminPassword string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"phone":    adminPhone,
		"password": adminPassword,
	})
	resp, _ := http.Post(ts.URL+"/auth/login", "application/json", bytes.NewReader(body))
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	return result["token"].(map[string]interface{})["accessToken"].(string)
}

// submitPlugin uploads a plugin ZIP via multipart form and returns the response.
func submitPlugin(t *testing.T, ts *httptest.Server, token string, zipBytes []byte) map[string]interface{} {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("zip", "plugin.zip")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(zipBytes)
	mw.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/market/plugins/submit", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("submit status = %d, body = %s", resp.StatusCode, b)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	return result
}

func TestSubmitPlugin(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := registerAndLogin(t, ts, "13800000001")
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

	token := registerAndLogin(t, ts, "13800000009")
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
			part.Write(zipBytes)
			mw.Close()

			req, _ := http.NewRequest("POST", ts.URL+"/market/plugins/submit", body)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
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

	token := registerAndLogin(t, ts, "13800000002")
	zipBytes := createPluginZip(t, "pending-plugin", "Pending", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	// Public list should be empty — nothing approved yet
	resp, _ := http.Get(ts.URL + "/market/plugins")
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
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
	token := registerAndLogin(t, ts, "13800000003")
	zipBytes := createPluginZip(t, "approve-me", "Approve Me", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	// Admin approves
	adminToken := loginAdmin(t, ts, adminPhone, adminPassword)
	req, _ := http.NewRequest("POST", ts.URL+"/market/plugins/approve-me/approve", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("approve status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Now public list should contain it
	resp, _ = http.Get(ts.URL + "/market/plugins")
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
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

	token := registerAndLogin(t, ts, "13800000004")
	zipBytes := createPluginZip(t, "reject-me", "Reject Me", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	adminToken := loginAdmin(t, ts, adminPhone, adminPassword)
	body, _ := json.Marshal(map[string]string{"reason": "bad plugin"})
	req, _ := http.NewRequest("POST", ts.URL+"/market/plugins/reject-me/reject", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("reject status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Public list should still be empty
	resp, _ = http.Get(ts.URL + "/market/plugins")
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	entries, _ := result["entries"].([]interface{})
	if len(entries) != 0 {
		t.Fatalf("entries len = %d, want 0", len(entries))
	}
}

func TestMySubmissions(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := registerAndLogin(t, ts, "13800000005")
	zipBytes := createPluginZip(t, "my-plugin", "My Plugin", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	req, _ := http.NewRequest("GET", ts.URL+"/market/submissions/mine", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("mine status = %d, want 200", resp.StatusCode)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
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

	token := registerAndLogin(t, ts, "13800000006")
	zipBytes := createPluginZip(t, "download-me", "Download Me", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	// Approve it
	adminToken := loginAdmin(t, ts, adminPhone, adminPassword)
	req, _ := http.NewRequest("POST", ts.URL+"/market/plugins/download-me/approve", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Download
	resp, err := http.Get(ts.URL + "/market/plugins/download-me/download")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("download status = %d, want 200", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
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
	token := registerAndLogin(t, ts, "13800000007")
	zipBytes := createPluginZip(t, "update-me", "Update Me", "1.0.0")
	submitPlugin(t, ts, token, zipBytes)

	adminToken := loginAdmin(t, ts, adminPhone, adminPassword)
	req, _ := http.NewRequest("POST", ts.URL+"/market/plugins/update-me/approve", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Submit version 2.0.0 and approve
	zipBytes2 := createPluginZip(t, "update-me", "Update Me", "2.0.0")
	submitPlugin(t, ts, token, zipBytes2)
	req, _ = http.NewRequest("POST", ts.URL+"/market/plugins/update-me/approve", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	// Check updates — installed is 1.0.0, market is 2.0.0
	body, _ := json.Marshal(map[string]interface{}{
		"installed": []map[string]string{
			{"id": "update-me", "version": "1.0.0"},
		},
	})
	req, _ = http.NewRequest("POST", ts.URL+"/market/updates", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("updates status = %d, want 200", resp.StatusCode)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
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
	if resp.StatusCode != 401 {
		t.Fatalf("submit without auth status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPendingRequiresAdmin(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := registerAndLogin(t, ts, "13800000008")
	req, _ := http.NewRequest("GET", ts.URL+"/market/plugins/pending", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 {
		t.Fatalf("pending as non-admin status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
}
