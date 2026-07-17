package sync_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/lynai/backend/internal/testutil"
)

const testPassword = "secret123"

func TestSyncStatusEmpty(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000001", testPassword)

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/status", nil)
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()
	if result["lastSeq"] != float64(0) {
		t.Fatalf("lastSeq = %v, want 0", result["lastSeq"])
	}
	limits, ok := result["limits"].(map[string]interface{})
	if !ok || limits["maxBlobBytes"] != float64(64<<20) || limits["maxChangesRequestBytes"] != float64(2<<20) || limits["maxChangesPerRequest"] != float64(500) || limits["maxChangeDataBytes"] != float64(256<<10) || limits["maxChangesPageSize"] != float64(1000) || limits["maxBlobsPageSize"] != float64(1000) {
		t.Fatalf("limits = %#v", result["limits"])
	}
}

func TestUploadChanges(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000002", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)

	changes := []map[string]interface{}{
		{
			"table":    "resources",
			"op":       "upsert",
			"recordId": "res-1",
			"data": map[string]interface{}{
				"id": "res-1", "sha256": strings.Repeat("a", 64),
			},
		},
		{
			"table":    "conversations",
			"op":       "upsert",
			"recordId": "conv-1",
			"data":     map[string]interface{}{"id": "conv-1", "title": "Test"},
		},
		{
			"table":    "messages",
			"op":       "upsert",
			"recordId": "msg-1",
			"data":     map[string]interface{}{"id": "msg-1", "content": "hello"},
		},
	}
	resp := uploadSignedChanges(t, ts.URL, token, device, changes)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()

	if result["latestSeq"] != float64(3) {
		t.Fatalf("latestSeq = %v, want 3", result["latestSeq"])
	}
	changeList, _ := result["changes"].([]interface{})
	if len(changeList) != 3 {
		t.Fatalf("changes len = %d, want 3", len(changeList))
	}
}

func TestPluginSyncDomainsAreAllowlisted(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000999", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	for _, table := range []string{"plugin_files", "plugin_settings", "plugin_config"} {
		change := map[string]interface{}{"table": table, "op": "upsert", "recordId": "plugin/file", "data": map[string]interface{}{"id": "plugin/file"}}
		resp := uploadSignedChanges(t, ts.URL, token, device, []map[string]interface{}{change})
		testutil.RequireStatus(t, resp, http.StatusOK)
		resp.Body.Close()
	}
	change := map[string]interface{}{"table": "plugin_storage", "op": "upsert", "recordId": "plugin/storage", "data": map[string]interface{}{"id": "plugin/storage"}}
	resp := uploadSignedChanges(t, ts.URL, token, device, []map[string]interface{}{change})
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestGetChanges(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000003", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)

	// Upload some changes
	changes := []map[string]interface{}{
		{"table": "conversations", "op": "upsert", "recordId": "c1", "data": map[string]interface{}{"id": "c1"}},
		{"table": "messages", "op": "delete", "recordId": "m1"},
	}
	resp := uploadSignedChanges(t, ts.URL, token, device, changes)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Get changes since seq=0
	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/changes?since=0", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()

	changeList, _ := result["changes"].([]interface{})
	if len(changeList) != 2 {
		t.Fatalf("changes len = %d, want 2", len(changeList))
	}
	if result["latestSeq"] != float64(2) {
		t.Fatalf("latestSeq = %v, want 2", result["latestSeq"])
	}

	// Get changes since seq=1 — should only return seq=2
	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/changes?since=1", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()
	changeList, _ = result["changes"].([]interface{})
	if len(changeList) != 1 {
		t.Fatalf("changes since 1 len = %d, want 1", len(changeList))
	}
}

func TestSyncBlobs(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000004", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)

	// Upload a blob
	blobData := []byte("test blob content")
	hash := sha256.Sum256(blobData)
	sha := hex.EncodeToString(hash[:])
	resp := doSignedBlob(t, ts.URL+"/sync/blobs/"+sha, token, device, randomRequestID(t), blobData)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// List blobs
	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/blobs", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()
	blobs, _ := result["blobs"].([]interface{})
	if len(blobs) != 1 || result["hasMore"] != false || result["nextAfter"] != float64(1) {
		t.Fatalf("blobs len = %d, want 1", len(blobs))
	}

	// Download the blob
	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/blobs/"+sha, nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	downloaded := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if !bytes.Equal(downloaded, blobData) {
		t.Fatalf("downloaded blob = %q, want %q", downloaded, blobData)
	}
}

