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
	if detail["indexRevision"] != float64(revision) {
		t.Fatalf("object detail revision = %#v", detail)
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
	operation := purge["operation"].(map[string]interface{})
	operationID := operation["id"].(string)
	if operation["generation"] != float64(2) {
		t.Fatalf("selective purge operation = %#v", operation)
	}

	replayConflictBody, _ := json.Marshal(map[string]interface{}{"requestId": requestID, "expectedGeneration": 1, "expectedIndexRevision": revision + 1, "selector": map[string]interface{}{"type": "category", "category": "messages"}})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, requestID, 1, replayConflictBody)
	testutil.RequireStatus(t, resp, http.StatusConflict)
	var replayConflict map[string]interface{}
	testutil.DecodeJSON(t, resp, &replayConflict)
	resp.Body.Close()
	if replayConflict["code"] != "replay_conflict" {
		t.Fatalf("management replay conflict = %#v", replayConflict)
	}

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
	ackBody, _ := json.Marshal(map[string]interface{}{"requestId": ackID, "expectedGeneration": 2, "operationId": operationID})
	ackTarget := "/sync/manage/operations/:id/ack"
	resp = doSignedManagement(t, ts.URL+"/sync/manage/operations/"+operationID+"/ack", ackTarget, token, device, ackID, 2, ackBody)
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

	statusResp := authenticatedJSON(t, http.MethodGet, ts.URL+"/sync/status", token, nil)
	var statusAfterSelective map[string]interface{}
	testutil.DecodeJSON(t, statusResp, &statusAfterSelective)
	statusResp.Body.Close()
	if statusAfterSelective["generation"] != float64(2) || statusAfterSelective["lastSeq"] != float64(3) || statusAfterSelective["minAvailableSeq"] != float64(3) {
		t.Fatalf("status after selective purge = %#v", statusAfterSelective)
	}
	for _, since := range []string{"0", "999"} {
		req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/changes?since="+since, nil)
		testutil.SetBearer(req, token)
		req.Header.Set("X-LynAI-Expected-Generation", "1")
		resp = testutil.Do(t, req)
		testutil.RequireStatus(t, resp, http.StatusConflict)
		var generationMismatch map[string]interface{}
		testutil.DecodeJSON(t, resp, &generationMismatch)
		resp.Body.Close()
		if generationMismatch["code"] != "generation_mismatch" || generationMismatch["expectedGeneration"] != float64(1) || generationMismatch["currentGeneration"] != float64(2) {
			t.Fatalf("old generation pull response for since=%s: %#v", since, generationMismatch)
		}
	}

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/changes?since=0", nil)
	testutil.SetBearer(req, token)
	req.Header.Set("X-LynAI-Expected-Generation", "2")
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusConflict)
	var stale map[string]interface{}
	testutil.DecodeJSON(t, resp, &stale)
	resp.Body.Close()
	if stale["code"] != "stale_cursor" || stale["minAvailableSeq"] != float64(3) || stale["indexRevision"] != float64(revision+1) {
		t.Fatalf("new generation stale cursor response = %#v", stale)
	}

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/index/objects/messages/conversation-2?expectedIndexRevision="+strconv.FormatInt(revision+1, 10), nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var remainingDetail map[string]interface{}
	testutil.DecodeJSON(t, resp, &remainingDetail)
	resp.Body.Close()
	if remainingDetail["latestSeq"] != float64(3) || remainingDetail["recordCount"] != float64(1) {
		t.Fatalf("remaining selective-purge object = %#v", remainingDetail)
	}

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/index/objects?category=messages&expectedIndexRevision="+strconv.FormatInt(revision, 10), nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestAckOperationOptionalIDMustMatchPath(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000112", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	resp := uploadSignedChanges(t, ts.URL, token, device, []map[string]interface{}{{"table": "messages", "op": "delete", "recordId": "ack-path"}})
	resp.Body.Close()
	purgeID := randomRequestID(t)
	purgeBody, _ := json.Marshal(map[string]interface{}{"requestId": purgeID, "expectedGeneration": 1, "expectedIndexRevision": 1, "selector": map[string]interface{}{"type": "all"}})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, purgeID, 1, purgeBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	ackID := randomRequestID(t)
	body, _ := json.Marshal(map[string]interface{}{"requestId": ackID, "expectedGeneration": 2, "operationId": "different-operation"})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/operations/"+purgeID+"/ack", "/sync/manage/operations/:id/ack", token, device, ackID, 2, body)
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()

	legacyAckID := randomRequestID(t)
	legacyBody, _ := json.Marshal(map[string]interface{}{"requestId": legacyAckID, "expectedGeneration": 2})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/operations/"+purgeID+"/ack", "/sync/manage/operations/:id/ack", token, device, legacyAckID, 2, legacyBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestConsecutivePurgesKeepOnlyLatestPendingAndReplayOlder(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000115", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	resp := uploadSignedChanges(t, ts.URL, token, device, []map[string]interface{}{{"table": "messages", "op": "delete", "recordId": "first"}})
	resp.Body.Close()

	firstID := randomRequestID(t)
	firstBody, _ := json.Marshal(map[string]interface{}{"requestId": firstID, "expectedGeneration": 1, "expectedIndexRevision": 1, "selector": map[string]interface{}{"type": "category", "category": "messages"}})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, firstID, 1, firstBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	firstResponse := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()

	resp = uploadSignedChangesWithGeneration(t, ts.URL, token, device, 2, []map[string]interface{}{{"table": "tasks", "op": "delete", "recordId": "second"}})
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	secondID := randomRequestID(t)
	secondBody, _ := json.Marshal(map[string]interface{}{"requestId": secondID, "expectedGeneration": 2, "expectedIndexRevision": 3, "selector": map[string]interface{}{"type": "all"}})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, secondID, 2, secondBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/manage/operations", nil)
	testutil.SetBearer(req, token)
	resp = testutil.Do(t, req)
	var pending map[string]interface{}
	testutil.DecodeJSON(t, resp, &pending)
	resp.Body.Close()
	operations := pending["operations"].([]interface{})
	if len(operations) != 1 || operations[0].(map[string]interface{})["id"] != secondID {
		t.Fatalf("pending operations = %#v", pending)
	}

	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, firstID, 1, firstBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	replayed := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if !bytes.Equal(firstResponse, replayed) {
		t.Fatalf("superseded purge replay differs:\n%s\n%s", firstResponse, replayed)
	}

	ackID := randomRequestID(t)
	ackBody, _ := json.Marshal(map[string]interface{}{"requestId": ackID, "expectedGeneration": 3, "operationId": firstID})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/operations/"+firstID+"/ack", "/sync/manage/operations/:id/ack", token, device, ackID, 3, ackBody)
	testutil.RequireStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestPurgeGenerationConflictPrecedesIndexRevisionConflict(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000116", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	resp := uploadSignedChanges(t, ts.URL, token, device, []map[string]interface{}{{"table": "messages", "op": "delete", "recordId": "seed"}})
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	firstID := randomRequestID(t)
	firstBody, _ := json.Marshal(map[string]interface{}{"requestId": firstID, "expectedGeneration": 1, "expectedIndexRevision": 1, "selector": map[string]interface{}{"type": "all"}})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, firstID, 1, firstBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	staleID := randomRequestID(t)
	staleBody, _ := json.Marshal(map[string]interface{}{"requestId": staleID, "expectedGeneration": 1, "expectedIndexRevision": 999, "selector": map[string]interface{}{"type": "all"}})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, staleID, 1, staleBody)
	testutil.RequireStatus(t, resp, http.StatusConflict)
	var conflict map[string]interface{}
	testutil.DecodeJSON(t, resp, &conflict)
	resp.Body.Close()
	if conflict["code"] != "generation_mismatch" || conflict["currentGeneration"] != float64(2) {
		t.Fatalf("purge conflict precedence = %#v", conflict)
	}
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

