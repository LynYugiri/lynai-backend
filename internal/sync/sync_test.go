package sync_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	stdsync "sync"
	"testing"

	"github.com/lynai/backend/internal/testutil"
)

const testPassword = "secret123"

func registerAndGetToken(t *testing.T, tsURL, phone string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"phone": phone, "password": testPassword})
	resp, _ := http.Post(tsURL+"/auth/register", "application/json", bytes.NewReader(body))
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	return result["token"].(map[string]interface{})["accessToken"].(string)
}

func TestSyncStatusEmpty(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := registerAndGetToken(t, ts.URL, "13100000001")

	req, _ := http.NewRequest("GET", ts.URL+"/sync/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	if result["lastSeq"] != float64(0) {
		t.Fatalf("lastSeq = %v, want 0", result["lastSeq"])
	}
}

func TestUploadChanges(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := registerAndGetToken(t, ts.URL, "13100000002")

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
	body, _ := json.Marshal(map[string]interface{}{"changes": changes})
	req, _ := http.NewRequest("POST", ts.URL+"/sync/changes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("upload status = %d, want 200", resp.StatusCode)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
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

	token := registerAndGetToken(t, ts.URL, "13100000003")

	// Upload some changes
	changes := []map[string]interface{}{
		{"table": "conversations", "op": "upsert", "recordId": "c1", "data": map[string]interface{}{"id": "c1"}},
		{"table": "messages", "op": "delete", "recordId": "m1"},
	}
	body, _ := json.Marshal(map[string]interface{}{"changes": changes})
	req, _ := http.NewRequest("POST", ts.URL+"/sync/changes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Get changes since seq=0
	req, _ = http.NewRequest("GET", ts.URL+"/sync/changes?since=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("get changes status = %d, want 200", resp.StatusCode)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()

	changeList, _ := result["changes"].([]interface{})
	if len(changeList) != 2 {
		t.Fatalf("changes len = %d, want 2", len(changeList))
	}
	if result["latestSeq"] != float64(2) {
		t.Fatalf("latestSeq = %v, want 2", result["latestSeq"])
	}

	// Get changes since seq=1 — should only return seq=2
	req, _ = http.NewRequest("GET", ts.URL+"/sync/changes?since=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = http.DefaultClient.Do(req)
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	changeList, _ = result["changes"].([]interface{})
	if len(changeList) != 1 {
		t.Fatalf("changes since 1 len = %d, want 1", len(changeList))
	}
}

func TestSyncBlobs(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := registerAndGetToken(t, ts.URL, "13100000004")

	// Upload a blob
	blobData := []byte("test blob content")
	req, _ := http.NewRequest("POST", ts.URL+"/sync/blobs/abc123def456", bytes.NewReader(blobData))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("upload blob status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// List blobs
	req, _ = http.NewRequest("GET", ts.URL+"/sync/blobs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = http.DefaultClient.Do(req)
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	blobs, _ := result["blobs"].([]interface{})
	if len(blobs) != 1 {
		t.Fatalf("blobs len = %d, want 1", len(blobs))
	}

	// Download the blob
	req, _ = http.NewRequest("GET", ts.URL+"/sync/blobs/abc123def456", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("download blob status = %d, want 200", resp.StatusCode)
	}
	downloaded, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(downloaded, blobData) {
		t.Fatalf("downloaded blob = %q, want %q", downloaded, blobData)
	}
}

func TestConcurrentUploadChanges(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()

	token := registerAndGetToken(t, ts.URL, "13100000005")

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
			body, _ := json.Marshal(map[string]interface{}{"changes": changes})
			req, _ := http.NewRequest("POST", ts.URL+"/sync/changes", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				data, _ := io.ReadAll(resp.Body)
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

	req, _ := http.NewRequest("GET", ts.URL+"/sync/changes?since=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
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
	if resp.StatusCode != 401 {
		t.Fatalf("sync without auth status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}