func TestBlobValidationAndSizeLimit(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000006", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)

	for _, tc := range []struct {
		name string
		sha  string
		body []byte
	}{
		{name: "short hash", sha: "abc", body: []byte("data")},
		{name: "uppercase hash", sha: strings.Repeat("A", 64), body: []byte("data")},
		{name: "hash mismatch", sha: strings.Repeat("0", 64), body: []byte("data")},
		{name: "too large", sha: strings.Repeat("0", 64), body: bytes.Repeat([]byte("x"), (64<<20)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/sync/blobs/"+tc.sha, bytes.NewReader(tc.body))
			testutil.SetBearer(req, token)
			var resp *http.Response
			if len(tc.sha) == 64 && tc.sha == strings.ToLower(tc.sha) {
				resp = doSignedBlob(t, ts.URL+"/sync/blobs/"+tc.sha, token, device, randomRequestID(t), tc.body)
			} else {
				resp = testutil.Do(t, req)
			}
			want := http.StatusBadRequest
			if tc.name == "too large" {
				want = http.StatusRequestEntityTooLarge
			}
			testutil.RequireStatus(t, resp, want)
			resp.Body.Close()
		})
	}

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/blobs", nil)
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	if blobs := result["blobs"].([]interface{}); len(blobs) != 0 {
		t.Fatalf("failed uploads left %d blob records", len(blobs))
	}
}

