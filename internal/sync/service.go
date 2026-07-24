package sync

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	stdsync "sync"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	maxRecordIDLength = 256
	maxChangeIDLength = 128
)

var (
	ErrInvalidChange      = errors.New("invalid change")
	ErrSyncLimit          = errors.New("sync limit exceeded")
	ErrChangeConflict     = errors.New("change ID conflicts with an existing change")
	ErrReplayConflict     = errors.New("request ID conflicts with an existing request")
	ErrBlobNotFound       = errors.New("blob not found")
	ErrGenerationConflict = errors.New("sync generation conflict")
	ErrFutureCursor       = errors.New("sync cursor is ahead of the current generation")
	allowedTables         = map[string]struct{}{
		"resources": {}, "conversations": {}, "messages": {}, "message_attachments": {},
		"calendar_events": {}, "anniversaries": {}, "tasks": {}, "task_lists": {}, "task_list_entries": {},
		"roleplay_scenarios": {}, "roleplay_threads": {}, "recycle_bin": {},
		"note_folders": {}, "notes": {}, "note_pages": {}, "note_revisions": {},
		"note_page_heads": {}, "note_page_tombstones": {},
		"shared_settings": {}, "synced_model_configs": {},
		"plugin_files": {}, "plugin_settings": {}, "plugin_config": {},
	}
	sha256ValuePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type generationConflictError struct {
	Expected int64
	Current  int64
}

func (e generationConflictError) Error() string { return ErrGenerationConflict.Error() }
func (e generationConflictError) Unwrap() error { return ErrGenerationConflict }

// Service handles incremental sync operations.
type Service struct {
	db              *gorm.DB
	blob            *BlobStorage
	replayRetention time.Duration
	now             func() time.Time
	blobMu          stdsync.Mutex
	managementMu    stdsync.Mutex
}

// NewService creates a sync service.
func NewService(db *gorm.DB, blob *BlobStorage) *Service {
	return NewServiceWithReplayRetention(db, blob, 24*time.Hour)

}

// NewServiceWithReplayRetention creates a sync service with durable replay expiry.
func NewServiceWithReplayRetention(db *gorm.DB, blob *BlobStorage, retention time.Duration) *Service {
	return &Service{db: db, blob: blob, replayRetention: retention, now: time.Now}
}

// SyncStatus is the response for GET /sync/status.
type SyncStatus struct {
	LastSeq         int64     `json:"lastSeq"`
	Generation      int64     `json:"generation"`
	IndexRevision   int64     `json:"indexRevision"`
	MinAvailableSeq int64     `json:"minAvailableSeq"`
	BlobCount       int64     `json:"blobCount"`
	Usage           SyncUsage `json:"usage"`
	Limits          Limits    `json:"limits"`
}

// SyncUsage contains basic per-user sync storage counters.
type SyncUsage struct {
	RecordCount  int64 `json:"recordCount"`
	BlobCount    int64 `json:"blobCount"`
	BlobBytes    int64 `json:"blobBytes"`
	BlobRefCount int64 `json:"blobRefCount"`
}

// GetStatus returns the user's current sync sequence and blob count.
func (s *Service) GetStatus(userID int64) (*SyncStatus, error) {
	var meta database.SyncMetadata
	err := s.db.First(&meta, "user_id = ?", userID).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}

	var usage SyncUsage
	if err := s.db.Model(&database.SyncRecord{}).Where("user_id = ?", userID).Count(&usage.RecordCount).Error; err != nil {
		return nil, err
	}
	if err := s.db.Model(&database.SyncBlob{}).Where("user_id = ?", userID).
		Select("COUNT(*) AS blob_count, COALESCE(SUM(size), 0) AS blob_bytes").Scan(&usage).Error; err != nil {
		return nil, err
	}
	if err := s.db.Model(&database.SyncRecordBlob{}).Where("user_id = ?", userID).Count(&usage.BlobRefCount).Error; err != nil {
		return nil, err
	}

	return &SyncStatus{
		LastSeq: meta.LastSeq, Generation: metadataGeneration(meta), IndexRevision: meta.IndexRevision,
		MinAvailableSeq: meta.MinAvailableSeq, BlobCount: usage.BlobCount, Usage: usage, Limits: syncLimits,
	}, nil
}

