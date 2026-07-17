package sync_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/lynai/backend/internal/device"
	syncapi "github.com/lynai/backend/internal/sync"
	"github.com/lynai/backend/internal/testutil"
)

type syncDevice struct {
	publicKey  ed25519.PublicKey
	privateKey ed25519.PrivateKey
	deviceID   string
	userID     string
	sessionID  string
}

func TestSignedSyncReplayConflictRevocationAndCompatibility(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000101", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	requestID := randomRequestID(t)
	body := signedBody(t, requestID, "change-signed-1", "message-1")

	resp := doSignedSync(t, ts.URL+"/sync/changes", token, device, requestID, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	first := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	resp = doSignedSync(t, ts.URL+"/sync/changes", token, device, requestID, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	second := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if !bytes.Equal(first, second) {
		t.Fatalf("replayed response differs:\n%s\n%s", first, second)
	}
	var firstResult struct {
		Changes []struct {
			Seq int64 `json:"seq"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(first, &firstResult); err != nil || len(firstResult.Changes) != 1 {
		t.Fatalf("decode first response: %v, body=%s", err, first)
	}

	duplicateRequestID := randomRequestID(t)
	duplicateBody := signedBody(t, duplicateRequestID, "change-signed-1", "message-1")
	resp = doSignedSync(t, ts.URL+"/sync/changes", token, device, duplicateRequestID, duplicateBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var duplicateResult struct {
		Changes []struct {
			Seq int64 `json:"seq"`
		} `json:"changes"`
	}
	testutil.DecodeJSON(t, resp, &duplicateResult)
	resp.Body.Close()
	if len(duplicateResult.Changes) != 1 || duplicateResult.Changes[0].Seq != firstResult.Changes[0].Seq {
		t.Fatalf("stable change ACK = %+v, first = %+v", duplicateResult, firstResult)
	}

	changeConflictID := randomRequestID(t)
	resp = doSignedSync(t, ts.URL+"/sync/changes", token, device, changeConflictID, signedBody(t, changeConflictID, "change-signed-1", "different-record"))
	testutil.RequireStatus(t, resp, http.StatusConflict)
	resp.Body.Close()

	conflicting := signedBody(t, requestID, "change-signed-2", "message-2")
	resp = doSignedSync(t, ts.URL+"/sync/changes", token, device, requestID, conflicting)
	testutil.RequireStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
	resp = doSignedSync(t, ts.URL+"/sync/v1/changes", token, device, requestID, body)
	testutil.RequireStatus(t, resp, http.StatusConflict)
	resp.Body.Close()

	compatID := randomRequestID(t)
	compatBody := signedBody(t, compatID, "change-compat-1", "message-compat")
	resp = doSignedSync(t, ts.URL+"/sync/v1/changes", token, device, compatID, compatBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	revoke := testutil.NewRequest(t, http.MethodDelete, ts.URL+"/devices/"+device.deviceID, nil)
	testutil.SetBearer(revoke, token)
	resp = testutil.Do(t, revoke)
	testutil.RequireStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
	revokedID := randomRequestID(t)
	resp = doSignedSync(t, ts.URL+"/sync/changes", token, device, revokedID, signedBody(t, revokedID, "change-revoked", "message-revoked"))
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestRequiredSigningRejectsLegacyUnsignedUpload(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000103", testPassword)
	req := testutil.NewJSONRequest(t, http.MethodPost, ts.URL+"/sync/changes", map[string]any{"changes": []map[string]any{{
		"table": "messages", "op": "delete", "recordId": "legacy",
	}}})
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
}

func TestRequiredSigningRejectsUnsignedBlobUpload(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000107", testPassword)
	body := []byte("unsigned blob")
	digest := sha256.Sum256(body)
	req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/sync/blobs/"+hex.EncodeToString(digest[:]), bytes.NewReader(body))
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
}

func TestRequiredSignedSyncScopesAccountIdentities(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	tokenA := testutil.RegisterAndGetToken(t, ts.URL, "13100000105", testPassword)
	tokenB := testutil.RegisterAndGetToken(t, ts.URL, "13100000106", testPassword)
	deviceA := enrollSyncDevice(t, ts.URL, tokenA)
	deviceB := enrollSyncDevice(t, ts.URL, tokenB)
	if deviceA.deviceID == deviceB.deviceID {
		t.Fatalf("account-scoped device IDs unexpectedly match: %q", deviceA.deviceID)
	}

	requestA := randomRequestID(t)
	resp := doSignedSync(t, ts.URL+"/sync/changes", tokenA, deviceA, requestA, signedBody(t, requestA, "account-a-change", "account-a-record"))
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	requestB := randomRequestID(t)
	resp = doSignedSync(t, ts.URL+"/sync/changes", tokenB, deviceB, requestB, signedBody(t, requestB, "account-b-change", "account-b-record"))
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	crossAccountRequest := randomRequestID(t)
	resp = doSignedSync(t, ts.URL+"/sync/changes", tokenB, deviceA, crossAccountRequest, signedBody(t, crossAccountRequest, "cross-account-change", "cross-account-record"))
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()

	revoke := testutil.NewRequest(t, http.MethodDelete, ts.URL+"/devices/"+deviceA.deviceID, nil)
	testutil.SetBearer(revoke, tokenA)
	resp = testutil.Do(t, revoke)
	testutil.RequireStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()

	revokedRequest := randomRequestID(t)
	resp = doSignedSync(t, ts.URL+"/sync/changes", tokenA, deviceA, revokedRequest, signedBody(t, revokedRequest, "account-a-revoked", "account-a-revoked"))
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
	activeRequest := randomRequestID(t)
	resp = doSignedSync(t, ts.URL+"/sync/changes", tokenB, deviceB, activeRequest, signedBody(t, activeRequest, "account-b-active", "account-b-active"))
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestConcurrentSignedDuplicateReturnsOneExactResult(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000102", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	requestID := randomRequestID(t)
	body := signedBody(t, requestID, "change-concurrent-signed", "message-concurrent")

	var wg stdsync.WaitGroup
	responses := make(chan []byte, 2)
	statuses := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := doSignedSync(t, ts.URL+"/sync/changes", token, device, requestID, body)
			statuses <- resp.StatusCode
			responses <- testutil.ReadAll(t, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(statuses)
	close(responses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent status = %d", status)
		}
	}
	var first []byte
	for response := range responses {
		if first == nil {
			first = response
		} else if !bytes.Equal(first, response) {
			t.Fatalf("concurrent replay bodies differ:\n%s\n%s", first, response)
		}
	}

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/changes?since=0", nil)
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	var result struct {
		Changes []json.RawMessage `json:"changes"`
	}
	testutil.DecodeJSON(t, resp, &result)
	if len(result.Changes) != 1 {
		t.Fatalf("stored changes = %d, want 1", len(result.Changes))
	}
}

func TestSignedBlobUploadReplaysExactResponse(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000104", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	requestID := randomRequestID(t)
	body := []byte("signed blob content")
	digest := sha256.Sum256(body)
	target := ts.URL + "/sync/blobs/" + hex.EncodeToString(digest[:])

	resp := doSignedBlob(t, target, token, device, requestID, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	first := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	resp = doSignedBlob(t, target, token, device, requestID, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	second := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if !bytes.Equal(first, second) {
		t.Fatalf("replayed blob response differs:\n%s\n%s", first, second)
	}

	otherRequestID := randomRequestID(t)
	resp = doSignedBlob(t, target, token, device, otherRequestID, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result struct {
		Owned   bool  `json:"owned"`
		Created bool  `json:"created"`
		Size    int64 `json:"size"`
	}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()
	if !result.Owned || result.Created || result.Size != int64(len(body)) {
		t.Fatalf("duplicate blob result = %+v", result)
	}
}

func enrollSyncDevice(t *testing.T, baseURL, token string) syncDevice {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return enrollSyncDeviceIdentity(t, baseURL, token, publicKey, privateKey)
}

func enrollSyncDeviceIdentity(t *testing.T, baseURL, token string, publicKey ed25519.PublicKey, privateKey ed25519.PrivateKey) syncDevice {
	t.Helper()
	digest := sha256.Sum256(publicKey)
	deviceID := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))
	proposal := map[string]any{"deviceId": deviceID, "publicKey": base64.RawURLEncoding.EncodeToString(publicKey), "displayName": "Sync test", "platform": "linux", "protocolVersion": 1}
	resp := authenticatedJSON(t, http.MethodPost, baseURL+"/devices/challenge", token, proposal)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var challenge struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		UserID      string `json:"userId"`
		SessionID   string `json:"sessionId"`
	}
	testutil.DecodeJSON(t, resp, &challenge)
	rawChallenge, _ := base64.RawURLEncoding.DecodeString(challenge.Challenge)
	message := device.EnrollmentMessage(1, challenge.ChallengeID, rawChallenge, challenge.UserID, challenge.SessionID, deviceID, publicKey, "Sync test", "linux")
	enrollment := proposal
	enrollment["challengeId"] = challenge.ChallengeID
	enrollment["challenge"] = challenge.Challenge
	enrollment["signature"] = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))
	resp = authenticatedJSON(t, http.MethodPost, baseURL+"/devices/enroll", token, enrollment)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	return syncDevice{publicKey: publicKey, privateKey: privateKey, deviceID: deviceID, userID: challenge.UserID, sessionID: challenge.SessionID}
}

func doSignedSync(t testing.TB, target, token string, device syncDevice, requestID string, body []byte) *http.Response {
	t.Helper()
	path := "/sync/changes"
	if strings.Contains(target, "/sync/v1/changes") {
		path = "/sync/v1/changes"
	}
	digest := sha256.Sum256(body)
	timestamp := time.Now().UnixMilli()
	message := syncapi.SyncRequestMessage(1, device.userID, device.sessionID, device.deviceID, timestamp, requestID, http.MethodPost, path, digest[:])
	req := testutil.NewRequest(t, http.MethodPost, target, bytes.NewReader(body))
	testutil.SetBearer(req, token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LynAI-Protocol", "1")
	req.Header.Set("X-LynAI-Device-ID", device.deviceID)
	req.Header.Set("X-LynAI-Timestamp", strconv.FormatInt(timestamp, 10))
	req.Header.Set("X-LynAI-Request-ID", requestID)
	req.Header.Set("X-LynAI-Body-SHA256", hex.EncodeToString(digest[:]))
	req.Header.Set("X-LynAI-Signature", base64.RawURLEncoding.EncodeToString(ed25519.Sign(device.privateKey, message)))
	return testutil.Do(t, req)
}

func doSignedBlob(t testing.TB, target, token string, device syncDevice, requestID string, body []byte) *http.Response {
	t.Helper()
	digest := sha256.Sum256(body)
	path := "/sync/blobs/:sha256"
	timestamp := time.Now().UnixMilli()
	message := syncapi.SyncRequestMessage(1, device.userID, device.sessionID, device.deviceID, timestamp, requestID, http.MethodPost, path, digest[:])
	req := testutil.NewRequest(t, http.MethodPost, target, bytes.NewReader(body))
	testutil.SetBearer(req, token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-LynAI-Protocol", "1")
	req.Header.Set("X-LynAI-Device-ID", device.deviceID)
	req.Header.Set("X-LynAI-Timestamp", strconv.FormatInt(timestamp, 10))
	req.Header.Set("X-LynAI-Request-ID", requestID)
	req.Header.Set("X-LynAI-Body-SHA256", hex.EncodeToString(digest[:]))
	req.Header.Set("X-LynAI-Signature", base64.RawURLEncoding.EncodeToString(ed25519.Sign(device.privateKey, message)))
	return testutil.Do(t, req)
}

func signedBody(t testing.TB, requestID, changeID, recordID string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"requestId": requestID, "changes": []map[string]any{{
		"changeId": changeID, "table": "messages", "op": "delete", "recordId": recordID,
		"clientCreatedAt": "2026-07-16T12:00:00Z",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func authenticatedJSON(t testing.TB, method, target, token string, body any) *http.Response {
	t.Helper()
	req := testutil.NewJSONRequest(t, method, target, body)
	testutil.SetBearer(req, token)
	return testutil.Do(t, req)
}

func randomRequestID(t testing.TB) string {
	t.Helper()
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
