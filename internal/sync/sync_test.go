package sync_test

import (
	"bytes"
	"net/http"
	stdsync "sync"
	"testing"

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
}

func TestUploadChanges(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000002", testPassword)

	changes := []map[string]interface{}{
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
	req := testutil.NewJSONRequest(t, http.MethodPost, ts.URL+"/sync/changes", map[string]interface{}{"changes": changes})
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()

	if result["latestSeq"] != float64(2) {
		t.Fatalf("latestSeq = %v, want 2", result["latestSeq"])
	}
	changeList, _ := result["changes"].([]interface{})
	if len(changeList) != 2 {
		t.Fatalf("changes len = %d, want 2", len(changeList))
	}
}

func TestGetChanges(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000003", testPassword)

	// Upload some changes
	changes := []map[string]interface{}{
		{"table": "conversations", "op": "upsert", "recordId": "c1", "data": map[string]interface{}{"id": "c1"}},
		{"table": "messages", "op": "delete", "recordId": "m1"},
	}
	req := testutil.NewJSONRequest(t, http.MethodPost, ts.URL+"/sync/changes", map[string]interface{}{"changes": changes})
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Get changes since seq=0
	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/changes?since=0", nil)
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

	// Upload a blob
	blobData := []byte("test blob content")
	req := testutil.NewRequest(t, http.MethodPost, ts.URL+"/sync/blobs/abc123def456", bytes.NewReader(blobData))
	testutil.SetBearer(req, token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// List blobs
	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/blobs", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()
	blobs, _ := result["blobs"].([]interface{})
	if len(blobs) != 1 {
		t.Fatalf("blobs len = %d, want 1", len(blobs))
	}

	// Download the blob
	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/blobs/abc123def456", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	downloaded := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if !bytes.Equal(downloaded, blobData) {
		t.Fatalf("downloaded blob = %q, want %q", downloaded, blobData)
	}
}

func TestConcurrentUploadChanges(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000005", testPassword)

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
			req := testutil.NewJSONRequest(t, http.MethodPost, ts.URL+"/sync/changes", map[string]interface{}{"changes": changes})
			testutil.SetBearer(req, token)
			resp := testutil.Do(t, req)
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
