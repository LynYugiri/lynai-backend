package device_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/lynai/backend/internal/device"
	"github.com/lynai/backend/internal/testutil"
)

type challengeResponse struct {
	ChallengeID string `json:"challengeId"`
	Challenge   string `json:"challenge"`
	UserID      string `json:"userId"`
	SessionID   string `json:"sessionId"`
}

type enrollmentInput struct {
	publicKey  ed25519.PublicKey
	privateKey ed25519.PrivateKey
	deviceID   string
	name       string
	platform   string
}

type deviceValue struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Current bool   `json:"current"`
}

func TestDeviceEnrollmentAndLifecycle(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13810000001", "secret123")
	input := newEnrollment(t, "Alice phone")
	deviceValue := enroll(t, ts.URL, token, input)
	if deviceValue.ID != input.deviceID || deviceValue.Name != input.name || !deviceValue.Current {
		t.Fatalf("enrolled device = %+v", deviceValue)
	}

	resp := authenticated(t, http.MethodPost, ts.URL+"/devices/challenge", token, proposal(input))
	testutil.RequireStatus(t, resp, http.StatusOK)
	var challenge challengeResponse
	testutil.DecodeJSON(t, resp, &challenge)
	resp.Body.Close()
	resp = authenticated(t, http.MethodPost, ts.URL+"/devices/enroll", token, signedEnrollment(t, challenge, input))
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = authenticated(t, http.MethodPatch, ts.URL+"/devices/"+input.deviceID, token, map[string]string{"name": "Renamed phone"})
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = authenticated(t, http.MethodDelete, ts.URL+"/devices/"+input.deviceID, token, nil)
	testutil.RequireStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
	resp = authenticated(t, http.MethodGet, ts.URL+"/devices/current", token, nil)
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestDeviceJSONBodyLimits(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13810000010", "secret123")
	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/devices/challenge"},
		{method: http.MethodPost, path: "/devices/enroll"},
		{method: http.MethodPatch, path: "/devices/device-id"},
	} {
		body := strings.NewReader(`{"padding":"` + strings.Repeat("x", 20<<10) + `"}`)
		req := testutil.NewRequest(t, tc.method, ts.URL+tc.path, body)
		req.Header.Set("Content-Type", "application/json")
		testutil.SetBearer(req, token)
		resp := testutil.Do(t, req)
		testutil.RequireStatus(t, resp, http.StatusRequestEntityTooLarge)
		resp.Body.Close()
	}
}

func TestDeviceRevokeOnlyRevokesItsAssociatedSession(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	phone := "13810000009"
	boundToken := testutil.RegisterAndGetToken(t, ts.URL, phone, "secret123")
	otherToken := testutil.LoginAndGetToken(t, ts.URL, phone, "secret123")
	input := newEnrollment(t, "Session-bound device")
	enroll(t, ts.URL, boundToken, input)

	resp := authenticated(t, http.MethodDelete, ts.URL+"/devices/"+input.deviceID, otherToken, nil)
	testutil.RequireStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()

	for _, tc := range []struct {
		token string
		want  int
	}{{token: boundToken, want: http.StatusUnauthorized}, {token: otherToken, want: http.StatusOK}} {
		req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/auth/me", nil)
		testutil.SetBearer(req, tc.token)
		resp = testutil.Do(t, req)
		testutil.RequireStatus(t, resp, tc.want)
		resp.Body.Close()
	}
}