// ChangeRecord is a single change in the sync log, as transmitted over the API.
type ChangeRecord struct {
	ChangeID        string          `json:"changeId,omitempty"`
	Table           string          `json:"table"`
	Op              string          `json:"op"`
	RecordID        string          `json:"recordId"`
	Data            json.RawMessage `json:"data,omitempty"`
	ClientCreatedAt time.Time       `json:"clientCreatedAt,omitempty"`
}

// ChangeWithSeq is a change record with its assigned sequence number.
type ChangeWithSeq struct {
	Seq             int64           `json:"seq"`
	ChangeID        string          `json:"changeId"`
	DeviceID        *string         `json:"deviceId,omitempty"`
	Table           string          `json:"table"`
	Op              string          `json:"op"`
	RecordID        string          `json:"recordId"`
	Data            json.RawMessage `json:"data,omitempty"`
	ClientCreatedAt time.Time       `json:"clientCreatedAt"`
	CreatedAt       time.Time       `json:"createdAt"`
}

// UploadResult is the committed response body for a change upload.
type UploadResult struct {
	RequestID string          `json:"requestId,omitempty"`
	Changes   []ChangeWithSeq `json:"changes"`
	LatestSeq int64           `json:"latestSeq"`
}

// ReplayResponse is an exact stored HTTP response.
type ReplayResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

// UploadChanges accepts a batch of change records, assigns sequence numbers,
// persists them, and returns the assigned range.
func (s *Service) UploadChanges(userID int64, changes []ChangeRecord) ([]ChangeWithSeq, error) {
	now := s.now()
	for i := range changes {
		if changes[i].ChangeID == "" {
			changes[i].ChangeID = legacyChangeID()
		}
		if changes[i].ClientCreatedAt.IsZero() {
			changes[i].ClientCreatedAt = now
		}
	}
	result, _, err := s.commitChanges(userID, "", nil, "", changes, now, "", 0)
	return result.Changes, err
}

// UploadChangesIdempotent commits changes and the exact replay response atomically.
func (s *Service) UploadChangesIdempotent(userID int64, requestID string, bodyHash []byte, operation, deviceID string, expectedGeneration int64, changes []ChangeRecord) (ReplayResponse, error) {
	now := s.now()
	for i := range changes {
		changes[i].ClientCreatedAt = changes[i].ClientCreatedAt.UTC().Truncate(time.Microsecond)
	}
	if replay, found, err := s.lookupReplay(userID, requestID, bodyHash, operation, expectedGeneration); err != nil || found {
		return replay, err
	}
	result, response, err := s.commitChanges(userID, requestID, bodyHash, operation, changes, now, deviceID, expectedGeneration)
	_ = result
	if err == nil {
		return response, nil
	}
	if !isUniqueViolation(err) {
		return ReplayResponse{}, err
	}
	if replay, found, lookupErr := s.lookupReplay(userID, requestID, bodyHash, operation, expectedGeneration); lookupErr != nil || found {
		return replay, lookupErr
	}
	return ReplayResponse{}, err
}

