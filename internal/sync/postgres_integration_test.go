package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/pgtest"
	"gorm.io/gorm"
)

func TestPostgresReadAPIsUseOneNonLockingSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(*Service) (int64, int, error)
	}{
		{name: "index list", read: func(s *Service) (int64, int, error) {
			page, err := s.ListIndexObjects(42, "messages", "", 10, 1)
			return page.IndexRevision, len(page.Objects), err
		}},
		{name: "purge preview", read: func(s *Service) (int64, int, error) {
			preview, err := s.PreviewPurge(42, PurgeSelector{Type: "category", Category: "messages"}, 1)
			return preview.IndexRevision, int(preview.RecordCount), err
		}},
		{name: "object detail", read: func(s *Service) (int64, int, error) {
			detail, err := s.GetIndexObject(42, "messages", "conversation-1", 1)
			return detail.IndexRevision, len(detail.Records), err
		}},
		{name: "changes page", read: func(s *Service) (int64, int, error) {
			page, err := s.GetChangesPage(42, 0, 10)
			return page.IndexRevision, len(page.Changes), err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := pgtest.Open(t)
			if err := database.Migrate(context.Background(), db); err != nil {
				t.Fatalf("migrate PostgreSQL schema: %v", err)
			}
			storage, err := NewBlobStorage(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(db, storage)
			if _, err := service.UploadChanges(42, []ChangeRecord{postgresMessage("change-1", "message-1", "conversation-1")}); err != nil {
				t.Fatalf("seed change: %v", err)
			}

			metadataRead := make(chan struct{})
			releaseRead := make(chan struct{})
			var paused atomic.Bool
			callbackName := "test:pause_snapshot_" + tc.name
			if err := db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement.Table == "sync_metadata" && tx.Error == nil && paused.CompareAndSwap(false, true) {
					close(metadataRead)
					<-releaseRead
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Callback().Query().Remove(callbackName) })

			type readResult struct {
				revision int64
				count    int
				err      error
			}
			readDone := make(chan readResult, 1)
			go func() {
				revision, count, err := tc.read(service)
				readDone <- readResult{revision: revision, count: count, err: err}
			}()
			select {
			case <-metadataRead:
			case <-time.After(5 * time.Second):
				t.Fatal("read API did not reach metadata snapshot")
			}

			writeDone := make(chan error, 1)
			go func() {
				_, err := service.UploadChanges(42, []ChangeRecord{postgresMessage("change-2", "message-2", "conversation-2")})
				writeDone <- err
			}()
			select {
			case err := <-writeDone:
				if err != nil {
					t.Fatalf("concurrent upload: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("read-only API blocked a concurrent metadata update")
			}
			close(releaseRead)
			result := <-readDone
			if result.err != nil {
				t.Fatalf("read API: %v", result.err)
			}
			if result.revision != 1 || result.count != 1 {
				t.Fatalf("snapshot result = revision %d, count %d; want revision 1, count 1", result.revision, result.count)
			}
		})
	}
}

func TestPostgresPurgeUploadAndConsecutivePurgeCoalescing(t *testing.T) {
	db := pgtest.Open(t)
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate PostgreSQL schema: %v", err)
	}
	storage, err := NewBlobStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(db, storage)
	if _, err := service.UploadChanges(42, []ChangeRecord{postgresMessage("change-1", "message-1", "conversation-1")}); err != nil {
		t.Fatal(err)
	}

	firstID := "11111111111111111111111111111111"
	firstHash := sha256.Sum256([]byte("first purge"))
	first, err := service.PurgeIdempotent(42, firstID, firstHash[:], "POST /sync/manage/purge", "signed-device", PurgeSelector{Type: "category", Category: "messages"}, 1, 1)
	if err != nil {
		t.Fatalf("first purge: %v", err)
	}
	if _, err := service.UploadChangesIdempotent(42, "22222222222222222222222222222222", bytes.Repeat([]byte{2}, 32), "POST /sync/changes", "signed-device", 2, []ChangeRecord{postgresTask("change-2", "task-1")}); err != nil {
		t.Fatalf("upload after purge: %v", err)
	}

	secondID := "33333333333333333333333333333333"
	secondHash := sha256.Sum256([]byte("second purge"))
	if _, err := service.PurgeIdempotent(42, secondID, secondHash[:], "POST /sync/manage/purge", "signed-device", PurgeSelector{Type: "all"}, 3, 2); err != nil {
		t.Fatalf("second purge: %v", err)
	}
	operations, err := service.PendingOperations(42)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != secondID || operations[0].Generation != 3 || operations[0].CreatedByDeviceID != "signed-device" {
		t.Fatalf("pending operations = %+v", operations)
	}
	replayed, err := service.PurgeIdempotent(42, firstID, firstHash[:], "POST /sync/manage/purge", "forged-device", PurgeSelector{Type: "category", Category: "messages"}, 1, 1)
	if err != nil {
		t.Fatalf("replay superseded purge: %v", err)
	}
	if !bytes.Equal(first.Body, replayed.Body) {
		t.Fatalf("superseded replay differs:\n%s\n%s", first.Body, replayed.Body)
	}
}

func postgresMessage(changeID, recordID, conversationID string) ChangeRecord {
	data, _ := json.Marshal(map[string]string{"id": recordID, "conversationId": conversationID})
	return ChangeRecord{ChangeID: changeID, Table: "messages", Op: "upsert", RecordID: recordID, Data: data, ClientCreatedAt: time.Now().UTC()}
}

func postgresTask(changeID, recordID string) ChangeRecord {
	data := json.RawMessage(fmt.Sprintf(`{"id":%q}`, recordID))
	return ChangeRecord{ChangeID: changeID, Table: "tasks", Op: "upsert", RecordID: recordID, Data: data, ClientCreatedAt: time.Now().UTC()}
}
