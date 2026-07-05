package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// AuthTokenPair 是认证接口返回的 token 载荷。
type AuthTokenPair struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

type authResponse struct {
	Token AuthTokenPair `json:"token"`
}

// PostJSON 发送 JSON 请求，并在编码或请求失败时立即终止测试。
func PostJSON(t testing.TB, target string, body any) *http.Response {
	t.Helper()
	req := NewJSONRequest(t, http.MethodPost, target, body)
	return Do(t, req)
}

// NewRequest 创建 HTTP 请求，并把 URL/请求体错误转成测试失败。
func NewRequest(t testing.TB, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatalf("new %s request for %s: %v", method, target, err)
	}
	return req
}

// NewJSONRequest 创建 JSON 请求，并自动设置 Content-Type。
func NewJSONRequest(t testing.TB, method, target string, body any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode json for %s: %v", target, err)
	}
	req := NewRequest(t, method, target, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// Do 执行 HTTP 请求，并在网络层失败时立即终止测试。
func Do(t testing.TB, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.String(), err)
	}
	return resp
}

// SetBearer 为请求设置 Bearer token。
func SetBearer(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
}

// ReadAll 读取响应体，并在读取失败时终止测试。
func ReadAll(t testing.TB, r io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return data
}

// DecodeJSON 解码响应体，并在 JSON 格式不符合预期时给出明确错误。
func DecodeJSON(t testing.TB, resp *http.Response, target any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		t.Fatalf("decode %s response: %v", resp.Request.URL.Path, err)
	}
}

// RequireStatus 校验 HTTP 状态码，失败时附带响应体，便于定位服务端错误。
func RequireStatus(t testing.TB, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode == want {
		return
	}
	body, _ := io.ReadAll(resp.Body)
	t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, want, body)
}

// RegisterAndGetToken 注册普通用户，并返回 access token。
func RegisterAndGetToken(t testing.TB, baseURL, phone, password string) string {
	t.Helper()
	return authToken(t, baseURL+"/auth/register", phone, password)
}

// LoginAndGetToken 登录已有用户，并返回 access token。
func LoginAndGetToken(t testing.TB, baseURL, phone, password string) string {
	t.Helper()
	return authToken(t, baseURL+"/auth/login", phone, password)
}

func authToken(t testing.TB, target, phone, password string) string {
	t.Helper()
	resp := PostJSON(t, target, map[string]string{
		"phone":    phone,
		"password": password,
	})
	defer resp.Body.Close()
	RequireStatus(t, resp, http.StatusOK)

	var result authResponse
	DecodeJSON(t, resp, &result)
	if result.Token.AccessToken == "" {
		t.Fatalf("%s returned empty access token", target)
	}
	return result.Token.AccessToken
}