func (s *Service) commitChanges(userID int64, requestID string, bodyHash []byte, operation string, changes []ChangeRecord, now time.Time, deviceIDValue string, expectedGeneration int64) (UploadResult, ReplayResponse, error) {
	if len(changes) > MaxChangeBatch {
		return UploadResult{}, ReplayResponse{}, fmt.Errorf("%w: too many changes", ErrSyncLimit)
	}
	seen := make(map[string]struct{}, len(changes))
	for i, change := range changes {
		if err := validateChange(change); err != nil {
			if errors.Is(err, ErrSyncLimit) {
				return UploadResult{}, ReplayResponse{}, fmt.Errorf("%w at index %d: %v", ErrSyncLimit, i, err)
			}
			return UploadResult{}, ReplayResponse{}, fmt.Errorf("%w at index %d: %v", ErrInvalidChange, i, err)
		}
		if _, ok := seen[change.ChangeID]; ok {
			return UploadResult{}, ReplayResponse{}, fmt.Errorf("%w at index %d: duplicate changeId", ErrInvalidChange, i)
		}
		seen[change.ChangeID] = struct{}{}
	}

	result := make([]ChangeWithSeq, 0, len(changes))
	var response ReplayResponse
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var meta database.SyncMetadata
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&meta, "user_id = ?", userID).Error
		if err == gorm.ErrRecordNotFound {
			created := database.SyncMetadata{UserID: userID, LastSeq: 0, Generation: 1, UpdatedAt: now}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&created).Error; err != nil {
				return fmt.Errorf("create sync metadata: %w", err)
			}
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&meta, "user_id = ?", userID).Error; err != nil {
				return fmt.Errorf("lock sync metadata: %w", err)
			}
		} else if err != nil {
			return err
		}
		if expectedGeneration > 0 && metadataGeneration(meta) != expectedGeneration {
			return generationConflictError{Expected: expectedGeneration, Current: metadataGeneration(meta)}
		}

		seq := meta.LastSeq
		var deviceID *string
		if deviceIDValue != "" {
			deviceID = &deviceIDValue
		}
		for _, ch := range changes {
			var existing database.SyncChange
			err := tx.Where("user_id = ? AND change_id = ?", userID, ch.ChangeID).First(&existing).Error
			if err == nil {
				if !sameChange(existing, ch) {
					return ErrChangeConflict
				}
				result = append(result, changeFromModel(existing))
				continue
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			seq++
			var dataPtr *string
			if len(ch.Data) > 0 {
				s := string(ch.Data)
				dataPtr = &s
			}
			change := database.SyncChange{
				UserID: userID, Seq: seq, ChangeID: ch.ChangeID, DeviceID: deviceID,
				TableName: ch.Table, Op: ch.Op, RecordID: ch.RecordID, Data: dataPtr,
				ClientCreatedAt: ch.ClientCreatedAt, CreatedAt: now,
			}
			if err := tx.Create(&change).Error; err != nil {
				return fmt.Errorf("create sync change: %w", err)
			}
			if err := updateProjection(tx, userID, ch, seq, now); err != nil {
				return err
			}
			result = append(result, changeFromModel(change))
		}
		nextIndexRevision := meta.IndexRevision
		if seq != meta.LastSeq {
			nextIndexRevision++
			if nextIndexRevision < seq {
				nextIndexRevision = seq
			}
		}
		// Update metadata.
		if err := tx.Model(&database.SyncMetadata{}).
			Where("user_id = ?", userID).
			Updates(map[string]interface{}{
				"last_seq": seq, "index_revision": nextIndexRevision, "updated_at": now,
			}).Error; err != nil {
			return fmt.Errorf("update sync metadata: %w", err)
		}
		upload := UploadResult{RequestID: requestID, Changes: result, LatestSeq: seq}
		body, err := json.Marshal(upload)
		if err != nil {
			return err
		}
		response = ReplayResponse{Status: 200, ContentType: "application/json; charset=utf-8", Body: body}
		if requestID != "" {
			replay := database.SyncRequestReplay{UserID: userID, RequestID: requestID, Generation: metadataGeneration(meta), Operation: operation, BodyHash: bodyHash,
				ResponseStatus: response.Status, ResponseContentType: response.ContentType, ResponseBody: body,
				CreatedAt: now, ExpiresAt: now.Add(s.replayRetention)}
			if err := tx.Create(&replay).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return UploadResult{}, ReplayResponse{}, err
	}
	return UploadResult{RequestID: requestID, Changes: result}, response, nil
}

