package auth_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/auth"
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

func TestConcurrentRegisterDuplicateReturnsConflict(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	body, err := json.Marshal(map[string]string{"phone": "13700003334", "password": testPassword})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/auth/register", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp := testutil.Do(t, req)
			statuses <- resp.StatusCode
			resp.Body.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("concurrent registration statuses = %v, want one 200 and one 409", counts)
	}
}

func TestAuthJSONBodyLimits(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	for _, path := range []string{
		"/auth/register",
		"/auth/login",
		"/auth/send-otp",
		"/auth/verify-otp",
		"/auth/refresh",
		"/auth/revoke",
	} {
		body := strings.NewReader(`{"phone":"13800000000","password":"secret123","padding":"` + strings.Repeat("x", 20<<10) + `"}`)
		req := testutil.NewRequest(t, http.MethodPost, ts.URL+path, body)
		req.Header.Set("Content-Type", "application/json")
		resp := testutil.Do(t, req)
		testutil.RequireStatus(t, resp, http.StatusRequestEntityTooLarge)
		resp.Body.Close()
	}
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

func TestAdminMiddlewareRejectsMissingOrInvalidRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, value := range []interface{}{nil, "true"} {
		router := gin.New()
		router.Use(func(c *gin.Context) {
			if value != nil {
				c.Set("isAdmin", value)
			}
			c.Next()
		}, auth.AdminMiddleware())
		router.GET("/admin", func(c *gin.Context) { c.Status(http.StatusNoContent) })

		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin", nil))
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("isAdmin=%v status = %d, want 403", value, recorder.Code)
		}
	}
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

func TestLogoutRevokesSession(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone": "13500005556", "password": testPassword,
	})
	accessToken := res.Token["accessToken"].(string)
	refreshToken := res.Token["refreshToken"].(string)

	req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/auth/logout", nil)
	testutil.SetBearer(req, accessToken)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/auth/me", nil)
	testutil.SetBearer(req, accessToken)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()

	resp = testutil.PostJSON(t, ts.URL+"/auth/refresh", map[string]string{"refreshToken": refreshToken})
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestRefreshTokenRevokeIsIdempotent(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone": "13500005559", "password": testPassword,
	})
	refreshToken := res.Token["refreshToken"].(string)
	for range 2 {
		resp := testutil.PostJSON(t, ts.URL+"/auth/revoke", map[string]string{"refreshToken": refreshToken})
		testutil.RequireStatus(t, resp, http.StatusNoContent)
		resp.Body.Close()
	}
	resp := testutil.PostJSON(t, ts.URL+"/auth/revoke", map[string]string{"refreshToken": "invalid"})
	testutil.RequireStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()

	resp = testutil.PostJSON(t, ts.URL+"/auth/refresh", map[string]string{"refreshToken": refreshToken})
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestRotatedRefreshTokenRevokesSessionFamily(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone": "13500005560", "password": testPassword,
	})
	oldRefresh := res.Token["refreshToken"].(string)
	rotated := doAuthRequest(t, ts.URL, "/auth/refresh", map[string]string{"refreshToken": oldRefresh})
	resp := testutil.PostJSON(t, ts.URL+"/auth/revoke", map[string]string{"refreshToken": oldRefresh})
	testutil.RequireStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()

	resp = testutil.PostJSON(t, ts.URL+"/auth/refresh", map[string]string{
		"refreshToken": rotated.Token["refreshToken"].(string),
	})
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/auth/me", nil)
	testutil.SetBearer(req, rotated.Token["accessToken"].(string))
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestConcurrentRefreshAndFamilyRevokeLeavesSessionRevoked(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone": "13500005561", "password": testPassword,
	})
	refreshToken := res.Token["refreshToken"].(string)
	body, err := json.Marshal(map[string]string{"refreshToken": refreshToken})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	statuses := make(chan int, 2)
	var rotatedRefresh string
	var rotatedMu sync.Mutex
	var wg sync.WaitGroup
	for _, path := range []string{"/auth/refresh", "/auth/revoke"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Errorf("POST %s: %v", path, err)
				return
			}
			defer resp.Body.Close()
			statuses <- resp.StatusCode
			if path == "/auth/refresh" && resp.StatusCode == http.StatusOK {
				var payload map[string]interface{}
				if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
					t.Errorf("decode refresh response: %v", err)
					return
				}
				token, _ := payload["token"].(map[string]interface{})
				rotatedMu.Lock()
				rotatedRefresh, _ = token["refreshToken"].(string)
				rotatedMu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)

	seenNoContent := false
	for status := range statuses {
		if status == http.StatusNoContent {
			seenNoContent = true
			continue
		}
		if status != http.StatusOK && status != http.StatusUnauthorized {
			t.Fatalf("unexpected concurrent status %d", status)
		}
	}
	if !seenNoContent {
		t.Fatal("revoke did not succeed")
	}

	rotatedMu.Lock()
	currentRefresh := rotatedRefresh
	rotatedMu.Unlock()
	if currentRefresh == "" {
		currentRefresh = refreshToken
	}
	resp := testutil.PostJSON(t, ts.URL+"/auth/refresh", map[string]string{"refreshToken": currentRefresh})
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestRefreshRotationRejectsOldTokenWithoutRevokingSession(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone": "13500005557", "password": testPassword,
	})
	oldRefresh := res.Token["refreshToken"].(string)
	rotated := doAuthRequest(t, ts.URL, "/auth/refresh", map[string]string{"refreshToken": oldRefresh})

	resp := testutil.PostJSON(t, ts.URL+"/auth/refresh", map[string]string{"refreshToken": oldRefresh})
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/auth/me", nil)
	testutil.SetBearer(req, rotated.Token["accessToken"].(string))
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = testutil.PostJSON(t, ts.URL+"/auth/refresh", map[string]string{
		"refreshToken": rotated.Token["refreshToken"].(string),
	})
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestConcurrentRefreshConsumesTokenOnce(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	res := doAuthRequest(t, ts.URL, "/auth/register", map[string]string{
		"phone": "13500005558", "password": testPassword,
	})
	body, err := json.Marshal(map[string]string{"refreshToken": res.Token["refreshToken"].(string)})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type refreshResult struct {
		status int
		token  map[string]interface{}
	}
	results := make(chan refreshResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Post(ts.URL+"/auth/refresh", "application/json", bytes.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			result := refreshResult{status: resp.StatusCode}
			if resp.StatusCode == http.StatusOK {
				var payload map[string]interface{}
				if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
					resp.Body.Close()
					errs <- err
					return
				}
				result.token, _ = payload["token"].(map[string]interface{})
			}
			resp.Body.Close()
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent refresh: %v", err)
	}

	counts := map[int]int{}
	var rotated map[string]interface{}
	for result := range results {
		counts[result.status]++
		if result.status == http.StatusOK {
			rotated = result.token
		}
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusUnauthorized] != 1 {
		t.Fatalf("refresh statuses = %v, want one 200 and one 401", counts)
	}
	if rotated == nil {
		t.Fatal("successful concurrent refresh response is missing token")
	}

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/auth/me", nil)
	testutil.SetBearer(req, rotated["accessToken"].(string))
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = testutil.PostJSON(t, ts.URL+"/auth/refresh", map[string]string{
		"refreshToken": rotated["refreshToken"].(string),
	})
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = testutil.PostJSON(t, ts.URL+"/auth/refresh", map[string]string{
		"refreshToken": res.Token["refreshToken"].(string),
	})
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
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
