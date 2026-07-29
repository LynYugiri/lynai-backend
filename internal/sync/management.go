package sync

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const MaxIndexPageSize = 500

var (
	ErrInvalidSelector       = errors.New("invalid purge selector")
	ErrIndexRevisionConflict = errors.New("index revision conflict")
	ErrOperationNotFound     = errors.New("sync management operation not found")
)

var tableCategories = map[string]string{
	"conversations": "conversations", "messages": "messages", "message_attachments": "attachments", "resources": "resources",
	"note_folders": "notes", "notes": "notes", "note_pages": "notes", "note_revisions": "notes", "note_page_heads": "notes", "note_page_tombstones": "notes",
	"tasks": "tasks", "task_lists": "tasks", "task_list_entries": "tasks",
	"calendar_events": "calendar", "anniversaries": "calendar",
	"roleplay_scenarios": "roleplay", "roleplay_threads": "roleplay", "recycle_bin": "recycle_bin",
	"shared_settings": "settings", "synced_model_configs": "models",
	"plugin_files": "plugins", "plugin_settings": "plugins", "plugin_config": "plugins",
	"knowledge_bases": "knowledge", "knowledge_categories": "knowledge", "knowledge_entries": "knowledge",
	"knowledge_sources": "knowledge", "knowledge_explanations": "knowledge", "knowledge_settings": "knowledge",
}

var objectParentFields = map[string][]string{
	"messages": {"conversationId"}, "message_attachments": {"messageId"},
	"note_pages": {"noteId"}, "note_revisions": {"noteId"},
	"task_list_entries": {"listId", "taskListId"}, "roleplay_threads": {"scenarioId"},
	"plugin_files": {"pluginId"}, "plugin_settings": {"pluginId"}, "plugin_config": {"pluginId"},
	"knowledge_categories": {"knowledgeBaseId"}, "knowledge_entries": {"knowledgeBaseId"},
	"knowledge_sources": {"knowledgeBaseId"}, "knowledge_explanations": {"knowledgeBaseId"},
}

func projectionIdentityDB(db *gorm.DB, userID int64, table, recordID string, raw json.RawMessage) (string, string, error) {
	category, objectID := projectionIdentity(table, recordID, raw)
	if table != "note_page_heads" && table != "note_page_tombstones" {
		return category, objectID, nil
	}
	var data map[string]interface{}
	if json.Unmarshal(raw, &data) != nil {
		return category, objectID, nil
	}
	pageID, _ := data["pageId"].(string)
	if pageID == "" {
		return category, objectID, nil
	}
	var page database.SyncRecord
	err := db.Where("user_id = ? AND table_name = ? AND record_id = ?", userID, "note_pages", pageID).First(&page).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return category, pageID, nil
	}
	if err != nil {
		return "", "", err
	}
	return category, page.ObjectID, nil
}

func relinkNotePageRecords(db *gorm.DB, userID int64, pageID, noteID string) error {
	for _, table := range []string{"note_page_heads", "note_page_tombstones"} {
		if err := db.Model(&database.SyncRecord{}).
			Where("user_id = ? AND table_name = ? AND data->>'pageId' = ?", userID, table, pageID).
			Update("object_id", noteID).Error; err != nil {
			if db.Dialector.Name() != "sqlite" {
				return err
			}
			var records []database.SyncRecord
			if findErr := db.Where("user_id = ? AND table_name = ?", userID, table).Find(&records).Error; findErr != nil {
				return findErr
			}
			for _, record := range records {
				var data map[string]interface{}
				if json.Unmarshal([]byte(record.Data), &data) == nil && data["pageId"] == pageID {
					if updateErr := db.Model(&record).Update("object_id", noteID).Error; updateErr != nil {
						return updateErr
					}
				}
			}
		}
	}
	return nil
}