func TestPurgeRetainsUnexpiredRequestReplays(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000113", testPassword)
	device := enrollSyncDevice(t, ts.URL, token)
	changeRequestID := randomRequestID(t)
	changeBody := signedBody(t, changeRequestID, "retained-replay-change", "retained-replay-record")
	resp := doSignedSync(t, ts.URL+"/sync/changes", token, device, changeRequestID, changeBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	original := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()

	purgeID := randomRequestID(t)
	purgeBody, _ := json.Marshal(map[string]interface{}{"requestId": purgeID, "expectedGeneration": 1, "expectedIndexRevision": 1, "selector": map[string]interface{}{"type": "all"}})
	resp = doSignedManagement(t, ts.URL+"/sync/manage/purge", "/sync/manage/purge", token, device, purgeID, 1, purgeBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doSignedSync(t, ts.URL+"/sync/changes", token, device, changeRequestID, changeBody)
	testutil.RequireStatus(t, resp, http.StatusOK)
	replayed := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if !bytes.Equal(original, replayed) {
		t.Fatalf("retained replay differs:\n%s\n%s", original, replayed)
	}
}

func TestIndexCursorValidationIsCanonicalBoundedUTF8(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	token := testutil.RegisterAndGetToken(t, ts.URL, "13100000114", testPassword)
	for _, cursor := range []string{
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("x"), 257)),
		base64.RawURLEncoding.EncodeToString([]byte{0xff}),
		base64.RawURLEncoding.EncodeToString([]byte("x")) + "=",
	} {
		req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/sync/index/objects?category=messages&expectedIndexRevision=0&after="+cursor, nil)
		testutil.SetBearer(req, token)
		resp := testutil.Do(t, req)
		testutil.RequireStatus(t, resp, http.StatusBadRequest)
		resp.Body.Close()
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