func validateChange(change ChangeRecord) error {
	if len(change.ChangeID) == 0 || len(change.ChangeID) > maxChangeIDLength || strings.TrimSpace(change.ChangeID) != change.ChangeID {
		return errors.New("changeId must contain 1 to 128 non-whitespace-trimmed bytes")
	}
	if change.ClientCreatedAt.IsZero() {
		return errors.New("clientCreatedAt is required")
	}
	if _, ok := allowedTables[change.Table]; !ok {
		return errors.New("unsupported table")
	}
	if change.Op != "upsert" && change.Op != "delete" {
		return errors.New("unsupported operation")
	}
	if len(change.RecordID) == 0 || len(change.RecordID) > maxRecordIDLength {
		return errors.New("recordId must contain 1 to 256 bytes")
	}
	if change.Table == "shared_settings" && change.RecordID != "app-settings" {
		return errors.New("shared_settings recordId must be app-settings")
	}
	if len(change.Data) > MaxChangeDataBytes {
		return fmt.Errorf("%w: change data is too large", ErrSyncLimit)
	}
	if change.Op == "upsert" {
		var data map[string]interface{}
		decoder := json.NewDecoder(bytes.NewReader(change.Data))
		decoder.UseNumber()
		if len(change.Data) == 0 || decoder.Decode(&data) != nil || data == nil {
			return errors.New("upsert data must be a JSON object")
		}
		id, ok := data["id"].(string)
		if !ok || id != change.RecordID {
			return errors.New("upsert data.id must equal recordId")
		}
		if err := validateSpecialRecord(change.Table, data); err != nil {
			return err
		}
	}
	return nil
}

func validateSpecialRecord(table string, data map[string]interface{}) error {
	switch table {
	case "shared_settings":
		if data["schemaVersion"] != json.Number("1") {
			return errors.New("shared_settings must use app-settings schemaVersion 1")
		}
	case "synced_model_configs":
		if data["schemaVersion"] != json.Number("1") {
			return errors.New("synced_model_configs must use schemaVersion 1")
		}
		if containsSecretKey(data) {
			return errors.New("synced_model_configs data contains a secret-like key")
		}
	}
	return nil
}

func containsSecretKey(value interface{}) bool {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			if isSecretKey(key) || containsSecretKey(child) {
				return true
			}
		}
	case []interface{}:
		for _, child := range typed {
			if containsSecretKey(child) {
				return true
			}
		}
	}
	return false
}

func isSecretKey(key string) bool {
	normalized := strings.ToLower(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return -1
	}, key))
	switch normalized {
	case "apikey", "apikeysecretref", "secret", "secretkey", "token", "authtoken", "accesstoken", "refreshtoken", "password", "credential", "credentials", "authorization", "authkey":
		return true
	default:
		return false
	}
}

func updateProjection(tx *gorm.DB, userID int64, change ChangeRecord, seq int64, now time.Time) error {
	key := "user_id = ? AND table_name = ? AND record_id = ?"
	if err := tx.Where(key, userID, change.Table, change.RecordID).Delete(&database.SyncRecordBlob{}).Error; err != nil {
		return fmt.Errorf("delete sync record blob refs: %w", err)
	}
	if change.Op == "delete" {
		if err := tx.Where(key, userID, change.Table, change.RecordID).Delete(&database.SyncRecord{}).Error; err != nil {
			return fmt.Errorf("delete sync record: %w", err)
		}
		return nil
	}
	category, objectID, err := projectionIdentityDB(tx, userID, change.Table, change.RecordID, change.Data)
	if err != nil {
		return err
	}
	record := database.SyncRecord{UserID: userID, TableName: change.Table, RecordID: change.RecordID, Category: category, ObjectID: objectID, Data: string(change.Data), Seq: seq, UpdatedAt: now}
	if err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "table_name"}, {Name: "record_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"category", "object_id", "data", "seq", "updated_at"}),
	}).Create(&record).Error; err != nil {
		return fmt.Errorf("upsert sync record: %w", err)
	}
	for _, hash := range extractBlobRefs(change.Table, change.Data) {
		ref := database.SyncRecordBlob{UserID: userID, TableName: change.Table, RecordID: change.RecordID, SHA256: hash}
		if err := tx.Create(&ref).Error; err != nil {
			return fmt.Errorf("create sync record blob ref: %w", err)
		}
	}
	if change.Table == "note_pages" {
		if err := relinkNotePageRecords(tx, userID, change.RecordID, objectID); err != nil {
			return err
		}
	}
	return nil
}

