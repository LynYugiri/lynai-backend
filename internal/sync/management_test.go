package sync_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	syncapi "github.com/lynai/backend/internal/sync"
	"github.com/lynai/backend/internal/testutil"
)

func TestIndexPurgeReplayAndOperationAck(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000101", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	changes := []map[string]interface{}{
		{"table": "conversations", "op": "upsert", "recordId": "conversation-1", "data": map[string]interface{}{"id": "conversation-1"}},
		{"table": "messages", "op": "upsert", "recordId": "message-1", "data": map[string]interface{}{"id": "message-1", "conversationId": "conversation-1"}},
		{"table": "messages", "op": "upsert", "recordId": "message-2", "data": map[string]interface{}{"id": "message-2", "conversationId": "conversation-2"}},
	}
	resp := uploadSignedChanges(t, ts.URL, token, device, changes)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/index/status", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var status map[string]interface{}
	testutil.DecodeJSON(t, resp, &status)
	resp.Body.Close()
	revision := int64(status["indexRevision"].(float64))

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/index/objects?category=messages&limit=1&expectedIndexRevision="+strconv.FormatInt(revision, 10), nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var page map[string]interface{}
	testutil.DecodeJSON(t, resp, &page)
	resp.Body.Close()
	if len(page["objects"].([]interface{})) != 1 || page["hasMore"] != true || page["nextAfter"] == "" {
		t.Fatalf("unexpected index page: %#v", page)
	}

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/index/objects/messages/conversation-1?expectedIndexRevision="+strconv.FormatInt(revision, 10), nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var detail map[string]interface{}
	testutil.DecodeJSON(t, resp, &detail)
	resp.Body.Close()
	if detail["recordCount"] != float64(1) || len(detail["records"].([]interface{})) != 1 {
		t.Fatalf("unexpected object detail: %#v", detail)
	}

	preview := map[string]interface{}{"expectedIndexRevision": revision, "selector": map[string]interface{}{"type": "object", "category": "messages", "objectId": "conversation-1"}}
	resp = authenticatedJSON(t, http.MethodPost, ts.URL+"/sync/manage/purge/preview", token, preview)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var previewResult map[string]interface{}
	testutil.DecodeJSON(t, resp, &previewResult)
	resp.Body.Close()
	if previewResult["recordCount"] != float64(1) || previewResult["changeCount"] != float64(1) {
		t.Fatalf("unexpected purge preview: %#v", previewResult)
	}

	requestID := randomRequestID(t)
	body, _ := json.Marshal(map[string]interface{}{"requestId": requestID, "expectedGeneration": 1, "expectedIndexRevision": revision, "selector": map[string]interface{}{"type": "object", "category": "messages", "objectId": "conversation-1"}})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, requestID, 1, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	firstBody := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, requestID, 1, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	secondBody := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if !bytes.Equal(firstBody, secondBody) {
		t.Fatal("purge replay response bytes differ")
	}
	var purge map[string]interface{}
	if err := json.Unmarshal(firstBody, &purge); err != nil {
		t.Fatal(err)
	}
	operationID := purge["operation"].(map[string]interface{})["id"].(string)

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/manage/operations", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	var pending map[string]interface{}
	testutil.DecodeJSON(t, resp, &pending)
	resp.Body.Close()
	if len(pending["operations"].([]interface{})) != 1 {
		t.Fatalf("pending operations = %#v", pending)
	}

	ackID := randomRequestID(t)
	ackBody, _ := json.Marshal(map[string]interface{}{"requestId": ackID, "expectedGeneration": 1})
	ackTarget := "/sync/manage/operations/:id/ack"
	resp = doSignedManagement(t, ts.URL+"/sync/manage/operations/"+operationID+"/ack", ackTarget, token, device, ackID, 1, ackBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/manage/operations", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.DecodeJSON(t, resp, &pending)
	resp.Body.Close()
	if len(pending["operations"].([]interface{})) != 0 {
		t.Fatalf("acked operation remained pending: %#v", pending)
	}

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/index/objects?category=messages&expectedIndexRevision="+strconv.FormatInt(revision, 10), nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestFullPurgeSwitchesGenerationWithoutDeletingBlobMetadata(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000102", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	blob := []byte("retained blob")
	hash := sha256.Sum256(blob)
	resp := doSignedBlob(t, ts.URL+"/sync/blobs/"+hex.EncodeToString(hash[:]), token, device, randomRequestID(t), blob)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = uploadSignedChanges(t, ts.URL, token, device, []map[string]interface{}{{"table": "resources", "op": "upsert", "recordId": "resource-1", "data": map[string]interface{}{"id": "resource-1", "sha256": hex.EncodeToString(hash[:])}}})
	resp.Body.Close()

	requestID := randomRequestID(t)
	body, _ := json.Marshal(map[string]interface{}{"requestId": requestID, "expectedGeneration": 1, "expectedIndexRevision": 1, "selector": map[string]interface{}{"type": "all"}})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, requestID, 1, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	first := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, requestID, 1, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	second := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if !bytes.Equal(first, second) {
		t.Fatal("full purge replay response bytes differ")
	}

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/index/status", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	var status map[string]interface{}
	testutil.DecodeJSON(t, resp, &status)
	resp.Body.Close()
	if status["generation"] != float64(2) || status["lastSeq"] != float64(0) || status["indexRevision"] != float64(2) || status["blobCount"] != float64(1) {
		t.Fatalf("unexpected status after full purge: %#v", status)
	}
}

