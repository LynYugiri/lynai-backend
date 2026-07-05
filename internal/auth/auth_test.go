package auth_test

import (
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
	resp := testutil.PostJSON(t, tsURL+path, body)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)

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

	body := map[string]string{"phone": "13700003333", "password": testPassword}
	resp := testutil.PostJSON(t, ts.URL+"/auth/register", body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = testutil.PostJSON(t, ts.URL+"/auth/register", body)
	testutil.RequireStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestLoginUnregistered(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	body := map[string]string{"phone": "99999999999", "password": testPassword}
	resp := testutil.PostJSON(t, ts.URL+"/auth/login", body)
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestLoginWrongPassword(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone":    "13900009999",
		"password": testPassword,
	})
	body := map[string]string{
		"phone":    "13900009999",
		"password": "wrong-password",
	}
	resp := testutil.PostJSON(t, ts.URL+"/auth/login", body)
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestMeRequiresAuth(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	resp, err := http.Get(ts.URL + "/auth/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
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

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/auth/me", nil)
	testutil.SetBearer(req, accessToken)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
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

	body := map[string]string{
		"refreshToken": refreshToken,
	}
	resp := testutil.PostJSON(t, ts.URL+"/auth/refresh", body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()

	newToken := result["token"].(map[string]interface{})
	if newToken["accessToken"] == "" {
		t.Fatal("refresh: new accessToken is empty")
	}
	if newToken["refreshToken"] == "" {
		t.Fatal("refresh: new refreshToken is empty")
	}

	// Verify the new access token works
	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/auth/me", nil)
	testutil.SetBearer(req, newToken["accessToken"].(string))
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestRefreshInvalid(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	body := map[string]string{
		"refreshToken": "invalid-token-string",
	}
	resp := testutil.PostJSON(t, ts.URL+"/auth/refresh", body)
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
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

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/auth/me", nil)
	testutil.SetBearer(req, refreshToken)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestVerifyOTP(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	// OTP routes are reserved but disabled until SMS verification ships.
	body := map[string]string{
		"phone": "13300007777",
		"code":  "1234",
	}
	resp := testutil.PostJSON(t, ts.URL+"/auth/verify-otp", body)
	testutil.RequireStatus(t, resp, http.StatusNotImplemented)
	resp.Body.Close()
}

func TestSendOTP(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	body := map[string]string{"phone": "13200008888"}
	resp := testutil.PostJSON(t, ts.URL+"/auth/send-otp", body)
	testutil.RequireStatus(t, resp, http.StatusNotImplemented)
	resp.Body.Close()
}