func extractBlobRefs(table string, raw json.RawMessage) []string {
	var data map[string]interface{}
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	values := make(map[string]struct{})
	add := func(value interface{}) {
		if hash, ok := value.(string); ok && sha256ValuePattern.MatchString(hash) {
			values[hash] = struct{}{}
		}
	}
	switch table {
	case "resources":
		add(data["sha256"])
	case "note_revisions":
		add(data["contentHash"])
	case "plugin_files", "plugin_settings", "plugin_config":
		collectSHA256Values(data, add)
	}
	result := make([]string, 0, len(values))
	for hash := range values {
		result = append(result, hash)
	}
	sort.Strings(result)
	return result
}

func collectSHA256Values(value interface{}, add func(interface{})) {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			if strings.HasSuffix(strings.ToLower(key), "sha256") {
				add(child)
			}
			collectSHA256Values(child, add)
		}
	case []interface{}:
		for _, child := range typed {
			collectSHA256Values(child, add)
		}
	}
}

func metadataGeneration(meta database.SyncMetadata) int64 {
	if meta.Generation == 0 {
		return 1
	}
	return meta.Generation
}

type ChangesPage struct {
	Changes         []ChangeWithSeq
	HasMore         bool
	GlobalLatestSeq int64
	Generation      int64
}

// GetChangesPage reads metadata and a bounded page from one snapshot.
func (s *Service) GetChangesPage(userID int64, since int64, limit int) (ChangesPage, error) {
	var page ChangesPage
	err := s.db.Transaction(func(tx *gorm.DB) error {
		meta, err := s.readMetadata(tx, userID)
		if err != nil {
			return err
		}
		page.Generation, page.GlobalLatestSeq = metadataGeneration(meta), meta.LastSeq
		if since > meta.LastSeq {
			return ErrFutureCursor
		}
		var changes []database.SyncChange
		if err := tx.Where("user_id = ? AND seq > ?", userID, since).Order("seq ASC").Limit(limit + 1).Find(&changes).Error; err != nil {
			return err
		}
		page.HasMore = len(changes) > limit
		if page.HasMore {
			changes = changes[:limit]
		}
		page.Changes = make([]ChangeWithSeq, 0, len(changes))
		for _, ch := range changes {
			page.Changes = append(page.Changes, changeFromModel(ch))
		}
		return nil
	})
	return page, err
}

// GetChanges returns a bounded page of changes after the given sequence number.
func (s *Service) GetChanges(userID int64, since int64, limit int) ([]ChangeWithSeq, bool, error) {
	page, err := s.GetChangesPage(userID, since, limit)
	return page.Changes, page.HasMore, err
}

func (s *Service) lookupReplay(userID int64, requestID string, bodyHash []byte, operation string, generation int64) (ReplayResponse, bool, error) {
	return s.lookupReplayDB(s.db, userID, requestID, bodyHash, operation, generation)
}

func (s *Service) lookupReplayDB(db *gorm.DB, userID int64, requestID string, bodyHash []byte, operation string, generation int64) (ReplayResponse, bool, error) {
	var replay database.SyncRequestReplay
	err := db.Where("user_id = ? AND request_id = ?", userID, requestID).First(&replay).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		meta, metaErr := s.readMetadata(db, userID)
		if metaErr != nil {
			return ReplayResponse{}, false, metaErr
		}
		if metadataGeneration(meta) != generation {
			return ReplayResponse{}, false, generationConflictError{Expected: generation, Current: metadataGeneration(meta)}
		}
		return ReplayResponse{}, false, nil
	}
	if err != nil {
		return ReplayResponse{}, false, err
	}
	if !replay.ExpiresAt.After(s.now()) {
		if err := db.Delete(&replay).Error; err != nil {
			return ReplayResponse{}, false, err
		}
		return ReplayResponse{}, false, nil
	}
	if replay.Generation != generation || replay.Operation != operation || !bytes.Equal(replay.BodyHash, bodyHash) {
		return ReplayResponse{}, false, ErrReplayConflict
	}
	return ReplayResponse{Status: replay.ResponseStatus, ContentType: replay.ResponseContentType, Body: append([]byte(nil), replay.ResponseBody...)}, true, nil
}

