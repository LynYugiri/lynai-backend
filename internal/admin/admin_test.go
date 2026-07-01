package admin_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/lynai/backend/internal/testutil"
)

const userPassword = "secret123"

func TestAdminPanelLoginUsersAndCSRF(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()

	client := adminClient(t)
	loginAdmin(t, client, ts.URL, adminPhone, adminPassword)

	body := getAdminPage(t, client, ts.URL+"/admin/users")
	csrf := extractCSRF(t, body)

	resp := postForm(t, client, ts.URL+"/admin/users/create", url.Values{
		"phone":       {"13900000001"},
		"password":    {userPassword},
		"displayName": {"Second Admin"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create admin without csrf = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	postFormFollow(t, client, ts.URL+"/admin/users/create", url.Values{
		"_csrf":       {csrf},
		"phone":       {"13900000001"},
		"password":    {userPassword},
		"displayName": {"Second Admin"},
	})
	body = getAdminPage(t, client, ts.URL+"/admin/users")
	if !strings.Contains(body, "13900000001") || !strings.Contains(body, "Second Admin") {
		t.Fatal("created admin is not visible on users page")
	}
}

func TestAdminPanelPluginManage(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()

	client := adminClient(t)
	loginAdmin(t, client, ts.URL, adminPhone, adminPassword)
	userToken := registerAndGetToken(t, ts.URL, "13800000088")
	submitPlugin(t, ts.URL, userToken, "admin-test-plugin", "Admin Test", "1.0.0")

	body := getAdminPage(t, client, ts.URL+"/admin/plugins/admin-test-plugin")
	if !strings.Contains(body, "Admin Test") {
		t.Fatal("plugin detail page does not show submitted plugin")
	}
	csrf := extractCSRF(t, body)

	postFormFollow(t, client, ts.URL+"/admin/plugins/admin-test-plugin/edit", url.Values{
		"_csrf":       {csrf},
		"name":        {"Edited Plugin"},
		"version":     {"1.0.1"},
		"category":    {"tools"},
		"description": {"Edited description"},
	})
	body = getAdminPage(t, client, ts.URL+"/admin/plugins/admin-test-plugin")
	if !strings.Contains(body, "Edited Plugin") || !strings.Contains(body, "Edited description") {
		t.Fatal("plugin edit did not persist")
	}
	csrf = extractCSRF(t, body)

	postFormFollow(t, client, ts.URL+"/admin/plugins/admin-test-plugin/approve", url.Values{"_csrf": {csrf}, "redirect": {"/admin/plugins/admin-test-plugin"}})
	body = getAdminPage(t, client, ts.URL+"/admin/plugins/admin-test-plugin")
	if !strings.Contains(body, "已上架") {
		t.Fatal("plugin approve did not update status")
	}
	csrf = extractCSRF(t, body)

	postFormFollow(t, client, ts.URL+"/admin/plugins/admin-test-plugin/unpublish", url.Values{"_csrf": {csrf}})
	body = getAdminPage(t, client, ts.URL+"/admin/plugins/admin-test-plugin")
	if !strings.Contains(body, "待审核") {
		t.Fatal("plugin unpublish did not move status back to pending")
	}
	csrf = extractCSRF(t, body)

	postFormFollow(t, client, ts.URL+"/admin/plugins/admin-test-plugin/delete", url.Values{"_csrf": {csrf}})
	resp, err := client.Get(ts.URL + "/admin/plugins/admin-test-plugin")
	if err != nil {
		t.Fatalf("get deleted plugin: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted plugin detail status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func adminClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func loginAdmin(t *testing.T, client *http.Client, baseURL, phone, password string) {
	t.Helper()
	postFormFollow(t, client, baseURL+"/admin/login", url.Values{"phone": {phone}, "password": {password}})
}

func getAdminPage(t *testing.T, client *http.Client, target string) string {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("get %s: %v", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s status = %d, want 200", target, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func postForm(t *testing.T, client *http.Client, target string, values url.Values) *http.Response {
	t.Helper()
	resp, err := client.PostForm(target, values)
	if err != nil {
		t.Fatalf("post %s: %v", target, err)
	}
	return resp
}

func postFormFollow(t *testing.T, client *http.Client, target string, values url.Values) {
	t.Helper()
	resp := postForm(t, client, target, values)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post %s final status = %d, want 200", target, resp.StatusCode)
	}
}

func extractCSRF(t *testing.T, body string) string {
	t.Helper()
	matches := regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindStringSubmatch(body)
	if len(matches) != 2 {
		t.Fatal("csrf token not found")
	}
	return matches[1]
}

func registerAndGetToken(t *testing.T, tsURL, phone string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"phone": phone, "password": userPassword})
	resp, err := http.Post(tsURL+"/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result["token"].(map[string]interface{})["accessToken"].(string)
}

func submitPlugin(t *testing.T, tsURL, token, id, name, version string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("zip", id+".zip")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(createPluginZip(t, id, name, version))
	writer.Close()
	req, _ := http.NewRequest("POST", tsURL+"/market/plugins/submit", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("submit plugin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit plugin status = %d, want 200", resp.StatusCode)
	}
}

func createPluginZip(t *testing.T, id, name, version string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	writer := zip.NewWriter(buf)
	manifest := map[string]interface{}{
		"id":          id,
		"name":        name,
		"version":     version,
		"author":      "test-author",
		"description": "A test plugin",
		"permissions": []string{"network"},
	}
	manifestJSON, _ := json.Marshal(manifest)
	file, err := writer.Create("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	file.Write(manifestJSON)
	mainFile, err := writer.Create("main.lua")
	if err != nil {
		t.Fatal(err)
	}
	mainFile.Write([]byte("-- test plugin\n"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
