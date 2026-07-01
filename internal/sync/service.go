package sync

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Service handles incremental sync operations.
type Service struct {
	db   *gorm.DB
	blob *BlobStorage
}

// NewService creates a sync service.
func NewService(db *gorm.DB, blob *BlobStorage) *Service {
	return &Service{db: db, blob: blob}
}

// SyncStatus is the response for GET /sync/status.
type SyncStatus struct {
	LastSeq   int64 `json:"lastSeq"`
	BlobCount int64 `json:"blobCount"`
}

// GetStatus returns the user's current sync sequence and blob count.
func (s *Service) GetStatus(userID int64) (*SyncStatus, error) {
	var meta database.SyncMetadata
	err := s.db.First(&meta, "user_id = ?", userID).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}

	var blobCount int64
	s.db.Model(&database.SyncBlob{}).Where("user_id = ?", userID).Count(&blobCount)

	return &SyncStatus{
		LastSeq:   meta.LastSeq,
		BlobCount: blobCount,
	}, nil
}

// ChangeRecord is a single change in the sync log, as transmitted over the API.
type ChangeRecord struct {
	Table    string          `json:"table"`
	Op       string          `json:"op"`
	RecordID string          `json:"recordId"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// ChangeWithSeq is a change record with its assigned sequence number.
type ChangeWithSeq struct {
	Seq       int64           `json:"seq"`
	Table     string          `json:"table"`
	Op        string          `json:"op"`
	RecordID  string          `json:"recordId"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}

// UploadChanges accepts a batch of change records, assigns sequence numbers,
// persists them, and returns the assigned range.
func (s *Service) UploadChanges(userID int64, changes []ChangeRecord) ([]ChangeWithSeq, error) {
	if len(changes) == 0 {
		return []ChangeWithSeq{}, nil
	}

	result := make([]ChangeWithSeq, 0, len(changes))
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var meta database.SyncMetadata
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&meta, "user_id = ?", userID).Error
		if err == gorm.ErrRecordNotFound {
			created := database.SyncMetadata{UserID: userID, LastSeq: 0, UpdatedAt: time.Now()}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&created).Error; err != nil {
				return fmt.Errorf("create sync metadata: %w", err)
			}
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&meta, "user_id = ?", userID).Error; err != nil {
				return fmt.Errorf("lock sync metadata: %w", err)
			}
		} else if err != nil {
			return err
		}

		seq := meta.LastSeq
		for _, ch := range changes {
			seq++
			var dataPtr *string
			if len(ch.Data) > 0 {
				s := string(ch.Data)
				dataPtr = &s
			}
			change := database.SyncChange{
				UserID:    userID,
				Seq:       seq,
				TableName: ch.Table,
				Op:        ch.Op,
				RecordID:  ch.RecordID,
				Data:      dataPtr,
				CreatedAt: time.Now(),
			}
			if err := tx.Create(&change).Error; err != nil {
				return fmt.Errorf("create sync change: %w", err)
			}
			result = append(result, ChangeWithSeq{
				Seq:       seq,
				Table:     ch.Table,
				Op:        ch.Op,
				RecordID:  ch.RecordID,
				Data:      ch.Data,
				CreatedAt: change.CreatedAt,
			})
		}
		// Update metadata.
		if err := tx.Model(&database.SyncMetadata{}).
			Where("user_id = ?", userID).
			Updates(map[string]interface{}{
				"last_seq":   seq,
				"updated_at": time.Now(),
			}).Error; err != nil {
			return fmt.Errorf("update sync metadata: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetChanges returns all changes after the given sequence number.
func (s *Service) GetChanges(userID int64, since int64) ([]ChangeWithSeq, error) {
	var changes []database.SyncChange
	err := s.db.Where("user_id = ? AND seq > ?", userID, since).
		Order("seq ASC").
		Find(&changes).Error
	if err != nil {
		return nil, err
	}

	result := make([]ChangeWithSeq, 0, len(changes))
	for _, ch := range changes {
		var data json.RawMessage
		if ch.Data != nil {
			data = json.RawMessage(*ch.Data)
		}
		result = append(result, ChangeWithSeq{
			Seq:       ch.Seq,
			Table:     ch.TableName,
			Op:        ch.Op,
			RecordID:  ch.RecordID,
			Data:      data,
			CreatedAt: ch.CreatedAt,
		})
	}
	return result, nil
}

// GetLatestSeq returns the user's latest sync sequence number.
func (s *Service) GetLatestSeq(userID int64) int64 {
	var meta database.SyncMetadata
	if err := s.db.First(&meta, "user_id = ?", userID).Error; err != nil {
		return 0
	}
	return meta.LastSeq
}

// BlobInfo describes a stored blob.
type BlobInfo struct {
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
}

// ListBlobs returns all blobs owned by the user.
func (s *Service) ListBlobs(userID int64) ([]BlobInfo, error) {
	var blobs []database.SyncBlob
	err := s.db.Where("user_id = ?", userID).Find(&blobs).Error
	if err != nil {
		return nil, err
	}
	result := make([]BlobInfo, 0, len(blobs))
	for _, b := range blobs {
		result = append(result, BlobInfo{SHA256: b.SHA256, Size: b.Size})
	}
	return result, nil
}

// SaveBlob stores a blob on disk and records its metadata.
// If the blob already exists (same SHA), it's a no-op.
func (s *Service) SaveBlob(userID int64, sha256 string, data []byte) error {
	// Check if already stored.
	var count int64
	s.db.Model(&database.SyncBlob{}).
		Where("user_id = ? AND sha256 = ?", userID, sha256).
		Count(&count)
	if count > 0 {
		return nil
	}

	// Save to disk.
	if err := s.blob.SaveBlob(userID, sha256, data); err != nil {
		return err
	}

	// Record metadata.
	blob := database.SyncBlob{
		UserID:    userID,
		SHA256:    sha256,
		Size:      len(data),
		CreatedAt: time.Now(),
	}
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&blob).Error
}

// LoadBlob reads a blob from disk.
func (s *Service) LoadBlob(userID int64, sha256 string) ([]byte, error) {
	return s.blob.LoadBlob(userID, sha256)
}
