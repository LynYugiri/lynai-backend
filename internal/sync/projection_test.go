package sync

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestUploadChangesMaintainsProjectionAndBlobRefs(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.SyncMetadata{}, &database.SyncChange{}, &database.SyncRecord{}, &database.SyncRecordBlob{}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, nil)
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	hashes := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	changes := []ChangeRecord{
		{ChangeID: "resource-change", Table: "resources", Op: "upsert", RecordID: "resource-1", Data: rawJSON(t, map[string]interface{}{"id": "resource-1", "sha256": hashes[0]}), ClientCreatedAt: now},
		{ChangeID: "revision-change", Table: "note_revisions", Op: "upsert", RecordID: "revision-1", Data: rawJSON(t, map[string]interface{}{"id": "revision-1", "contentHash": hashes[1]}), ClientCreatedAt: now},
		{ChangeID: "plugin-change", Table: "plugin_files", Op: "upsert", RecordID: "plugin-1", Data: rawJSON(t, map[string]interface{}{"id": "plugin-1", "pluginJsonSha256": hashes[2]}), ClientCreatedAt: now},
	}
	if _, err := svc.UploadChanges(42, changes); err != nil {
		t.Fatal(err)
	}

	var records int64
	if err := db.Model(&database.SyncRecord{}).Where("user_id = ?", 42).Count(&records).Error; err != nil {
		t.Fatal(err)
	}
	if records != 3 {
		t.Fatalf("record count = %d, want 3", records)
	}
	var refs []string
	if err := db.Model(&database.SyncRecordBlob{}).Where("user_id = ?", 42).Order("sha256").Pluck("sha256", &refs).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(refs, hashes) {
		t.Fatalf("blob refs = %#v, want %#v", refs, hashes)
	}

	if _, err := svc.UploadChanges(42, []ChangeRecord{{ChangeID: "resource-delete", Table: "resources", Op: "delete", RecordID: "resource-1", ClientCreatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&database.SyncRecord{}).Where("user_id = ? AND table_name = ? AND record_id = ?", 42, "resources", "resource-1").Count(&records).Error; err != nil {
		t.Fatal(err)
	}
	if records != 0 {
		t.Fatalf("deleted projection record count = %d, want 0", records)
	}
	var meta database.SyncMetadata
	if err := db.First(&meta, "user_id = ?", 42).Error; err != nil {
		t.Fatal(err)
	}
	if meta.LastSeq != 4 || meta.IndexRevision != 4 || meta.Generation != 1 {
		t.Fatalf("metadata = %+v", meta)
	}
}

func TestProjectionIdentityMapping(t *testing.T) {
	tests := []struct {
		table, recordID, category, objectID string
		data                                map[string]interface{}
	}{
		{table: "conversations", recordID: "c1", category: "conversations", objectID: "c1"},
		{table: "messages", recordID: "m1", category: "messages", objectID: "c1", data: map[string]interface{}{"conversationId": "c1"}},
		{table: "message_attachments", recordID: "a1", category: "attachments", objectID: "m1", data: map[string]interface{}{"messageId": "m1"}},
		{table: "resources", recordID: "r1", category: "resources", objectID: "r1"},
		{table: "note_folders", recordID: "f1", category: "notes", objectID: "f1"},
		{table: "notes", recordID: "n1", category: "notes", objectID: "n1"},
		{table: "note_pages", recordID: "p1", category: "notes", objectID: "n1", data: map[string]interface{}{"noteId": "n1"}},
		{table: "note_revisions", recordID: "v1", category: "notes", objectID: "n1", data: map[string]interface{}{"noteId": "n1"}},
		{table: "note_page_heads", recordID: "h1", category: "notes", objectID: "h1", data: map[string]interface{}{"pageId": "p1"}},
		{table: "note_page_tombstones", recordID: "x1", category: "notes", objectID: "x1", data: map[string]interface{}{"pageId": "p1"}},
		{table: "tasks", recordID: "t1", category: "tasks", objectID: "t1"},
		{table: "task_lists", recordID: "l1", category: "tasks", objectID: "l1"},
		{table: "task_list_entries", recordID: "t1", category: "tasks", objectID: "l1", data: map[string]interface{}{"listId": "l1"}},
		{table: "calendar_events", recordID: "e1", category: "calendar", objectID: "e1"},
		{table: "anniversaries", recordID: "a1", category: "calendar", objectID: "a1"},
		{table: "roleplay_scenarios", recordID: "s1", category: "roleplay", objectID: "s1"},
		{table: "roleplay_threads", recordID: "t1", category: "roleplay", objectID: "s1", data: map[string]interface{}{"scenarioId": "s1"}},
		{table: "recycle_bin", recordID: "r1", category: "recycle_bin", objectID: "r1"},
		{table: "shared_settings", recordID: "app-settings", category: "settings", objectID: "app-settings"},
		{table: "synced_model_configs", recordID: "m1", category: "models", objectID: "m1"},
		{table: "plugin_files", recordID: "p1/file", category: "plugins", objectID: "p1", data: map[string]interface{}{"pluginId": "p1"}},
		{table: "plugin_settings", recordID: "p1", category: "plugins", objectID: "p1", data: map[string]interface{}{"pluginId": "p1"}},
		{table: "plugin_config", recordID: "p1", category: "plugins", objectID: "p1", data: map[string]interface{}{"pluginId": "p1"}},
	}
	for _, test := range tests {
		raw := rawJSON(t, test.data)
		category, objectID := projectionIdentity(test.table, test.recordID, raw)
		if category != test.category || objectID != test.objectID {
			t.Errorf("%s identity = %s/%s, want %s/%s", test.table, category, objectID, test.category, test.objectID)
		}
	}
}

func TestObjectPurgeIncludesUpsertAndDeleteHistoryWithoutProjection(t *testing.T) {
	db, svc, now := newManagementTestService(t)
	changes := []ChangeRecord{
		{ChangeID: "message-upsert", Table: "messages", Op: "upsert", RecordID: "message-1", Data: rawJSON(t, map[string]interface{}{"id": "message-1", "conversationId": "conversation-1"}), ClientCreatedAt: now},
		{ChangeID: "message-delete", Table: "messages", Op: "delete", RecordID: "message-1", ClientCreatedAt: now},
	}
	if _, err := svc.UploadChanges(42, changes); err != nil {
		t.Fatal(err)
	}
	selector := PurgeSelector{Type: "object", Category: "messages", ObjectID: "conversation-1"}
	preview, err := svc.PreviewPurge(42, selector, 2)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RecordCount != 0 || preview.ChangeCount != 2 {
		t.Fatalf("preview = %+v, want 0 records and 2 changes", preview)
	}
	response, err := svc.PurgeIdempotent(42, "11111111111111111111111111111111", make([]byte, 32), "POST /sync/manage/purge", "device", selector, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	var result PurgeResult
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Operation.DeletedRecordCount != 0 || result.Operation.DeletedChangeCount != 2 {
		t.Fatalf("operation = %+v", result.Operation)
	}
	var remaining int64
	if err := db.Model(&database.SyncChange{}).Where("user_id = ?", 42).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("remaining changes = %d, want 0", remaining)
	}
}

func TestObjectPurgeUsesMembershipAtEachHistoricalChange(t *testing.T) {
	db, svc, now := newManagementTestService(t)
	changes := []ChangeRecord{
		{ChangeID: "message-in-first", Table: "messages", Op: "upsert", RecordID: "message-1", Data: rawJSON(t, map[string]interface{}{"id": "message-1", "conversationId": "conversation-1"}), ClientCreatedAt: now},
		{ChangeID: "message-moved", Table: "messages", Op: "upsert", RecordID: "message-1", Data: rawJSON(t, map[string]interface{}{"id": "message-1", "conversationId": "conversation-2"}), ClientCreatedAt: now},
		{ChangeID: "message-deleted", Table: "messages", Op: "delete", RecordID: "message-1", ClientCreatedAt: now},
	}
	if _, err := svc.UploadChanges(42, changes); err != nil {
		t.Fatal(err)
	}
	first := PurgeSelector{Type: "object", Category: "messages", ObjectID: "conversation-1"}
	preview, err := svc.PreviewPurge(42, first, 3)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RecordCount != 0 || preview.ChangeCount != 1 {
		t.Fatalf("first membership preview = %+v", preview)
	}
	if _, err := svc.PurgeIdempotent(42, "22222222222222222222222222222222", make([]byte, 32), "POST /sync/manage/purge", "device", first, 3, 1); err != nil {
		t.Fatal(err)
	}
	var remaining []database.SyncChange
	if err := db.Where("user_id = ?", 42).Order("seq").Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0].ChangeID != "message-moved" || remaining[1].ChangeID != "message-deleted" {
		t.Fatalf("remaining history = %+v", remaining)
	}
	second := PurgeSelector{Type: "object", Category: "messages", ObjectID: "conversation-2"}
	preview, err = svc.PreviewPurge(42, second, 4)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ChangeCount != 2 {
		t.Fatalf("second membership preview = %+v", preview)
	}
}

