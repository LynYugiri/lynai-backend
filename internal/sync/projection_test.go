package sync

import (
	"encoding/json"
	"errors"
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
		{table: "knowledge_bases", recordID: "kb1", category: "knowledge", objectID: "kb1"},
		{table: "knowledge_categories", recordID: "category1", category: "knowledge", objectID: "kb1", data: map[string]interface{}{"knowledgeBaseId": "kb1"}},
		{table: "knowledge_entries", recordID: "entry1", category: "knowledge", objectID: "kb1", data: map[string]interface{}{"knowledgeBaseId": "kb1"}},
		{table: "knowledge_sources", recordID: "source1", category: "knowledge", objectID: "kb1", data: map[string]interface{}{"knowledgeBaseId": "kb1"}},
		{table: "knowledge_explanations", recordID: "explanation1", category: "knowledge", objectID: "kb1", data: map[string]interface{}{"knowledgeBaseId": "kb1"}},
		{table: "knowledge_settings", recordID: "global", category: "knowledge", objectID: "global"},
	}
	for _, test := range tests {
		raw := rawJSON(t, test.data)
		category, objectID := projectionIdentity(test.table, test.recordID, raw)
		if category != test.category || objectID != test.objectID {
			t.Errorf("%s identity = %s/%s, want %s/%s", test.table, category, objectID, test.category, test.objectID)
		}
	}
}

func TestValidateKnowledgeChanges(t *testing.T) {
	now := time.Now().UTC()
	for _, table := range []string{
		"knowledge_bases",
		"knowledge_categories",
		"knowledge_entries",
		"knowledge_sources",
		"knowledge_explanations",
		"knowledge_settings",
	} {
		data := validKnowledgeData(table, "record-1", now)
		if table == "knowledge_settings" {
			data["id"] = "global"
		}
		recordID := data["id"].(string)
		change := ChangeRecord{ChangeID: table, Table: table, Op: "upsert", RecordID: recordID, Data: rawJSON(t, data), ClientCreatedAt: now}
		if err := validateChange(change); err != nil {
			t.Errorf("validateChange(%s) error = %v", table, err)
		}
	}

	for _, knowledgeBaseID := range []interface{}{nil, "", 1, strings.Repeat("x", maxRecordIDLength+1)} {
		data := validKnowledgeData("knowledge_entries", "entry-1", now)
		if knowledgeBaseID != nil {
			data["knowledgeBaseId"] = knowledgeBaseID
		} else {
			delete(data, "knowledgeBaseId")
		}
		change := ChangeRecord{ChangeID: "invalid-entry", Table: "knowledge_entries", Op: "upsert", RecordID: "entry-1", Data: rawJSON(t, data), ClientCreatedAt: now}
		if err := validateChange(change); err == nil {
			t.Errorf("validateChange accepted knowledgeBaseId %#v", knowledgeBaseID)
		}
	}

	reservedBase := validKnowledgeData("knowledge_bases", "global", now)
	if err := validateChange(ChangeRecord{
		ChangeID:        "reserved-global-base",
		Table:           "knowledge_bases",
		Op:              "upsert",
		RecordID:        "global",
		Data:            rawJSON(t, reservedBase),
		ClientCreatedAt: now,
	}); err == nil {
		t.Fatal("validateChange accepted reserved knowledge base id global")
	}

	validSettings := map[string]interface{}{
		"id": "global", "defaultKnowledgeBaseId": "base-1", "defaultCategoryId": "category-1", "updatedAt": now.Format(time.RFC3339Nano),
	}
	for name, mutate := range map[string]func(map[string]interface{}){
		"wrong record ID":   func(data map[string]interface{}) { data["id"] = "other" },
		"missing paired ID": func(data map[string]interface{}) { delete(data, "defaultCategoryId") },
		"invalid timestamp": func(data map[string]interface{}) { data["updatedAt"] = "today" },
	} {
		data := make(map[string]interface{}, len(validSettings))
		for key, value := range validSettings {
			data[key] = value
		}
		mutate(data)
		change := ChangeRecord{ChangeID: "invalid-settings-" + name, Table: "knowledge_settings", Op: "upsert", RecordID: "global", Data: rawJSON(t, data), ClientCreatedAt: now}
		if err := validateChange(change); err == nil {
			t.Errorf("validateChange accepted knowledge settings with %s", name)
		}
	}

	invalidFields := []struct {
		table string
		field string
		value interface{}
	}{
		{table: "knowledge_bases", field: "name", value: ""},
		{table: "knowledge_bases", field: "enabled", value: 1},
		{table: "knowledge_bases", field: "sortOrder", value: 1.5},
		{table: "knowledge_bases", field: "createdAt", value: "today"},
		{table: "knowledge_categories", field: "alias", value: "Bad Alias"},
		{table: "knowledge_categories", field: "autoAnnotate", value: "true"},
		{table: "knowledge_categories", field: "colorValue", value: true},
		{table: "knowledge_entries", field: "categoryId", value: 1},
		{table: "knowledge_entries", field: "title", value: nil},
		{table: "knowledge_sources", field: "entryId", value: ""},
		{table: "knowledge_sources", field: "url", value: 1},
		{table: "knowledge_explanations", field: "entryId", value: nil},
		{table: "knowledge_explanations", field: "updatedAt", value: false},
	}
	for _, test := range invalidFields {
		data := validKnowledgeData(test.table, "record-1", now)
		data[test.field] = test.value
		change := ChangeRecord{ChangeID: test.table + "-" + test.field, Table: test.table, Op: "upsert", RecordID: "record-1", Data: rawJSON(t, data), ClientCreatedAt: now}
		if err := validateChange(change); err == nil {
			t.Errorf("validateChange accepted %s.%s = %#v", test.table, test.field, test.value)
		}
	}
}