func TestBlobAcceptsMoreThanOneMiB(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000009", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	body := bytes.Repeat([]byte("x"), (1<<20)+1)
	hash := sha256.Sum256(body)

	resp := doSignedBlob(t, ts.URL+"/sync/blobs/"+hex.EncodeToString(hash[:]), token, device, randomRequestID(t), body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestChangesValidationAndLimits(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000007", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)

	tests := []struct {
		name    string
		changes interface{}
	}{
		{name: "unsupported table", changes: []map[string]interface{}{{"table": "unknown", "op": "upsert", "recordId": "1", "data": map[string]interface{}{"id": "1"}}}},
		{name: "unsupported op", changes: []map[string]interface{}{{"table": "messages", "op": "merge", "recordId": "1"}}},
		{name: "missing record id", changes: []map[string]interface{}{{"table": "messages", "op": "delete"}}},
		{name: "long record id", changes: []map[string]interface{}{{"table": "messages", "op": "delete", "recordId": strings.Repeat("x", 257)}}},
		{name: "missing upsert data", changes: []map[string]interface{}{{"table": "messages", "op": "upsert", "recordId": "1"}}},
		{name: "too many changes", changes: makeDeleteChanges(501)},
		{name: "large data", changes: []map[string]interface{}{{"table": "messages", "op": "upsert", "recordId": "1", "data": map[string]interface{}{"value": strings.Repeat("x", (256<<10)+1)}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := uploadSignedChanges(t, ts.URL, token, device, tc.changes)
			want := http.StatusBadRequest
			if tc.name == "too many changes" || tc.name == "large data" {
				want = http.StatusRequestEntityTooLarge
			}
			testutil.RequireStatus(t, resp, want)
			resp.Body.Close()
		})
	}
	requestID := randomRequestID(t)
	missingBody, _ := json.Marshal(map[string]interface{}{"requestId": requestID})
	resp := doSignedSync(t, ts.URL+"/sync/changes", token, device, requestID, missingBody)
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()

	requestID = randomRequestID(t)
	body := `{"requestId":"` + requestID + `","changes":[{"changeId":"large","clientCreatedAt":"2026-07-16T12:00:00Z","table":"messages","op":"upsert","recordId":"1","data":{"value":"` + strings.Repeat("x", 2<<20) + `"}}]}`
	resp = doSignedSync(t, ts.URL+"/sync/changes", token, device, requestID, []byte(body))
	testutil.RequireStatus(t, resp, http.StatusRequestEntityTooLarge)
	resp.Body.Close()

	requestID = randomRequestID(t)
	valid := signedBody(t, requestID, "trailing-change", "trailing-record")
	resp = doSignedSync(t, ts.URL+"/sync/changes", token, device, requestID, append(valid, []byte(` {}`)...))
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestChangesAllowSharedSettingsAndSyncedModelConfigs(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000010", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)

	changes := []map[string]interface{}{
		{"table": "shared_settings", "op": "upsert", "recordId": "app-settings", "data": map[string]interface{}{"id": "app-settings", "schemaVersion": 1}},
		{"table": "synced_model_configs", "op": "upsert", "recordId": "provider-1", "data": map[string]interface{}{"id": "provider-1", "schemaVersion": 1}},
	}
	resp := uploadSignedChanges(t, ts.URL, token, device, changes)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestGetChangesPaginationAndEmptyUpload(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000008", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)

	resp := uploadSignedChanges(t, ts.URL, token, device, makeDeleteChanges(3))
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/changes?since=0&limit=2", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	var page map[string]interface{}
	testutil.DecodeJSON(t, resp, &page)
	resp.Body.Close()
	if len(page["changes"].([]interface{})) != 2 || page["hasMore"] != true || page["nextSince"] != float64(2) || page["latestSeq"] != float64(2) || page["globalLatestSeq"] != float64(3) {
		t.Fatalf("unexpected first page: %#v", page)
	}

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/changes?since=2&limit=2", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.DecodeJSON(t, resp, &page)
	resp.Body.Close()
	if len(page["changes"].([]interface{})) != 1 || page["hasMore"] != false || page["nextSince"] != float64(3) || page["latestSeq"] != float64(3) || page["globalLatestSeq"] != float64(3) {
		t.Fatalf("unexpected final page: %#v", page)
	}

	resp = uploadSignedChanges(t, ts.URL, token, device, []interface{}{})
	var upload map[string]interface{}
	testutil.DecodeJSON(t, resp, &upload)
	resp.Body.Close()
	if upload["latestSeq"] != float64(3) {
		t.Fatalf("empty upload latestSeq = %v, want 3", upload["latestSeq"])
	}

	for _, limit := range []string{"0", "1001", "bad"} {
		req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/changes?limit="+limit, nil)
		testutil.SetBearer(req, token)
		resp = testutil.Do(t, req)
		testutil.RequireStatus(t, resp, http.StatusBadRequest)
		resp.Body.Close()
	}
}

func TestBlobListingPagination(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000011", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	for _, body := range [][]byte{[]byte("first"), []byte("second"), []byte("third")} {
		hash := sha256.Sum256(body)
		resp := doSignedBlob(t, ts.URL+"/sync/blobs/"+hex.EncodeToString(hash[:]), token, device, randomRequestID(t), body)
		testutil.RequireStatus(t, resp, http.StatusOK)
		resp.Body.Close()
	}

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/blobs?limit=2", nil)
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	var page map[string]interface{}
	testutil.DecodeJSON(t, resp, &page)
	resp.Body.Close()
	if len(page["blobs"].([]interface{})) != 2 || page["hasMore"] != true || page["truncated"] != true || page["nextAfter"] != float64(2) || page["returnedCount"] != float64(2) || page["pageSize"] != float64(2) {
		t.Fatalf("unexpected first blob page: %#v", page)
	}

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/blobs?after=2&limit=2", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.DecodeJSON(t, resp, &page)
	resp.Body.Close()
	if len(page["blobs"].([]interface{})) != 1 || page["hasMore"] != false || page["nextAfter"] != float64(3) {
		t.Fatalf("unexpected final blob page: %#v", page)
	}

	for _, query := range []string{"?limit=0", "?limit=1001", "?limit=bad", "?after=bad"} {
		req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/blobs"+query, nil)
		testutil.SetBearer(req, token)
		resp = testutil.Do(t, req)
		testutil.RequireStatus(t, resp, http.StatusBadRequest)
		resp.Body.Close()
	}
}

func makeDeleteChanges(count int) []map[string]interface{} {
	changes := make([]map[string]interface{}, count)
	for i := range changes {
		changes[i] = map[string]interface{}{"table": "messages", "op": "delete", "recordId": "record-" + strconv.Itoa(i)}
	}
	return changes
}

func TestConcurrentUploadChanges(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000005", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)

	var wg stdsync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			changes := []map[string]interface{}{{
				"table":    "conversations",
				"op":       "upsert",
				"recordId": "conv-concurrent-" + string(rune('a'+i)),
				"data":     map[string]interface{}{"id": "conv-concurrent"},
			}}
			resp := uploadSignedChanges(t, ts.URL, token, device, changes)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				data := testutil.ReadAll(t, resp.Body)
				errs <- &statusError{status: resp.StatusCode, body: string(data)}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/changes?since=0", nil)
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()
	if result["latestSeq"] != float64(2) {
		t.Fatalf("latestSeq = %v, want 2", result["latestSeq"])
	}
}

func uploadSignedChanges(t testing.TB, baseURL, token string, device syncDevice, changes interface{}) *http.Response {
	t.Helper()
	raw, err := json.Marshal(changes)
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]interface{}
	if err := json.Unmarshal(raw, &records); err == nil {
		for i := range records {
			if _, ok := records[i]["changeId"]; !ok {
				records[i]["changeId"] = "test-change-" + randomRequestID(t)
			}
			if _, ok := records[i]["clientCreatedAt"]; !ok {
				records[i]["clientCreatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
			}
		}
		changes = records
	}
	requestID := randomRequestID(t)
	body, err := json.Marshal(map[string]interface{}{"requestId": requestID, "changes": changes})
	if err != nil {
		t.Fatal(err)
	}
	return doSignedSync(t, baseURL+"/sync/changes", token, device, requestID, body)
}

type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return "status " + http.StatusText(e.status) + ": " + e.body
}

func TestSyncRequiresAuth(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	resp, err := http.Get(ts.URL + "/sync/status")
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}