// PurgeSelector identifies one indexed object, one category, or all sync data.
type PurgeSelector struct {
	Type     string `json:"type"`
	Category string `json:"category,omitempty"`
	ObjectID string `json:"objectId,omitempty"`
}

type IndexObject struct {
	Category     string    `json:"category"`
	ObjectID     string    `json:"objectId"`
	RecordCount  int64     `json:"recordCount"`
	BlobRefCount int64     `json:"blobRefCount"`
	LatestSeq    int64     `json:"latestSeq"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type IndexObjectsPage struct {
	Objects       []IndexObject `json:"objects"`
	IndexRevision int64         `json:"indexRevision"`
	NextAfter     string        `json:"nextAfter,omitempty"`
	HasMore       bool          `json:"hasMore"`
}

type IndexObjectDetail struct {
	IndexObject
	IndexRevision int64          `json:"indexRevision"`
	Records       []ChangeRecord `json:"records"`
}

type PurgePreview struct {
	Selector               PurgeSelector `json:"selector"`
	Generation             int64         `json:"generation"`
	IndexRevision          int64         `json:"indexRevision"`
	RecordCount            int64         `json:"recordCount"`
	ChangeCount            int64         `json:"changeCount"`
	BlobRefCount           int64         `json:"blobRefCount"`
	ReleasedBlobCandidates int64         `json:"releasedBlobCandidates"`
}

type PurgeResult struct {
	Operation database.SyncManagementOperation `json:"operation"`
}

func projectionIdentity(table, recordID string, raw json.RawMessage) (string, string) {
	category := tableCategories[table]
	objectID := recordID
	if fields := objectParentFields[table]; len(fields) > 0 {
		var data map[string]interface{}
		if json.Unmarshal(raw, &data) == nil {
			for _, field := range fields {
				if value, ok := data[field].(string); ok && value != "" {
					objectID = value
					break
				}
			}
		}
	}
	return category, objectID
}

func validateSelector(selector PurgeSelector) error {
	switch selector.Type {
	case "object":
		if !validCategory(selector.Category) || selector.ObjectID == "" || len(selector.ObjectID) > maxRecordIDLength {
			return ErrInvalidSelector
		}
	case "category":
		if !validCategory(selector.Category) || selector.ObjectID != "" {
			return ErrInvalidSelector
		}
	case "all":
		if selector.Category != "" || selector.ObjectID != "" {
			return ErrInvalidSelector
		}
	default:
		return ErrInvalidSelector
	}
	return nil
}

func validCategory(category string) bool {
	for _, value := range tableCategories {
		if value == category {
			return true
		}
	}
	return false
}

func (s *Service) ListIndexObjects(userID int64, category, after string, limit int, expectedRevision int64) (IndexObjectsPage, error) {
	if !validCategory(category) {
		return IndexObjectsPage{}, ErrInvalidSelector
	}
	var page IndexObjectsPage
	err := s.db.Transaction(func(tx *gorm.DB) error {
		meta, err := s.readMetadata(tx, userID)
		if err != nil {
			return err
		}
		if meta.IndexRevision != expectedRevision {
			return ErrIndexRevisionConflict
		}
		var objectIDs []string
		if err := tx.Model(&database.SyncRecord{}).Where("user_id = ? AND category = ? AND object_id > ?", userID, category, after).
			Distinct("object_id").Order("object_id ASC").Limit(limit+1).Pluck("object_id", &objectIDs).Error; err != nil {
			return err
		}
		page.HasMore = len(objectIDs) > limit
		if page.HasMore {
			objectIDs = objectIDs[:limit]
		}
		for _, objectID := range objectIDs {
			var records []database.SyncRecord
			if err := tx.Where("user_id = ? AND category = ? AND object_id = ?", userID, category, objectID).Find(&records).Error; err != nil {
				return err
			}
			object := IndexObject{Category: category, ObjectID: objectID, RecordCount: int64(len(records))}
			for _, record := range records {
				if record.Seq > object.LatestSeq {
					object.LatestSeq = record.Seq
				}
				if record.UpdatedAt.After(object.UpdatedAt) {
					object.UpdatedAt = record.UpdatedAt
				}
			}
			if err := tx.Model(&database.SyncRecordBlob{}).Where("user_id = ? AND (table_name, record_id) IN (?)", userID,
				tx.Model(&database.SyncRecord{}).Select("table_name, record_id").Where("user_id = ? AND category = ? AND object_id = ?", userID, category, objectID)).Distinct("sha256").Count(&object.BlobRefCount).Error; err != nil {
				return err
			}
			page.Objects = append(page.Objects, object)
		}
		page.IndexRevision = meta.IndexRevision
		if len(page.Objects) > 0 {
			page.NextAfter = page.Objects[len(page.Objects)-1].ObjectID
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return page, err
}

func (s *Service) GetIndexObject(userID int64, category, objectID string, expectedRevision int64) (IndexObjectDetail, error) {
	if !validCategory(category) || objectID == "" {
		return IndexObjectDetail{}, ErrInvalidSelector
	}
	var detail IndexObjectDetail
	err := s.db.Transaction(func(tx *gorm.DB) error {
		meta, err := s.readMetadata(tx, userID)
		if err != nil {
			return err
		}
		if meta.IndexRevision != expectedRevision {
			return ErrIndexRevisionConflict
		}
		var records []database.SyncRecord
		if err := tx.Where("user_id = ? AND category = ? AND object_id = ?", userID, category, objectID).Order("table_name, record_id").Find(&records).Error; err != nil {
			return err
		}
		if len(records) == 0 {
			return gorm.ErrRecordNotFound
		}
		detail = IndexObjectDetail{IndexObject: IndexObject{Category: category, ObjectID: objectID}, Records: make([]ChangeRecord, 0, len(records))}
		detail.IndexRevision = meta.IndexRevision
		for _, record := range records {
			detail.RecordCount++
			if record.Seq > detail.LatestSeq {
				detail.LatestSeq = record.Seq
			}
			if record.UpdatedAt.After(detail.UpdatedAt) {
				detail.UpdatedAt = record.UpdatedAt
			}
			detail.Records = append(detail.Records, ChangeRecord{Table: record.TableName, Op: "upsert", RecordID: record.RecordID, Data: json.RawMessage(record.Data)})
		}
		if err := tx.Model(&database.SyncRecordBlob{}).Where("user_id = ? AND (table_name, record_id) IN (?)", userID,
			tx.Model(&database.SyncRecord{}).Select("table_name, record_id").Where("user_id = ? AND category = ? AND object_id = ?", userID, category, objectID)).Distinct("sha256").Count(&detail.BlobRefCount).Error; err != nil {
			return err
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return detail, err
}

func (s *Service) PreviewPurge(userID int64, selector PurgeSelector, expectedRevision int64) (PurgePreview, error) {
	if err := validateSelector(selector); err != nil {
		return PurgePreview{}, err
	}
	var preview PurgePreview
	err := s.db.Transaction(func(tx *gorm.DB) error {
		meta, err := s.readMetadata(tx, userID)
		if err != nil {
			return err
		}
		if meta.IndexRevision != expectedRevision {
			return ErrIndexRevisionConflict
		}
		preview, err = s.previewPurgeDB(tx, userID, selector, meta)
		if err != nil {
			return err
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return preview, err
}

func (s *Service) previewPurgeDB(db *gorm.DB, userID int64, selector PurgeSelector, meta database.SyncMetadata) (PurgePreview, error) {
	preview := PurgePreview{Selector: selector, Generation: metadataGeneration(meta), IndexRevision: meta.IndexRevision}
	records := scopedRecords(db.Model(&database.SyncRecord{}), userID, selector)
	if err := records.Count(&preview.RecordCount).Error; err != nil {
		return preview, err
	}
	settingsReferenced, err := knowledgeSettingsReferencesPurgedBase(db, userID, selector)
	if err != nil {
		return preview, err
	}
	if settingsReferenced {
		preview.RecordCount++
	}
	changeCount, err := countScopedChanges(db, userID, selector)
	if err != nil {
		return preview, err
	}
	preview.ChangeCount = changeCount
	refs := db.Model(&database.SyncRecordBlob{}).Where("user_id = ? AND (table_name, record_id) IN (?)", userID, records.Select("table_name, record_id"))
	if err := refs.Count(&preview.BlobRefCount).Error; err != nil {
		return preview, err
	}
	var hashes []string
	if err := refs.Distinct("sha256").Pluck("sha256", &hashes).Error; err != nil {
		return preview, err
	}
	for _, hash := range hashes {
		var outside int64
		outsideQuery := db.Model(&database.SyncRecordBlob{}).Where("user_id = ? AND sha256 = ?", userID, hash)
		if selector.Type != "all" {
			outsideQuery = outsideQuery.Where("(table_name, record_id) NOT IN (?)", records.Select("table_name, record_id"))
		} else {
			outsideQuery = outsideQuery.Where("1 = 0")
		}
		if err := outsideQuery.Count(&outside).Error; err != nil {
			return preview, err
		}
		if outside == 0 {
			preview.ReleasedBlobCandidates++
		}
	}
	return preview, nil
}

func scopedRecords(db *gorm.DB, userID int64, selector PurgeSelector) *gorm.DB {
	db = db.Where("user_id = ?", userID)
	if selector.Type == "category" {
		return db.Where("category = ?", selector.Category)
	}
	if selector.Type == "object" {
		return db.Where("category = ? AND object_id = ?", selector.Category, selector.ObjectID)
	}
	return db
}

func scopedTableChanges(db *gorm.DB, userID int64, selector PurgeSelector) *gorm.DB {
	query := db.Model(&database.SyncChange{}).Where("user_id = ?", userID)
	if selector.Type == "all" {
		return query
	}
	tables := make([]string, 0)
	for table, category := range tableCategories {
		if category == selector.Category {
			tables = append(tables, table)
		}
	}
	sort.Strings(tables)
	return query.Where("table_name IN ?", tables)
}

func countScopedChanges(db *gorm.DB, userID int64, selector PurgeSelector) (int64, error) {
	if selector.Type != "object" {
		var count int64
		return count, scopedTableChanges(db, userID, selector).Count(&count).Error
	}
	ids, err := scopedObjectChangeIDs(db, userID, selector)
	return int64(len(ids)), err
}

func scopedObjectChangeIDs(db *gorm.DB, userID int64, selector PurgeSelector) ([]int64, error) {
	var changes []database.SyncChange
	if err := scopedTableChanges(db, userID, selector).Order("seq ASC, id ASC").Find(&changes).Error; err != nil {
		return nil, err
	}
	ids := historicalObjectChangeIDs(changes, selector.ObjectID)
	settingsReferenced, err := knowledgeSettingsReferencesPurgedBase(db, userID, selector)
	if err != nil || !settingsReferenced {
		return ids, err
	}
	var settingsIDs []int64
	if err := db.Model(&database.SyncChange{}).
		Where("user_id = ? AND table_name = ?", userID, "knowledge_settings").Pluck("id", &settingsIDs).Error; err != nil {
		return nil, err
	}
	return append(ids, settingsIDs...), nil
}

func knowledgeSettingsReferencesPurgedBase(db *gorm.DB, userID int64, selector PurgeSelector) (bool, error) {
	if selector.Type != "object" || selector.Category != "knowledge" || selector.ObjectID == "global" {
		return false, nil
	}
	var record database.SyncRecord
	err := db.Where("user_id = ? AND table_name = ? AND record_id = ?", userID, "knowledge_settings", "global").First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(record.Data), &data); err != nil {
		return false, err
	}
	return data["defaultKnowledgeBaseId"] == selector.ObjectID, nil
}

type historicalMembership struct {
	seq      int64
	objectID string
}

func historicalObjectChangeIDs(changes []database.SyncChange, objectID string) []int64 {
	pageMemberships := make(map[string][]historicalMembership)
	for _, change := range changes {
		if change.TableName != "note_pages" || change.Op != "upsert" || change.Data == nil {
			continue
		}
		_, noteID := projectionIdentity(change.TableName, change.RecordID, json.RawMessage(*change.Data))
		pageMemberships[change.RecordID] = append(pageMemberships[change.RecordID], historicalMembership{seq: change.Seq, objectID: noteID})
	}

	currentMembership := make(map[string]string)
	ids := make([]int64, 0)
	for _, change := range changes {
		key := change.TableName + "\x00" + change.RecordID
		membership := currentMembership[key]
		if change.Op == "upsert" && change.Data != nil {
			_, membership = projectionIdentity(change.TableName, change.RecordID, json.RawMessage(*change.Data))
			if change.TableName == "note_page_heads" || change.TableName == "note_page_tombstones" {
				if pageID := historicalPageID(*change.Data); pageID != "" {
					membership = historicalPageMembership(pageMemberships[pageID], change.Seq, pageID)
				}
			}
			currentMembership[key] = membership
		} else if membership == "" {
			membership = change.RecordID
		}
		if membership == objectID {
			ids = append(ids, change.ID)
		}
	}
	return ids
}

func historicalPageID(data string) string {
	var value map[string]interface{}
	if json.Unmarshal([]byte(data), &value) != nil {
		return ""
	}
	pageID, _ := value["pageId"].(string)
	return pageID
}

func historicalPageMembership(memberships []historicalMembership, seq int64, fallback string) string {
	for i := len(memberships) - 1; i >= 0; i-- {
		if memberships[i].seq <= seq {
			return memberships[i].objectID
		}
	}
	if len(memberships) > 0 {
		return memberships[0].objectID
	}
	return fallback
}

func deleteObjectChanges(db *gorm.DB, userID int64, ids []int64) error {
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		if err := db.Where("user_id = ? AND id IN ?", userID, ids[start:end]).Delete(&database.SyncChange{}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) PurgeIdempotent(userID int64, requestID string, bodyHash []byte, operation, deviceID string, selector PurgeSelector, expectedRevision, expectedGeneration int64) (ReplayResponse, error) {
	if err := validateSelector(selector); err != nil {
		return ReplayResponse{}, err
	}
	if replay, found, err := s.lookupReplay(userID, requestID, bodyHash, operation, expectedGeneration); err != nil || found {
		return replay, err
	}
	var response ReplayResponse
	err := s.withUserLock(userID, func(tx *gorm.DB) error {
		if replay, found, err := s.lookupReplayDB(tx, userID, requestID, bodyHash, operation, expectedGeneration); err != nil || found {
			response = replay
			return err
		}
		meta, err := s.lockMetadata(tx, userID)
		if err != nil {
			return err
		}
		if metadataGeneration(meta) != expectedGeneration {
			return generationConflictError{Expected: expectedGeneration, Current: metadataGeneration(meta)}
		}
		if meta.IndexRevision != expectedRevision {
			return ErrIndexRevisionConflict
		}
		preview, err := s.previewPurgeDB(tx, userID, selector, meta)
		if err != nil {
			return err
		}
		meta.Generation = metadataGeneration(meta) + 1
		if selector.Type == "all" {
			for _, model := range []interface{}{&database.SyncRecordBlob{}, &database.SyncRecord{}, &database.SyncChange{}} {
				if err := tx.Where("user_id = ?", userID).Delete(model).Error; err != nil {
					return err
				}
			}
			meta.LastSeq, meta.MinAvailableSeq = 0, 0
		} else {
			records := scopedRecords(tx.Model(&database.SyncRecord{}), userID, selector)
			settingsReferenced, err := knowledgeSettingsReferencesPurgedBase(tx, userID, selector)
			if err != nil {
				return err
			}
			if selector.Type == "object" {
				changeIDs, err := scopedObjectChangeIDs(tx, userID, selector)
				if err != nil {
					return err
				}
				if err := deleteObjectChanges(tx, userID, changeIDs); err != nil {
					return err
				}
			} else if err := scopedTableChanges(tx, userID, selector).Delete(&database.SyncChange{}).Error; err != nil {
				return err
			}
			if err := tx.Where("user_id = ? AND (table_name, record_id) IN (?)", userID, records.Select("table_name, record_id")).Delete(&database.SyncRecordBlob{}).Error; err != nil {
				return err
			}
			if err := records.Delete(&database.SyncRecord{}).Error; err != nil {
				return err
			}
			if settingsReferenced {
				key := "user_id = ? AND table_name = ? AND record_id = ?"
				if err := tx.Where(key, userID, "knowledge_settings", "global").Delete(&database.SyncRecordBlob{}).Error; err != nil {
					return err
				}
				if err := tx.Where(key, userID, "knowledge_settings", "global").Delete(&database.SyncRecord{}).Error; err != nil {
					return err
				}
			}
			meta.MinAvailableSeq = meta.LastSeq
		}
		meta.IndexRevision++
		meta.UpdatedAt = s.now()
		if err := tx.Save(&meta).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND status = ?", userID, "pending").Delete(&database.SyncManagementOperation{}).Error; err != nil {
			return err
		}
		category, objectID := nullableSelector(selector)
		op := database.SyncManagementOperation{ID: requestID, UserID: userID, Kind: "selective", SelectorType: selector.Type, Category: category, ObjectID: objectID,
			Generation: meta.Generation, IndexRevision: meta.IndexRevision, ReleasedBlobCandidates: preview.ReleasedBlobCandidates,
			DeletedRecordCount: preview.RecordCount, DeletedChangeCount: preview.ChangeCount, Status: "pending", CreatedByDeviceID: deviceID, CreatedAt: s.now()}
		if selector.Type == "all" {
			op.Kind = "full"
		}
		if err := tx.Create(&op).Error; err != nil {
			return err
		}
		body, err := json.Marshal(PurgeResult{Operation: op})
		if err != nil {
			return err
		}
		response = ReplayResponse{Status: 200, ContentType: "application/json; charset=utf-8", Body: body}
		return s.storeReplay(tx, userID, requestID, bodyHash, operation, expectedGeneration, response)
	})
	if err == nil {
		return response, nil
	}
	if isUniqueViolation(err) {
		if replay, found, lookupErr := s.lookupReplay(userID, requestID, bodyHash, operation, expectedGeneration); lookupErr != nil || found {
			return replay, lookupErr
		}
	}
	return ReplayResponse{}, err
}

func (s *Service) PendingOperations(userID int64) ([]database.SyncManagementOperation, error) {
	var operations []database.SyncManagementOperation
	err := s.db.Where("user_id = ? AND status = ?", userID, "pending").Order("created_at, id").Find(&operations).Error
	return operations, err
}

func (s *Service) AckOperationIdempotent(userID int64, requestID string, bodyHash []byte, operation, deviceID, operationID string, expectedGeneration int64) (ReplayResponse, error) {
	if replay, found, err := s.lookupReplay(userID, requestID, bodyHash, operation, expectedGeneration); err != nil || found {
		return replay, err
	}
	var response ReplayResponse
	err := s.withUserLock(userID, func(tx *gorm.DB) error {
		if replay, found, err := s.lookupReplayDB(tx, userID, requestID, bodyHash, operation, expectedGeneration); err != nil || found {
			response = replay
			return err
		}
		meta, err := s.lockMetadata(tx, userID)
		if err != nil {
			return err
		}
		if metadataGeneration(meta) != expectedGeneration {
			return generationConflictError{Expected: expectedGeneration, Current: metadataGeneration(meta)}
		}
		var op database.SyncManagementOperation
		if err := tx.Where("id = ? AND user_id = ?", operationID, userID).First(&op).Error; err != nil {
			return ErrOperationNotFound
		}
		if op.Generation != expectedGeneration {
			return generationConflictError{Expected: expectedGeneration, Current: op.Generation}
		}
		if op.Status == "pending" {
			now := s.now()
			op.Status, op.AckedByDeviceID, op.AckedAt = "acked", &deviceID, &now
			if err := tx.Save(&op).Error; err != nil {
				return err
			}
		}
		body, err := json.Marshal(op)
		if err != nil {
			return err
		}
		response = ReplayResponse{Status: 200, ContentType: "application/json; charset=utf-8", Body: body}
		return s.storeReplay(tx, userID, requestID, bodyHash, operation, expectedGeneration, response)
	})
	if err == nil {
		return response, nil
	}
	if isUniqueViolation(err) {
		if replay, found, lookupErr := s.lookupReplay(userID, requestID, bodyHash, operation, expectedGeneration); lookupErr != nil || found {
			return replay, lookupErr
		}
	}
	return ReplayResponse{}, err
}

func (s *Service) readMetadata(db *gorm.DB, userID int64) (database.SyncMetadata, error) {
	var meta database.SyncMetadata
	err := db.First(&meta, "user_id = ?", userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return database.SyncMetadata{UserID: userID, Generation: 1}, nil
	}
	return meta, err
}

func (s *Service) lockMetadata(tx *gorm.DB, userID int64) (database.SyncMetadata, error) {
	now := s.now()
	tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&database.SyncMetadata{UserID: userID, Generation: 1, UpdatedAt: now})
	var meta database.SyncMetadata
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&meta, "user_id = ?", userID).Error
	return meta, err
}

func (s *Service) storeReplay(tx *gorm.DB, userID int64, requestID string, bodyHash []byte, operation string, generation int64, response ReplayResponse) error {
	now := s.now()
	return tx.Create(&database.SyncRequestReplay{UserID: userID, RequestID: requestID, Generation: generation, Operation: operation, BodyHash: bytes.Clone(bodyHash), ResponseStatus: response.Status,
		ResponseContentType: response.ContentType, ResponseBody: response.Body, CreatedAt: now, ExpiresAt: now.Add(s.replayRetention)}).Error
}

func (s *Service) withUserLock(userID int64, fn func(*gorm.DB) error) error {
	if s.db.Dialector.Name() != "postgres" {
		s.managementMu.Lock()
		defer s.managementMu.Unlock()
		return s.db.Transaction(fn)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", userLockID(userID)).Error; err != nil {
			return fmt.Errorf("acquire sync user lock: %w", err)
		}
		return fn(tx)
	})
}

func userLockID(userID int64) int64 {
	hash := fnv.New64a()
	_, _ = fmt.Fprintf(hash, "sync-management/%d", userID)
	return int64(hash.Sum64())
}

func nullableSelector(selector PurgeSelector) (*string, *string) {
	var category, objectID *string
	if selector.Category != "" {
		value := selector.Category
		category = &value
	}
	if selector.ObjectID != "" {
		value := selector.ObjectID
		objectID = &value
	}
	return category, objectID
}

func encodeIndexCursor(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}
func decodeIndexCursor(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) == 0 || len(raw) > maxRecordIDLength || !utf8.Valid(raw) ||
		base64.RawURLEncoding.EncodeToString(raw) != value || strings.ContainsRune(string(raw), 0) {
		return "", errors.New("invalid cursor")
	}
	return string(raw), nil
}