func TestCategoryPurgeIncludesHistoryWithoutProjection(t *testing.T) {
	db, svc, now := newManagementTestService(t)
	changes := []ChangeRecord{
		{ChangeID: "message-upsert", Table: "messages", Op: "upsert", RecordID: "message-1", Data: rawJSON(t, map[string]interface{}{"id": "message-1", "conversationId": "conversation-1"}), ClientCreatedAt: now},
		{ChangeID: "message-delete", Table: "messages", Op: "delete", RecordID: "message-1", ClientCreatedAt: now},
		{ChangeID: "task-upsert", Table: "tasks", Op: "upsert", RecordID: "task-1", Data: rawJSON(t, map[string]interface{}{"id": "task-1"}), ClientCreatedAt: now},
	}
	if _, err := svc.UploadChanges(42, changes); err != nil {
		t.Fatal(err)
	}
	selector := PurgeSelector{Type: "category", Category: "messages"}
	preview, err := svc.PreviewPurge(42, selector, 3)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RecordCount != 0 || preview.ChangeCount != 2 {
		t.Fatalf("preview = %+v, want 0 records and 2 changes", preview)
	}
	if _, err := svc.PurgeIdempotent(42, "33333333333333333333333333333333", make([]byte, 32), "POST /sync/manage/purge", "device", selector, 3, 1); err != nil {
		t.Fatal(err)
	}
	var remaining []database.SyncChange
	if err := db.Where("user_id = ?", 42).Order("seq").Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ChangeID != "task-upsert" {
		t.Fatalf("remaining history = %+v", remaining)
	}
}

func newManagementTestService(t *testing.T) (*gorm.DB, *Service, time.Time) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.SyncMetadata{}, &database.SyncChange{}, &database.SyncRecord{}, &database.SyncRecordBlob{}, &database.SyncRequestReplay{}, &database.SyncManagementOperation{}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, nil)
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	return db, svc, now
}

func rawJSON(t *testing.T, value interface{}) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