// DeleteExpired removes expired device challenges and sync replay responses.
func (s *Service) DeleteExpired(now time.Time) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ?", now).Delete(&database.SyncRequestReplay{}).Error; err != nil {
			return err
		}
		return tx.Where("expires_at <= ?", now).Delete(&database.DeviceChallenge{}).Error
	})
}

func sameChange(existing database.SyncChange, change ChangeRecord) bool {
	if existing.TableName != change.Table || existing.Op != change.Op || existing.RecordID != change.RecordID || !existing.ClientCreatedAt.Equal(change.ClientCreatedAt) {
		return false
	}
	var data string
	if len(change.Data) > 0 {
		data = string(change.Data)
	}
	return existing.Data == nil && data == "" || existing.Data != nil && *existing.Data == data
}

func changeFromModel(ch database.SyncChange) ChangeWithSeq {
	var data json.RawMessage
	if ch.Data != nil {
		data = json.RawMessage(*ch.Data)
	}
	return ChangeWithSeq{Seq: ch.Seq, ChangeID: ch.ChangeID, DeviceID: ch.DeviceID, Table: ch.TableName, Op: ch.Op,
		RecordID: ch.RecordID, Data: data, ClientCreatedAt: ch.ClientCreatedAt, CreatedAt: ch.CreatedAt}
}

func legacyChangeID() string {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		raw = digest[:18]
	}
	return "legacy-" + base64.RawURLEncoding.EncodeToString(raw)
}

func isUniqueViolation(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unique constraint") || strings.Contains(text, "duplicate key")
}