func TestKnowledgeBatchValidatesFinalProjectionRegardlessOfOrder(t *testing.T) {
	_, svc, now := newManagementTestService(t)
	changes := []ChangeRecord{
		{ChangeID: "source", Table: "knowledge_sources", Op: "upsert", RecordID: "source-1", Data: rawJSON(t, validKnowledgeData("knowledge_sources", "source-1", now)), ClientCreatedAt: now},
		{ChangeID: "entry", Table: "knowledge_entries", Op: "upsert", RecordID: "entry-1", Data: rawJSON(t, validKnowledgeData("knowledge_entries", "entry-1", now)), ClientCreatedAt: now},
		{ChangeID: "settings", Table: "knowledge_settings", Op: "upsert", RecordID: "global", Data: rawJSON(t, map[string]interface{}{"id": "global", "defaultKnowledgeBaseId": "base-1", "defaultCategoryId": "category-1", "updatedAt": now.Format(time.RFC3339Nano)}), ClientCreatedAt: now},
		{ChangeID: "category", Table: "knowledge_categories", Op: "upsert", RecordID: "category-1", Data: rawJSON(t, validKnowledgeData("knowledge_categories", "category-1", now)), ClientCreatedAt: now},
		{ChangeID: "base", Table: "knowledge_bases", Op: "upsert", RecordID: "base-1", Data: rawJSON(t, validKnowledgeData("knowledge_bases", "base-1", now)), ClientCreatedAt: now},
	}
	if _, err := svc.UploadChanges(42, changes); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeBatchRejectsUnapplicableFinalProjection(t *testing.T) {
	_, svc, now := newManagementTestService(t)
	tests := []struct {
		name    string
		changes []ChangeRecord
	}{
		{name: "missing base", changes: []ChangeRecord{{ChangeID: "category", Table: "knowledge_categories", Op: "upsert", RecordID: "category-1", Data: rawJSON(t, validKnowledgeData("knowledge_categories", "category-1", now)), ClientCreatedAt: now}}},
		{name: "entry category in another base", changes: []ChangeRecord{
			{ChangeID: "base-1", Table: "knowledge_bases", Op: "upsert", RecordID: "base-1", Data: rawJSON(t, validKnowledgeData("knowledge_bases", "base-1", now)), ClientCreatedAt: now},
			{ChangeID: "base-2", Table: "knowledge_bases", Op: "upsert", RecordID: "base-2", Data: rawJSON(t, func() map[string]interface{} {
				data := validKnowledgeData("knowledge_bases", "base-2", now)
				return data
			}()), ClientCreatedAt: now},
			{ChangeID: "category-2", Table: "knowledge_categories", Op: "upsert", RecordID: "category-2", Data: rawJSON(t, func() map[string]interface{} {
				data := validKnowledgeData("knowledge_categories", "category-2", now)
				data["knowledgeBaseId"] = "base-2"
				data["alias"] = "other"
				return data
			}()), ClientCreatedAt: now},
			{ChangeID: "entry-1", Table: "knowledge_entries", Op: "upsert", RecordID: "entry-1", Data: rawJSON(t, func() map[string]interface{} {
				data := validKnowledgeData("knowledge_entries", "entry-1", now)
				data["categoryId"] = "category-2"
				return data
			}()), ClientCreatedAt: now},
		}},
		{name: "source entry in another base", changes: []ChangeRecord{
			{ChangeID: "base", Table: "knowledge_bases", Op: "upsert", RecordID: "base-1", Data: rawJSON(t, validKnowledgeData("knowledge_bases", "base-1", now)), ClientCreatedAt: now},
			{ChangeID: "entry", Table: "knowledge_entries", Op: "upsert", RecordID: "entry-1", Data: rawJSON(t, validKnowledgeData("knowledge_entries", "entry-1", now)), ClientCreatedAt: now},
			{ChangeID: "source", Table: "knowledge_sources", Op: "upsert", RecordID: "source-1", Data: rawJSON(t, func() map[string]interface{} {
				data := validKnowledgeData("knowledge_sources", "source-1", now)
				data["knowledgeBaseId"] = "base-2"
				return data
			}()), ClientCreatedAt: now},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := svc.UploadChanges(42, test.changes); !errors.Is(err, ErrInvalidChange) {
				t.Fatalf("UploadChanges error = %v, want invalid change", err)
			}
		})
	}
}

func TestKnowledgeObjectPurgeRemovesSettingsReferencingPurgedBase(t *testing.T) {
	db, svc, now := newManagementTestService(t)
	changes := []ChangeRecord{
		{ChangeID: "base", Table: "knowledge_bases", Op: "upsert", RecordID: "base-1", Data: rawJSON(t, validKnowledgeData("knowledge_bases", "base-1", now)), ClientCreatedAt: now},
		{ChangeID: "category", Table: "knowledge_categories", Op: "upsert", RecordID: "category-1", Data: rawJSON(t, validKnowledgeData("knowledge_categories", "category-1", now)), ClientCreatedAt: now},
		{ChangeID: "settings", Table: "knowledge_settings", Op: "upsert", RecordID: "global", Data: rawJSON(t, map[string]interface{}{"id": "global", "defaultKnowledgeBaseId": "base-1", "defaultCategoryId": "category-1", "updatedAt": now.Format(time.RFC3339Nano)}), ClientCreatedAt: now},
		{ChangeID: "other-base", Table: "knowledge_bases", Op: "upsert", RecordID: "base-2", Data: rawJSON(t, validKnowledgeData("knowledge_bases", "base-2", now)), ClientCreatedAt: now},
	}
	if _, err := svc.UploadChanges(42, changes); err != nil {
		t.Fatal(err)
	}
	selector := PurgeSelector{Type: "object", Category: "knowledge", ObjectID: "base-1"}
	preview, err := svc.PreviewPurge(42, selector, 4)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RecordCount != 3 || preview.ChangeCount != 3 {
		t.Fatalf("preview = %+v, want 3 records and 3 changes", preview)
	}
	if _, err := svc.PurgeIdempotent(42, "44444444444444444444444444444444", make([]byte, 32), "POST /sync/manage/purge", "device", selector, 4, 1); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&database.SyncRecord{}).Where("user_id = ? AND category = ?", 42, "knowledge").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("remaining knowledge projections = %d, want only the unrelated base", count)
	}
	var remaining database.SyncRecord
	if err := db.Where("user_id = ? AND category = ?", 42, "knowledge").First(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining.TableName != "knowledge_bases" || remaining.RecordID != "base-2" {
		t.Fatalf("remaining knowledge projection = %+v, want base-2", remaining)
	}
	if err := db.Model(&database.SyncChange{}).Where("user_id = ? AND table_name = ?", 42, "knowledge_settings").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("remaining knowledge settings history = %d, want 0", count)
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

func validKnowledgeData(table, id string, now time.Time) map[string]interface{} {
	timestamp := now.Format(time.RFC3339Nano)
	common := map[string]interface{}{"id": id, "sortOrder": 0, "createdAt": timestamp, "updatedAt": timestamp}
	switch table {
	case "knowledge_bases":
		common["name"] = "Base"
		common["enabled"] = true
	case "knowledge_categories":
		common["knowledgeBaseId"] = "base-1"
		common["name"] = "Category"
		common["alias"] = "category"
		common["annotationRule"] = ""
		common["explanationPrompt"] = ""
		common["colorValue"] = 0
		common["autoAnnotate"] = true
		common["isDefault"] = true
		common["enabled"] = true
	case "knowledge_entries":
		common["knowledgeBaseId"] = "base-1"
		common["categoryId"] = "category-1"
		common["title"] = "Entry"
		common["content"] = ""
		common["enabled"] = true
	case "knowledge_sources":
		common["knowledgeBaseId"] = "base-1"
		common["entryId"] = "entry-1"
		common["title"] = "Source"
	case "knowledge_explanations":
		common["knowledgeBaseId"] = "base-1"
		common["entryId"] = "entry-1"
		common["title"] = "Explanation"
		common["content"] = "Content"
	case "knowledge_settings":
		return map[string]interface{}{"id": id, "updatedAt": timestamp}
	}
	return common
}