func TestChallengeIsBoundToUserSessionAndProposal(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	tokenA := testutil.RegisterAndGetToken(t, ts.URL, "13810000002", "secret123")
	tokenB := testutil.RegisterAndGetToken(t, ts.URL, "13810000003", "secret123")
	tokenASecondSession := testutil.LoginAndGetToken(t, ts.URL, "13810000002", "secret123")
	input := newEnrollment(t, "Bound device")
	challenge := issueChallenge(t, ts.URL, tokenA, input)
	body := signedEnrollment(t, challenge, input)

	for _, token := range []string{tokenB, tokenASecondSession} {
		resp := authenticated(t, http.MethodPost, ts.URL+"/devices/enroll", token, body)
		testutil.RequireStatus(t, resp, http.StatusBadRequest)
		resp.Body.Close()
	}
	tampered := mapsClone(body)
	tampered["displayName"] = "Substituted device"
	resp := authenticated(t, http.MethodPost, ts.URL+"/devices/enroll", tokenA, tampered)
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
	resp = authenticated(t, http.MethodPost, ts.URL+"/devices/enroll", tokenA, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestChallengeCanBeConsumedOnlyOnce(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13810000004", "secret123")
	input := newEnrollment(t, "Concurrent device")
	challenge := issueChallenge(t, ts.URL, token, input)
	body := signedEnrollment(t, challenge, input)

	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := authenticated(t, http.MethodPost, ts.URL+"/devices/enroll", token, body)
			statuses <- resp.StatusCode
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(statuses)
	succeeded, rejected := 0, 0
	for status := range statuses {
		if status == http.StatusOK {
			succeeded++
		} else if status == http.StatusConflict {
			rejected++
		} else {
			t.Fatalf("unexpected enrollment status %d", status)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("succeeded=%d rejected=%d", succeeded, rejected)
	}
}

func TestDeviceIdentityCannotBeEnrolledByAnotherAccount(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	tokenA := testutil.RegisterAndGetToken(t, ts.URL, "13810000005", "secret123")
	tokenB := testutil.RegisterAndGetToken(t, ts.URL, "13810000006", "secret123")
	input := newEnrollment(t, "Shared installation")
	enroll(t, ts.URL, tokenA, input)
	challenge := issueChallenge(t, ts.URL, tokenB, input)
	resp := authenticated(t, http.MethodPost, ts.URL+"/devices/enroll", tokenB, signedEnrollment(t, challenge, input))
	testutil.RequireStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestEnrollmentRejectsNonCanonicalBase64URL(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13810000007", "secret123")
	input := newEnrollment(t, "Canonical device")
	body := proposal(input)
	body["publicKey"] = body["publicKey"].(string) + "="
	resp := authenticated(t, http.MethodPost, ts.URL+"/devices/challenge", token, body)
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestEnrollmentRejectsDeviceIDFromAnotherPublicKey(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13810000008", "secret123")
	input := newEnrollment(t, "Conflicting identity")
	other := newEnrollment(t, "Other identity")
	body := proposal(input)
	body["deviceId"] = other.deviceID
	resp := authenticated(t, http.MethodPost, ts.URL+"/devices/challenge", token, body)
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func enroll(t *testing.T, baseURL, token string, input enrollmentInput) deviceValue {
	t.Helper()
	challenge := issueChallenge(t, baseURL, token, input)
	resp := authenticated(t, http.MethodPost, baseURL+"/devices/enroll", token, signedEnrollment(t, challenge, input))
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result struct {
		Device deviceValue `json:"device"`
	}
	testutil.DecodeJSON(t, resp, &result)
	return result.Device
}

func issueChallenge(t *testing.T, baseURL, token string, input enrollmentInput) challengeResponse {
	t.Helper()
	resp := authenticated(t, http.MethodPost, baseURL+"/devices/challenge", token, proposal(input))
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result challengeResponse
	testutil.DecodeJSON(t, resp, &result)
	return result
}

func newEnrollment(t *testing.T, name string) enrollmentInput {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(publicKey)
	return enrollmentInput{
		publicKey: publicKey, privateKey: privateKey,
		deviceID: strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])),
		name:     name, platform: "linux",
	}
}

func proposal(input enrollmentInput) map[string]any {
	return map[string]any{
		"deviceId":        input.deviceID,
		"publicKey":       base64.RawURLEncoding.EncodeToString(input.publicKey),
		"displayName":     input.name,
		"platform":        input.platform,
		"protocolVersion": 1,
	}
}

func signedEnrollment(t *testing.T, challenge challengeResponse, input enrollmentInput) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(challenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	message := device.EnrollmentMessage(
		1, challenge.ChallengeID, raw, challenge.UserID, challenge.SessionID,
		input.deviceID, input.publicKey, input.name, input.platform,
	)
	body := proposal(input)
	body["challengeId"] = challenge.ChallengeID
	body["challenge"] = challenge.Challenge
	body["signature"] = base64.RawURLEncoding.EncodeToString(ed25519.Sign(input.privateKey, message))
	return body
}

func mapsClone(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func authenticated(t testing.TB, method, target, token string, body any) *http.Response {
	t.Helper()
	var req *http.Request
	if body == nil {
		req = testutil.NewRequest(t, method, target, nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req = testutil.NewRequest(t, method, target, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
	}
	testutil.SetBearer(req, token)
	return testutil.Do(t, req)
}