// GetLatestSeq returns the user's latest sync sequence number.
func (s *Service) GetLatestSeq(userID int64) (int64, error) {
	var meta database.SyncMetadata
	if err := s.db.First(&meta, "user_id = ?", userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return meta.LastSeq, nil
}

// BlobInfo describes a stored blob.
type BlobInfo struct {
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"createdAt"`
}

// BlobSaveResult describes the user's ownership metadata after an upload.
type BlobSaveResult struct {
	BlobInfo
	Owned   bool `json:"owned"`
	Created bool `json:"created"`
}

// BlobReconcileResult summarizes conservative storage repairs.
type BlobReconcileResult struct {
	StaleTemps     int
	OrphanMetadata int
	OrphanFiles    int
	CorrectedSizes int
}

// ListBlobs returns a bounded page ordered by the stable blob metadata ID.
func (s *Service) ListBlobs(userID int64, after uint64, limit int) ([]BlobInfo, uint64, bool, error) {
	var blobs []database.SyncBlob
	err := s.db.Where("user_id = ? AND id > ?", userID, after).Order("id ASC").Limit(limit + 1).Find(&blobs).Error
	if err != nil {
		return nil, after, false, err
	}
	hasMore := len(blobs) > limit
	if hasMore {
		blobs = blobs[:limit]
	}
	result := make([]BlobInfo, 0, len(blobs))
	for _, b := range blobs {
		result = append(result, BlobInfo{SHA256: b.SHA256, Size: b.Size, CreatedAt: b.CreatedAt})
	}
	nextAfter := after
	if len(blobs) > 0 {
		nextAfter = uint64(blobs[len(blobs)-1].ID)
	}
	return result, nextAfter, hasMore, nil
}

// CheckReplay returns a stored response before an upload body is staged.
func (s *Service) CheckReplay(userID int64, requestID string, bodyHash []byte, operation string, generation int64) (ReplayResponse, bool, error) {
	return s.lookupReplay(userID, requestID, bodyHash, operation, generation)
}

// PrepareBlob streams and verifies an authorized upload before commit.
func (s *Service) PrepareBlob(userID int64, sha256 string, src io.Reader) (*PreparedBlob, error) {
	return s.blob.PrepareBlob(userID, sha256, src)
}

// SaveBlob verifies and stores a blob on disk, then records its metadata.
func (s *Service) SaveBlob(userID int64, sha256 string, src io.Reader) (BlobSaveResult, error) {
	prepared, err := s.PrepareBlob(userID, sha256, src)
	if err != nil {
		return BlobSaveResult{}, err
	}
	defer prepared.Close()
	return s.SavePreparedBlob(userID, prepared)
}

// SavePreparedBlob publishes a verified upload and establishes ownership metadata.
func (s *Service) SavePreparedBlob(userID int64, prepared *PreparedBlob) (BlobSaveResult, error) {
	var result BlobSaveResult
	err := s.withBlobLock(userID, prepared.sha256, func(tx *gorm.DB) error {
		commit, err := prepared.Commit()
		if err != nil {
			return err
		}
		var saveErr error
		result, saveErr = saveBlobMetadata(tx, userID, prepared.sha256, commit.Size, s.now())
		return saveErr
	})
	if err != nil {
		return BlobSaveResult{}, err
	}
	return result, nil
}

// SavePreparedBlobIdempotent commits a signed upload and stores its exact response.
func (s *Service) SavePreparedBlobIdempotent(userID int64, prepared *PreparedBlob, requestID string, bodyHash []byte, operation string, expectedGeneration int64) (ReplayResponse, error) {
	defer prepared.Close()
	var response ReplayResponse
	err := s.withBlobLock(userID, prepared.sha256, func(tx *gorm.DB) error {
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
		commit, err := prepared.Commit()
		if err != nil {
			return err
		}
		now := s.now()
		result, err := saveBlobMetadata(tx, userID, prepared.sha256, commit.Size, now)
		if err != nil {
			return err
		}
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		response = ReplayResponse{Status: 200, ContentType: "application/json; charset=utf-8", Body: body}
		replay := database.SyncRequestReplay{UserID: userID, RequestID: requestID, Generation: expectedGeneration, Operation: operation, BodyHash: bodyHash,
			ResponseStatus: response.Status, ResponseContentType: response.ContentType, ResponseBody: body,
			CreatedAt: now, ExpiresAt: now.Add(s.replayRetention)}
		return tx.Create(&replay).Error
	})
	if err != nil {
		if isUniqueViolation(err) {
			if stored, found, lookupErr := s.lookupReplay(userID, requestID, bodyHash, operation, expectedGeneration); lookupErr != nil || found {
				return stored, lookupErr
			}
		}
		return ReplayResponse{}, err
	}
	return response, nil
}

// LoadBlob requires both ownership metadata and the final file.
func (s *Service) LoadBlob(userID int64, sha256 string) ([]byte, error) {
	var data []byte
	corrupt := false
	err := s.withBlobLock(userID, sha256, func(tx *gorm.DB) error {
		var blob database.SyncBlob
		if err := tx.Where("user_id = ? AND sha256 = ?", userID, sha256).First(&blob).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrBlobNotFound
			}
			return err
		}
		loaded, err := s.blob.LoadBlob(userID, sha256)
		if errors.Is(err, os.ErrNotExist) {
			return ErrBlobNotFound
		}
		if err != nil {
			return err
		}
		if sha256Hex(loaded) != sha256 {
			if err := s.blob.DeleteBlob(userID, sha256); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := tx.Delete(&blob).Error; err != nil {
				return err
			}
			corrupt = true
			return nil
		}
		data = loaded
		return nil
	})
	if err == nil && corrupt {
		return nil, ErrBlobNotFound
	}
	return data, err
}

// ReconcileBlobs removes only stale inconsistencies older than grace.
func (s *Service) ReconcileBlobs(now time.Time, grace time.Duration) (BlobReconcileResult, error) {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	cutoff := now.Add(-grace)
	orphanCutoff := cutoff
	if s.replayRetention > grace {
		orphanCutoff = now.Add(-s.replayRetention)
	}
	var result BlobReconcileResult
	removed, err := s.blob.CleanupStaleTemps(cutoff)
	if err != nil {
		return result, err
	}
	result.StaleTemps = removed

	files, err := s.blob.ListBlobFiles()
	if err != nil {
		return result, err
	}
	fileByKey := make(map[string]BlobFile, len(files))
	for _, file := range files {
		fileByKey[blobKey(file.UserID, file.SHA256)] = file
	}
	var metadata []database.SyncBlob
	if err := s.db.Find(&metadata).Error; err != nil {
		return result, err
	}
	for _, blob := range metadata {
		key := blobKey(blob.UserID, blob.SHA256)
		file, found := fileByKey[key]
		if !found {
			if !blob.CreatedAt.After(cutoff) {
				if err := s.db.Delete(&blob).Error; err != nil {
					return result, err
				}
				result.OrphanMetadata++
			}
			continue
		}
		delete(fileByKey, key)
		if blob.Size != file.Size {
			if err := s.db.Model(&blob).Update("size", file.Size).Error; err != nil {
				return result, err
			}
			result.CorrectedSizes++
		}
	}
	for _, file := range fileByKey {
		if file.ModTime.After(orphanCutoff) {
			continue
		}
		if err := s.blob.DeleteBlob(file.UserID, file.SHA256); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		result.OrphanFiles++
	}
	return result, nil
}

func (s *Service) withBlobLock(userID int64, sha256 string, fn func(*gorm.DB) error) error {
	if s.db.Dialector.Name() != "postgres" {
		s.blobMu.Lock()
		defer s.blobMu.Unlock()
		return s.db.Transaction(fn)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", blobLockID(userID, sha256)).Error; err != nil {
			return fmt.Errorf("acquire blob advisory lock: %w", err)
		}
		return fn(tx)
	})
}

func blobLockID(userID int64, sha256 string) int64 {
	hash := fnv.New64a()
	_, _ = fmt.Fprintf(hash, "%d/%s", userID, sha256)
	return int64(hash.Sum64())
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:])
}

// RunCleanup periodically expires replay state and reconciles blob storage.
func (s *Service) RunCleanup(ctx context.Context, interval, grace time.Duration, report func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := s.DeleteExpired(now); err != nil && report != nil {
				report(err)
			}
			if _, err := s.ReconcileBlobs(now, grace); err != nil && report != nil {
				report(err)
			}
		}
	}
}

func blobSaveResult(blob database.SyncBlob, created bool) BlobSaveResult {
	return BlobSaveResult{BlobInfo: BlobInfo{SHA256: blob.SHA256, Size: blob.Size, CreatedAt: blob.CreatedAt}, Owned: true, Created: created}
}

func saveBlobMetadata(tx *gorm.DB, userID int64, sha256 string, size int64, now time.Time) (BlobSaveResult, error) {
	blob := database.SyncBlob{UserID: userID, SHA256: sha256, Size: size, CreatedAt: now}
	create := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&blob)
	if create.Error != nil {
		return BlobSaveResult{}, create.Error
	}
	created := create.RowsAffected == 1
	if !created {
		if err := tx.Where("user_id = ? AND sha256 = ?", userID, sha256).First(&blob).Error; err != nil {
			return BlobSaveResult{}, err
		}
		if blob.Size != size {
			blob.Size = size
			if err := tx.Model(&blob).Update("size", size).Error; err != nil {
				return BlobSaveResult{}, err
			}
		}
	}
	return blobSaveResult(blob, created), nil
}

func blobKey(userID int64, sha256 string) string {
	return fmt.Sprintf("%d/%s", userID, sha256)
}