func TestNotePurgeIncludesHeadsAndTombstonesLinkedThroughPage(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000110", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	changes := []map[string]interface{}{
		{"table": "note_page_heads", "op": "upsert", "recordId": "page-1", "data": map[string]interface{}{"id": "page-1", "pageId": "page-1", "headIds": []string{"revision-1"}}},
		{"table": "note_page_tombstones", "op": "upsert", "recordId": "page-1:revision-old", "data": map[string]interface{}{"id": "page-1:revision-old", "pageId": "page-1", "revisionId": "revision-old"}},
		{"table": "note_pages", "op": "upsert", "recordId": "page-1", "data": map[string]interface{}{"id": "page-1", "noteId": "note-1"}},
		{"table": "notes", "op": "upsert", "recordId": "note-1", "data": map[string]interface{}{"id": "note-1"}},
	}
	resp := uploadSignedChanges(t, ts.URL, token, device, changes)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	requestID := randomRequestID(t)
	body, _ := json.Marshal(map[string]interface{}{"requestId": requestID, "expectedGeneration": 1, "expectedIndexRevision": 4, "selector": map[string]interface{}{"type": "object", "category": "notes", "objectId": "note-1"}})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, requestID, 1, body)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result map[string]interface{}
	testutil.DecodeJSON(t, resp, &result)
	resp.Body.Close()
	op := result["operation"].(map[string]interface{})
	if op["deletedRecordCount"] != float64(4) || op["deletedChangeCount"] != float64(4) {
		t.Fatalf("note purge result = %#v", result)
	}
}

func doSignedManagement(t testing.TB, target, canonicalTarget, token string, device syncDevice, requestID string, generation int64, body []byte) *http.Response {
	t.Helper()
	digest := sha256.Sum256(body)
	timestamp := time.Now().UnixMilli()
	message := syncapi.SyncRequestMessage(1, device.userID, device.sessionID, device.deviceID, timestamp, requestID, http.MethodPost, canonicalTarget, digest[:], generation)
	req := testutil.NewRequest(t, http.MethodPost, target, bytes.NewReader(body))
	testutil.SetBearer(req, token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LynAI-Protocol", "1")
	req.Header.Set("X-LynAI-Device-ID", device.deviceID)
	req.Header.Set("X-LynAI-Timestamp", strconv.FormatInt(timestamp, 10))
	req.Header.Set("X-LynAI-Request-ID", requestID)
	req.Header.Set("X-LynAI-Body-SHA256", hex.EncodeToString(digest[:]))
	req.Header.Set("X-LynAI-Expected-Generation", strconv.FormatInt(generation, 10))
	req.Header.Set("X-LynAI-Signature", base64.RawURLEncoding.EncodeToString(ed25519.Sign(device.privateKey, message)))
	return testutil.Do(t, req)
}
