package sync

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/lynai/backend/internal/database"
)

func TestCanonicalSyncV1Fixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/sync-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cryptography struct {
			SeedHex, UserID, SessionID, DeviceID, RequestID string
			CanonicalTarget, BodySHA256, MessageHex         string
			SignatureBase64URL                              string
			TimestampMS, ExpectedGeneration                 int64
		} `json:"cryptography"`
		Status SyncStatus `json:"status"`
		Upload struct {
			RequestID, ExactBodyUTF8, BodySHA256 string
			Request                              json.RawMessage
		} `json:"upload"`
		PullPage struct {
			Changes                                   []ChangeWithSeq `json:"changes"`
			LatestSeq, GlobalLatestSeq, Generation    int64
			IndexRevision, MinAvailableSeq, NextSince int64
			HasMore                                   bool
		} `json:"pullPage"`
		SelectivePurge struct {
			Result struct {
				Operation database.SyncManagementOperation `json:"operation"`
			} `json:"result"`
		} `json:"selectivePurge"`
		OperationAck struct {
			Scope, CanonicalTarget, RequestID string
			Request                           struct {
				RequestID          string `json:"requestId"`
				ExpectedGeneration int64  `json:"expectedGeneration"`
				OperationID        string `json:"operationId"`
			} `json:"request"`
		} `json:"operationAck"`
		Conflicts map[string]struct {
			HTTPStatus int `json:"httpStatus"`
			Body       struct {
				Code string `json:"code"`
			} `json:"body"`
		} `json:"conflicts"`
		Blobs struct {
			ListPage struct {
				Blobs []database.SyncBlob `json:"blobs"`
			} `json:"listPage"`
		} `json:"blobs"`
		ClientOnlyExtensions struct {
			LANLineage struct {
				Change map[string]interface{} `json:"change"`
			} `json:"lanLineage"`
		} `json:"clientOnlyExtensions"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	seed := mustDecodeHex(t, fixture.Cryptography.SeedHex)
	bodyHash := mustDecodeHex(t, fixture.Cryptography.BodySHA256)
	message := SyncRequestMessage(1, fixture.Cryptography.UserID, fixture.Cryptography.SessionID,
		fixture.Cryptography.DeviceID, fixture.Cryptography.TimestampMS, fixture.Cryptography.RequestID,
		"POST", fixture.Cryptography.CanonicalTarget, bodyHash, fixture.Cryptography.ExpectedGeneration)
	if hex.EncodeToString(message) != fixture.Cryptography.MessageHex {
		t.Fatal("fixture sync signature message does not match encoder")
	}
	signature := ed25519.Sign(ed25519.NewKeyFromSeed(seed), message)
	if base64.RawURLEncoding.EncodeToString(signature) != fixture.Cryptography.SignatureBase64URL {
		t.Fatal("fixture sync signature does not match vector")
	}

	exactBody := []byte(fixture.Upload.ExactBodyUTF8)
	digest := sha256.Sum256(exactBody)
	if hex.EncodeToString(digest[:]) != fixture.Upload.BodySHA256 {
		t.Fatal("fixture upload body hash is stale")
	}
	if !jsonEqual(t, exactBody, fixture.Upload.Request) {
		t.Fatal("fixture exact upload body differs from parsed request")
	}
	var upload uploadChangesRequest
	decoder := json.NewDecoder(bytes.NewReader(exactBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&upload); err != nil {
		t.Fatal(err)
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		t.Fatal(err)
	}
	if upload.RequestID != fixture.Upload.RequestID || upload.ExpectedGeneration != 1 || upload.Changes == nil || len(*upload.Changes) != 2 {
		t.Fatalf("unexpected upload fixture: %+v", upload)
	}
	for _, change := range *upload.Changes {
		if change.DeviceID != "" {
			t.Fatal("canonical upload change contains deprecated deviceId")
		}
	}

	if !fixture.Status.Capabilities.Index || !fixture.Status.Capabilities.SelectivePurge ||
		!fixture.Status.Capabilities.FullPurge || !fixture.Status.Capabilities.OperationAck {
		t.Fatal("fixture status capabilities are incomplete")
	}
	if fixture.Status.Limits != syncLimits {
		t.Fatalf("fixture limits = %+v, want %+v", fixture.Status.Limits, syncLimits)
	}
	if len(fixture.PullPage.Changes) != 2 || fixture.PullPage.NextSince != 2 ||
		fixture.PullPage.LatestSeq != 2 || fixture.PullPage.GlobalLatestSeq != 2 || fixture.PullPage.HasMore {
		t.Fatalf("fixture pull page is incomplete: %+v", fixture.PullPage)
	}
	for index, change := range fixture.PullPage.Changes {
		if change.Seq != int64(index+1) || change.DeviceID == nil || *change.DeviceID != fixture.Cryptography.DeviceID {
			t.Fatalf("invalid pull change %d: %+v", index, change)
		}
	}

	operation := fixture.SelectivePurge.Result.Operation
	if operation.Kind != "selective" || operation.Generation != 2 || operation.IndexRevision != 3 {
		t.Fatalf("invalid selective purge operation: %+v", operation)
	}
	ackIdentity := "cloud-operation-ack\n" + fixture.OperationAck.Scope + "\n" + operation.ID + "\n2"
	ackDigest := sha256.Sum256([]byte(ackIdentity))
	ackRequestID := base64.RawURLEncoding.EncodeToString(ackDigest[:24])
	if fixture.OperationAck.CanonicalTarget != "/sync/manage/operations/:id/ack" ||
		ackRequestID != fixture.OperationAck.RequestID || fixture.OperationAck.Request.OperationID != operation.ID {
		t.Fatalf("invalid hardened ACK fixture: %+v", fixture.OperationAck)
	}
	for name, code := range map[string]string{"generation": "generation_mismatch", "replay": "replay_conflict", "indexRevision": "index_revision_conflict"} {
		conflict := fixture.Conflicts[name]
		if conflict.HTTPStatus != 409 || conflict.Body.Code != code {
			t.Fatalf("invalid %s conflict fixture: %+v", name, conflict)
		}
	}
	if len(fixture.Blobs.ListPage.Blobs) != 1 || fixture.Blobs.ListPage.Blobs[0].Size != 17 {
		t.Fatal("invalid blob metadata fixture")
	}
	blobDigest := sha256.Sum256([]byte("sync fixture blob"))
	if fixture.Blobs.ListPage.Blobs[0].SHA256 != hex.EncodeToString(blobDigest[:]) {
		t.Fatal("fixture blob hash is stale")
	}
	if fixture.ClientOnlyExtensions.LANLineage.Change["lineage"] != "dataset-fixture-1" || bytes.Contains(exactBody, []byte("lineage")) {
		t.Fatal("LAN lineage is not isolated to the client-only extension")
	}
}

func mustDecodeHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func jsonEqual(t *testing.T, left, right []byte) bool {
	t.Helper()
	var leftValue, rightValue interface{}
	if err := json.Unmarshal(left, &leftValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(mustJSON(t, leftValue), mustJSON(t, rightValue))
}

func mustJSON(t *testing.T, value interface{}) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
