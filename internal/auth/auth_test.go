package auth_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lynai/backend/internal/testutil"
)

type loginResult struct {
	User  map[string]interface{}
	Token map[string]interface{}
}

const testPassword = "secret123"

func doAuthRequest(t *testing.T, tsURL, path string, body map[string]string) loginResult {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(tsURL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()

	user, ok := result["user"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: response missing user", path)
	}
	token, ok := result["token"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: response missing token", path)
	}
	return loginResult{User: user, Token: token}
}

func TestRegisterAndLogin(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	// Register a new user
	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone":       "13800001111",
		"password":    testPassword,
		"displayName": "Alice",
	})
	if res.User["phone"] != "13800001111" {
		t.Fatalf("phone = %v, want 13800001111", res.User["phone"])
	}
	if res.User["displayName"] != "Alice" {
		t.Fatalf("displayName = %v, want Alice", res.User["displayName"])
	}
	if res.User["isAdmin"] != false {
		t.Fatalf("isAdmin = %v, want false", res.User["isAdmin"])
	}
	if res.Token["accessToken"] == "" {
		t.Fatal("accessToken is empty")
	}
	if res.Token["refreshToken"] == "" {
		t.Fatal("refreshToken is empty")
	}

	// Login with the same phone
	res = doAuthRequest(t, ts.URL, "/auth/login", map[string]string{
		"phone":    "13800001111",
		"password": testPassword,
	})
	if res.User["phone"] != "13800001111" {
		t.Fatal("login returned wrong user")
	}
	if res.Token["refreshToken"] == "" {
		t.Fatal("login: refreshToken is empty")
	}
}

func TestRegisterDefaultDisplayName(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone":    "13900002222",
		"password": testPassword,
	})
	if res.User["displayName"] != "用户2222" {
		t.Fatalf("default displayName = %v, want 用户2222", res.User["displayName"])
	}
}

func TestRegisterDuplicate(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	body, _ := json.Marshal(map[string]string{"phone": "13700003333", "password": testPassword})
	resp, _ := http.Post(ts.URL+"/auth/register", "application/json", bytes.NewReader(body))
	resp.Body.Close()

	resp, _ = http.Post(ts.URL+"/auth/register", "application/json", bytes.NewReader(body))
	if resp.StatusCode != 409 {
		t.Fatalf("duplicate register status = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLoginUnregistered(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	body, _ := json.Marshal(map[string]string{"phone": "99999999999", "password": testPassword})
	resp, _ := http.Post(ts.URL+"/auth/login", "application/json", bytes.NewReader(body))
	if resp.StatusCode != 401 {
		t.Fatalf("login unregistered status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLoginWrongPassword(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone":    "13900009999",
		"password": testPassword,
	})
	body, _ := json.Marshal(map[string]string{
		"phone":    "13900009999",
		"password": "wrong-password",
	})
	resp, _ := http.Post(ts.URL+"/auth/login", "application/json", bytes.NewReader(body))
	if resp.StatusCode != 401 {
		t.Fatalf("login wrong password status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMeRequiresAuth(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	resp, err := http.Get(ts.URL + "/auth/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("me without token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMeWithToken(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone":    "13600004444",
		"password": testPassword,
	})
	accessToken := res.Token["accessToken"].(string)

	req, _ := http.NewRequest("GET", ts.URL+"/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("me status = %d, want 200", resp.StatusCode)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	if result["user"].(map[string]interface{})["phone"] != "13600004444" {
		t.Fatal("me returned wrong user")
	}
}

func TestAdminLogin(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/login", map[string]string{
		"phone":    adminPhone,
		"password": adminPassword,
	})
	if res.User["isAdmin"] != true {
		t.Fatal("admin user should have isAdmin=true")
	}
}

func TestRefresh(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone":    "13500005555",
		"password": testPassword,
	})
	refreshToken := res.Token["refreshToken"].(string)

	body, _ := json.Marshal(map[string]string{
		"refreshToken": refreshToken,
	})
	resp, err := http.Post(ts.URL+"/auth/refresh", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("refresh status = %d, want 200", resp.StatusCode)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()

	newToken := result["token"].(map[string]interface{})
	if newToken["accessToken"] == "" {
		t.Fatal("refresh: new accessToken is empty")
	}
	if newToken["refreshToken"] == "" {
		t.Fatal("refresh: new refreshToken is empty")
	}

	// Verify the new access token works
	req, _ := http.NewRequest("GET", ts.URL+"/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+newToken["accessToken"].(string))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("me after refresh: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("me after refresh status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRefreshInvalid(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	body, _ := json.Marshal(map[string]string{
		"refreshToken": "invalid-token-string",
	})
	resp, err := http.Post(ts.URL+"/auth/refresh", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("refresh with invalid token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRefreshTokenRejectedForAPI(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone":    "13400006666",
		"password": testPassword,
	})
	refreshToken := res.Token["refreshToken"].(string)

	req, _ := http.NewRequest("GET", ts.URL+"/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+refreshToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("me with refresh token: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("me with refresh token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVerifyOTP(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	// OTP routes are reserved but disabled until SMS verification ships.
	body, _ := json.Marshal(map[string]string{
		"phone": "13300007777",
		"code":  "1234",
	})
	resp, err := http.Post(ts.URL+"/auth/verify-otp", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("verify-otp: %v", err)
	}
	if resp.StatusCode != 501 {
		t.Fatalf("verify-otp status = %d, want 501", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSendOTP(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	body, _ := json.Marshal(map[string]string{"phone": "13200008888"})
	resp, err := http.Post(ts.URL+"/auth/send-otp", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("send-otp: %v", err)
	}
	if resp.StatusCode != 501 {
		t.Fatalf("send-otp status = %d, want 501", resp.StatusCode)
	}
	resp.Body.Close()
}
