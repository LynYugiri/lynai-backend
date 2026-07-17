package admin_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
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

func TestAdminRoleDemotionTakesEffectImmediately(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()

	owner := adminClient(t)
	loginAdmin(t, owner, ts.URL, adminPhone, adminPassword)
	usersBody := getAdminPage(t, owner, ts.URL+"/admin/users")
	csrf := extractCSRF(t, usersBody)
	postFormFollow(t, owner, ts.URL+"/admin/users/create", url.Values{
		"_csrf": {csrf}, "phone": {"13900000002"}, "password": {userPassword}, "displayName": {"Demoted Admin"},
	})

	login := doAdminAuthRequest(t, ts.URL, "13900000002", userPassword)
	userID := login["user"].(map[string]interface{})["id"].(string)
	accessToken := login["token"].(map[string]interface{})["accessToken"].(string)
	demoted := adminClient(t)
	loginAdmin(t, demoted, ts.URL, "13900000002", userPassword)
	getAdminPage(t, demoted, ts.URL+"/admin/")

	usersBody = getAdminPage(t, owner, ts.URL+"/admin/users")
	csrf = extractCSRF(t, usersBody)
	postFormFollow(t, owner, ts.URL+"/admin/users/"+userID+"/demote", url.Values{"_csrf": {csrf}})

	resp, err := demoted.Get(ts.URL + "/admin/")
	if err != nil {
		t.Fatalf("get admin after demotion: %v", err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.Path != "/admin/login" {
		t.Fatalf("demoted admin final path = %s, want /admin/login", resp.Request.URL.Path)
	}

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/market/plugins/pending", nil)
	testutil.SetBearer(req, accessToken)
	apiResp := testutil.Do(t, req)
	defer apiResp.Body.Close()
	testutil.RequireStatus(t, apiResp, http.StatusForbidden)
}

func doAdminAuthRequest(t *testing.T, baseURL, phone, password string) map[string]interface{} {
	t.Helper()
	resp := testutil.PostJSON(t, baseURL+"/auth/login", map[string]string{"phone": phone, "password": password})
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	return result
}

func TestAdminPanelPluginManage(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()

	client := adminClient(t)
	loginAdmin(t, client, ts.URL, adminPhone, adminPassword)
	userToken := testutil.RegisterAndGetToken(t, ts.URL, "13800000088", userPassword)
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

	resp := postForm(t, client, ts.URL+"/admin/plugins/admin-test-plugin/approve", url.Values{"_csrf": {csrf}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("repeat approve status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

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

func TestAdminPanelRelayProviderManage(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()

	client := adminClient(t)
	loginAdmin(t, client, ts.URL, adminPhone, adminPassword)

	body := getAdminPage(t, client, ts.URL+"/admin/relay/new")
	csrf := extractCSRF(t, body)
	postFormFollow(t, client, ts.URL+"/admin/relay/new", url.Values{
		"_csrf":     {csrf},
		"name":      {"OpenAI Test"},
		"endpoint":  {"https://api.example.com/v1"},
		"apiFormat": {"openai"},
		"apiKey":    {"secret-key"},
		"models":    {"gpt-test\ngpt-other"},
		"enabled":   {"on"},
	})
	body = getAdminPage(t, client, ts.URL+"/admin/relay")
	if !strings.Contains(body, "OpenAI Test") || !strings.Contains(body, "gpt-test") || !strings.Contains(body, "openai") {
		t.Fatal("created relay provider is not visible")
	}
	if strings.Contains(body, "secret-key") {
		t.Fatal("relay provider page leaked api key")
	}
}

func TestAdminPanelRelayProviderModelRows(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()

	client := adminClient(t)
	loginAdmin(t, client, ts.URL, adminPhone, adminPassword)

	body := getAdminPage(t, client, ts.URL+"/admin/relay/new")
	csrf := extractCSRF(t, body)
	postFormFollow(t, client, ts.URL+"/admin/relay/new", url.Values{
		"_csrf":            {csrf},
		"name":             {"Rich OpenAI"},
		"endpoint":         {"https://api.example.com/v1"},
		"apiFormat":        {"openai"},
		"apiKey":           {"secret-key"},
		"modelId":          {"gpt-admin", "ocr-admin"},
		"displayName":      {"GPT Admin", "OCR Admin"},
		"description":      {"chat entry", "ocr entry"},
		"category":         {"chat", "ocr"},
		"maxTokens":        {"4096", ""},
		"temperature":      {"0.2", ""},
		"topP":             {"0.9", ""},
		"presencePenalty":  {"0.1", ""},
		"frequencyPenalty": {"0.2", ""},
		"seed":             {"42", ""},
		"stop":             {"END", ""},
		"user":             {"local-user", ""},
		"supportsVision_0": {"on"},
		"supportsTools_0":  {"on"},
		"debugSse_0":       {"on"},
		"modelEnabled_0":   {"on"},
		"modelEnabled_1":   {"on"},
		"enabled":          {"on"},
	})

	body = getAdminPage(t, client, ts.URL+"/admin/relay")
	if !strings.Contains(body, "Rich OpenAI") || !strings.Contains(body, "gpt-admin (chat)") || !strings.Contains(body, "ocr-admin (ocr)") {
		t.Fatalf("created relay model rows are not visible: %s", body)
	}
	match := regexp.MustCompile(`/admin/relay/(\d+)/edit`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("edit link not found")
	}
	body = getAdminPage(t, client, ts.URL+"/admin/relay/"+match[1]+"/edit")
	for _, want := range []string{"GPT Admin", "OCR Admin", "value=\"4096\"", "value=\"0.2\"", "value=\"0.9\"", "value=\"42\"", ">END</textarea>", "value=\"local-user\""} {
		if !strings.Contains(body, want) {
			t.Fatalf("edit form missing %q", want)
		}
	}
}

func TestAdminPanelRelayVivoAppID(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()

	client := adminClient(t)
	loginAdmin(t, client, ts.URL, adminPhone, adminPassword)

	body := getAdminPage(t, client, ts.URL+"/admin/relay/new")
	csrf := extractCSRF(t, body)
	resp := postForm(t, client, ts.URL+"/admin/relay/new", url.Values{
		"_csrf":          {csrf},
		"name":           {"Broken VIVO OCR"},
		"endpoint":       {"https://api-ai.vivo.com.cn/ocr/general_recognition"},
		"apiFormat":      {"vivo_ocr"},
		"apiKey":         {"app-key"},
		"modelId":        {"general_recognition"},
		"category":       {"ocr"},
		"modelEnabled_0": {"on"},
		"enabled":        {"on"},
	})
	brokenBody := string(testutil.ReadAll(t, resp.Body))
	resp.Body.Close()
	if !strings.Contains(brokenBody, "AppID") {
		t.Fatalf("missing AppID error not shown: %s", brokenBody)
	}

	body = getAdminPage(t, client, ts.URL+"/admin/relay/new")
	csrf = extractCSRF(t, body)
	postFormFollow(t, client, ts.URL+"/admin/relay/new", url.Values{
		"_csrf":          {csrf},
		"name":           {"VIVO OCR"},
		"endpoint":       {"https://api-ai.vivo.com.cn/ocr/general_recognition"},
		"apiFormat":      {"vivo_ocr"},
		"apiKey":         {"app-key"},
		"modelId":        {"general_recognition"},
		"category":       {"ocr"},
		"appId":          {"vivo-app-id"},
		"modelEnabled_0": {"on"},
		"enabled":        {"on"},
	})

	body = getAdminPage(t, client, ts.URL+"/admin/relay")
	if !strings.Contains(body, "VIVO OCR") || !strings.Contains(body, "general_recognition (ocr)") {
		t.Fatalf("created vivo relay provider is not visible: %s", body)
	}
	match := regexp.MustCompile(`/admin/relay/(\d+)/edit`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("edit link not found")
	}
	body = getAdminPage(t, client, ts.URL+"/admin/relay/"+match[1]+"/edit")
	if !strings.Contains(body, `value="vivo-app-id"`) || !strings.Contains(body, "AppID") {
		t.Fatalf("edit form missing AppID: %s", body)
	}
}

func TestAdminPanelRelayRejectsInvalidTypeAndCategory(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()
	client := adminClient(t)
	loginAdmin(t, client, ts.URL, adminPhone, adminPassword)
	body := getAdminPage(t, client, ts.URL+"/admin/relay/new")
	csrf := extractCSRF(t, body)
	resp := postForm(t, client, ts.URL+"/admin/relay/new", url.Values{
		"_csrf": {csrf}, "name": {"Bad"}, "endpoint": {"https://example.com"}, "apiFormat": {"vivo_image"},
		"apiKey": {"key"}, "modelId": {"image"}, "category": {"chat"}, "modelEnabled_0": {"on"},
	})
	raw := string(testutil.ReadAll(t, resp.Body))
	resp.Body.Close()
	if !strings.Contains(raw, "API Type") || !strings.Contains(raw, "模型分类") {
		t.Fatalf("missing category validation: %s", raw)
	}
}

func TestAdminPanelRelayAllowsOllamaWithoutAPIKey(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()
	client := adminClient(t)
	loginAdmin(t, client, ts.URL, adminPhone, adminPassword)
	body := getAdminPage(t, client, ts.URL+"/admin/relay/new")
	csrf := extractCSRF(t, body)
	postFormFollow(t, client, ts.URL+"/admin/relay/new", url.Values{
		"_csrf": {csrf}, "name": {"Local Ollama"}, "endpoint": {"http://localhost:11434"}, "apiFormat": {"ollama"},
		"modelId": {"qwen"}, "category": {"chat"}, "modelEnabled_0": {"on"}, "enabled": {"on"},
	})
	body = getAdminPage(t, client, ts.URL+"/admin/relay")
	if !strings.Contains(body, "Local Ollama") {
		t.Fatalf("ollama provider was not created: %s", body)
	}
}

func TestAdminPanelRelayRejectsPublicHTTP(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()
	client := adminClient(t)
	loginAdmin(t, client, ts.URL, adminPhone, adminPassword)
	body := getAdminPage(t, client, ts.URL+"/admin/relay/new")
	csrf := extractCSRF(t, body)
	resp := postForm(t, client, ts.URL+"/admin/relay/new", url.Values{
		"_csrf": {csrf}, "name": {"Unsafe"}, "endpoint": {"http://api.example.com/v1"}, "apiFormat": {"openai"},
		"apiKey": {"key"}, "modelId": {"gpt"}, "category": {"chat"}, "modelEnabled_0": {"on"},
	})
	raw := string(testutil.ReadAll(t, resp.Body))
	resp.Body.Close()
	if !strings.Contains(raw, "Endpoint 不安全") || !strings.Contains(raw, "HTTPS") {
		t.Fatalf("missing endpoint policy error: %s", raw)
	}
}

func TestAdminPanelRelayLogPages(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()
	client := adminClient(t)
	loginAdmin(t, client, ts.URL, adminPhone, adminPassword)

	dashboard := getAdminPage(t, client, ts.URL+"/admin/relay/dashboard?range=7d")
	if !strings.Contains(dashboard, "调用概览") || !strings.Contains(dashboard, "用户调用排行") {
		t.Fatalf("relay dashboard did not render: %s", dashboard)
	}
	logs := getAdminPage(t, client, ts.URL+"/admin/relay/logs?range=7d")
	if !strings.Contains(logs, "调用日志") || !strings.Contains(logs, "用户 ID") {
		t.Fatalf("relay logs did not render: %s", logs)
	}
}

func TestAdminLoginBodyLimit(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTestWithAdminPanel()
	defer cleanup()
	client := adminClient(t)
	values := url.Values{"phone": {"0000000000"}, "password": {strings.Repeat("x", 20<<10)}}
	resp := postForm(t, client, ts.URL+"/admin/login", values)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("admin login status = %d, want 413", resp.StatusCode)
	}
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
	body := testutil.ReadAll(t, resp.Body)
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

func submitPlugin(t *testing.T, tsURL, token, id, name, version string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("zip", id+".zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(createPluginZip(t, id, name, version)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := testutil.NewRequest(t, http.MethodPost, tsURL+"/market/plugins/submit", body)
	testutil.SetBearer(req, token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
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
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := writer.Create("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(manifestJSON); err != nil {
		t.Fatal(err)
	}
	mainFile, err := writer.Create("main.lua")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mainFile.Write([]byte("-- test plugin\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
