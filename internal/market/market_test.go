package market_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

func createPluginZipWithExactSize(t *testing.T, id, name, version string, size int) []byte {
	t.Helper()
	build := func(paddingSize int) []byte {
		buf := &bytes.Buffer{}
		writer := zip.NewWriter(buf)
		manifest, err := json.Marshal(map[string]interface{}{
			"id": id, "name": name, "version": version,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifestFile, err := writer.Create("plugin.json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manifestFile.Write(manifest); err != nil {
			t.Fatal(err)
		}
		paddingHeader := &zip.FileHeader{Name: "padding.bin", Method: zip.Store}
		paddingFile, err := writer.CreateHeader(paddingHeader)
		if err != nil {
			t.Fatal(err)
		}
		if paddingSize > 0 {
			if _, err := paddingFile.Write(bytes.Repeat([]byte{0}, paddingSize)); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	base := build(0)
	if len(base) > size {
		t.Fatalf("minimum plugin ZIP size = %d, exceeds target %d", len(base), size)
	}
	result := build(size - len(base))
	if len(result) != size {
		t.Fatalf("plugin ZIP size = %d, want %d", len(result), size)
	}
	if _, err := zip.NewReader(bytes.NewReader(result), int64(len(result))); err != nil {
		t.Fatalf("exact-size fixture is not a valid ZIP: %v", err)
	}
	return result
}

func createZipWithEntries(t *testing.T, entries ...struct {
	name string
	body string
}) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for _, entry := range entries {
		writer, err := zw.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
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

func submitPluginResponse(t *testing.T, ts *testutil.TestServer, token string, zipBytes []byte) *http.Response {
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
	return testutil.Do(t, req)
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

func TestSubmitCannotTakeOverAnotherUsersPlugin(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	ownerToken := testutil.RegisterAndGetToken(t, ts.URL, "13800000011", testPassword)
	attackerToken := testutil.RegisterAndGetToken(t, ts.URL, "13800000012", testPassword)
	original := createPluginZip(t, "owned-plugin", "Original", "1.0.0")
	submitPlugin(t, ts, ownerToken, original)

	adminToken := testutil.LoginAndGetToken(t, ts.URL, adminPhone, adminPassword)
	req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/market/plugins/owned-plugin/approve", nil)
	testutil.SetBearer(req, adminToken)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	replacement := createPluginZip(t, "owned-plugin", "Taken Over", "1.0.0")
	resp = submitPluginResponse(t, ts, attackerToken, replacement)
	testutil.RequireStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()

	resp, err := http.Get(ts.URL + "/market/plugins/owned-plugin/download")
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusOK)
	downloaded := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if !bytes.Equal(downloaded, original) {
		t.Fatal("failed takeover replaced the owner's ZIP")
	}
}

func TestSubmitRejectsOversizedUpload(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000013", testPassword)
	zipBytes := createPluginZipWithExactSize(t, "oversized-plugin", "Oversized", "1.0.0", (16<<20)+1)
	resp := submitPluginResponse(t, ts, token, zipBytes)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusRequestEntityTooLarge)
}

func TestSubmitAcceptsExactly16MiBZip(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000019", testPassword)
	zipBytes := createPluginZipWithExactSize(t, "limit-plugin", "Limit", "1.0.0", 16<<20)
	resp := submitPluginResponse(t, ts, token, zipBytes)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
}

func TestSubmitRejectsUnsafeManifestArchiveLayout(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000016", testPassword)
	manifest := `{"id":"safe-plugin","name":"Safe","version":"1.0.0"}`

	tests := []struct {
		name    string
		entries []struct{ name, body string }
	}{
		{name: "nested manifest", entries: []struct{ name, body string }{{"nested/plugin.json", manifest}}},
		{name: "duplicate manifest", entries: []struct{ name, body string }{{"plugin.json", manifest}, {"plugin.json", manifest}}},
		{name: "backslash path", entries: []struct{ name, body string }{{`folder\plugin.json`, manifest}, {"plugin.json", manifest}}},
		{name: "dot segment", entries: []struct{ name, body string }{{"./plugin.json", manifest}}},
		{name: "parent segment", entries: []struct{ name, body string }{{"folder/../plugin.json", manifest}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := submitPluginResponse(t, ts, token, createZipWithEntries(t, tc.entries...))
			defer resp.Body.Close()
			testutil.RequireStatus(t, resp, http.StatusBadRequest)
		})
	}
}

func TestSubmitRejectsInvalidSemVer(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000017", testPassword)
	for _, version := range []string{"1", "1.0", "v1.0.0", "1.0.0_bad"} {
		t.Run(version, func(t *testing.T) {
			resp := submitPluginResponse(t, ts, token, createPluginZip(t, "invalid-version", "Invalid", version))
			defer resp.Body.Close()
			testutil.RequireStatus(t, resp, http.StatusBadRequest)
		})
	}
}

func TestSubmitOnlyPublishesAbsoluteHTTPIcons(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000018", testPassword)
	for _, tc := range []struct {
		name string
		icon string
		want interface{}
	}{
		{name: "relative", icon: "icons/plugin.png", want: nil},
		{name: "javascript", icon: "javascript:alert(1)", want: nil},
		{name: "https", icon: "https://cdn.example/plugin.png", want: "https://cdn.example/plugin.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := fmt.Sprintf(`{"id":"icon-%s","name":"Icon","version":"1.0.0","icon":%q}`, tc.name, tc.icon)
			result := submitPlugin(t, ts, token, createZipWithEntries(t, struct{ name, body string }{"plugin.json", manifest}))
			if result["iconUrl"] != tc.want {
				t.Fatalf("iconUrl = %v, want %v", result["iconUrl"], tc.want)
			}
		})
	}
}

func TestSubmitRejectsOversizedManifest(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	f, err := zw.Create("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, `{"id":"large-manifest","name":"Large","version":"1.0.0","description":"`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte("a"), 1<<20)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, `"}`); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000014", testPassword)
	resp := submitPluginResponse(t, ts, token, buf.Bytes())
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
}

func TestFailedSubmissionDoesNotOverwriteExistingZip(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000015", testPassword)
	original := createPluginZip(t, "preserve-plugin", "Preserve", "1.0.0")
	submitPlugin(t, ts, token, original)

	adminToken := testutil.LoginAndGetToken(t, ts.URL, adminPhone, adminPassword)
	req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/market/plugins/preserve-plugin/approve", nil)
	testutil.SetBearer(req, adminToken)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	f, err := zw.Create("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, `{"id":"preserve-plugin","name":"Replacement","version":"1.0.0","description":"`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte("a"), 1<<20)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, `"}`); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	resp = submitPluginResponse(t, ts, token, buf.Bytes())
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/market/plugins/preserve-plugin/download")
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusOK)
	downloaded := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if !bytes.Equal(downloaded, original) {
		t.Fatal("failed submission replaced the existing ZIP")
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
	if got := resp.Header.Get("Content-Type"); got != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("ETag"); got == "" {
		t.Fatal("ETag should be set when SHA-256 is available")
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="download-me-1.0.0.zip"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
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

func TestCheckUpdatesUsesSemVerOrdering(t *testing.T) {
	tests := []struct {
		name      string
		installed string
		market    string
		want      int
	}{
		{name: "higher major", installed: "1.9.9", market: "2.0.0", want: 1},
		{name: "numeric comparison", installed: "1.9.0", market: "1.10.0", want: 1},
		{name: "equal", installed: "1.0.0", market: "1.0.0", want: 0},
		{name: "installed newer", installed: "2.0.0", market: "1.0.0", want: 0},
		{name: "release beats prerelease", installed: "1.0.0-beta.1", market: "1.0.0", want: 1},
		{name: "loose installed", installed: "v1.0", market: "2.0.0", want: 1},
		{name: "invalid installed updated", installed: "development", market: "2.0.0", want: 1},
	}
	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
			defer cleanup()
			token := testutil.RegisterAndGetToken(t, ts.URL, fmt.Sprintf("13900000%03d", index), testPassword)
			submitPlugin(t, ts, token, createPluginZip(t, "semver-plugin", "SemVer", tc.market))
			adminToken := testutil.LoginAndGetToken(t, ts.URL, adminPhone, adminPassword)
			req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/market/plugins/semver-plugin/approve", nil)
			testutil.SetBearer(req, adminToken)
			resp := testutil.Do(t, req)
			testutil.RequireStatus(t, resp, http.StatusOK)
			resp.Body.Close()

			req = testutil.NewJSONRequest(t, http.MethodPost, ts.URL+"/market/updates", map[string]interface{}{
				"installed": []map[string]string{{"id": "semver-plugin", "version": tc.installed}},
			})
			testutil.SetBearer(req, token)
			resp = testutil.Do(t, req)
			testutil.RequireStatus(t, resp, http.StatusOK)
			var result map[string]interface{}
			testutil.DecodeJSON(t, resp, &result)
			resp.Body.Close()
			updates, _ := result["updates"].([]interface{})
			if len(updates) != tc.want {
				t.Fatalf("updates len = %d, want %d", len(updates), tc.want)
			}
		})
	}
}

func TestMarketJSONLimitsAndUpdateValidation(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000020", testPassword)
	adminToken := testutil.LoginAndGetToken(t, ts.URL, adminPhone, adminPassword)
	submitPlugin(t, ts, token, createPluginZip(t, "reject-limit", "Reject Limit", "1.0.0"))

	for _, tc := range []struct {
		path  string
		token string
	}{
		{path: "/market/updates", token: token},
		{path: "/market/plugins/reject-limit/reject", token: adminToken},
	} {
		body := strings.NewReader(`{"padding":"` + strings.Repeat("x", 600<<10) + `"}`)
		req := testutil.NewRequest(t, http.MethodPost, ts.URL+tc.path, body)
		req.Header.Set("Content-Type", "application/json")
		testutil.SetBearer(req, tc.token)
		resp := testutil.Do(t, req)
		testutil.RequireStatus(t, resp, http.StatusRequestEntityTooLarge)
		resp.Body.Close()
	}

	tests := []map[string]interface{}{
		{"installed": make([]map[string]string, 1025)},
		{"installed": []map[string]string{{"id": strings.Repeat("i", 129), "version": "1.0.0"}}},
		{"installed": []map[string]string{{"id": "plugin", "version": strings.Repeat("v", 129)}}},
		{"installed": []map[string]string{{"id": "plugin", "version": ""}}},
	}
	for _, body := range tests {
		req := testutil.NewJSONRequest(t, http.MethodPost, ts.URL+"/market/updates", body)
		testutil.SetBearer(req, token)
		resp := testutil.Do(t, req)
		testutil.RequireStatus(t, resp, http.StatusBadRequest)
		resp.Body.Close()
	}
}

func TestSubmitRejectsDifferentArchiveForSameVersion(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13800000021", testPassword)
	submitPlugin(t, ts, token, createPluginZip(t, "immutable-version", "First", "1.0.0"))
	resp := submitPluginResponse(t, ts, token, createPluginZip(t, "immutable-version", "Changed", "1.0.0"))
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusConflict)
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
