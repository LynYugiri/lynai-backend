package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newBlobTestService(t *testing.T) (*Service, *BlobStorage, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.SyncBlob{}, &database.SyncRequestReplay{}, &database.DeviceChallenge{}); err != nil {
		t.Fatal(err)
	}
	storage, err := NewBlobStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewServiceWithReplayRetention(db, storage, time.Hour), storage, db
}

func TestLoadBlobRequiresMetadataAndFile(t *testing.T) {
	service, storage, db := newBlobTestService(t)
	content := []byte("authorized blob")
	hash := sha256.Sum256(content)
	sha := hex.EncodeToString(hash[:])
	if _, err := storage.SaveBlob(42, sha, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.LoadBlob(42, sha); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("file-only LoadBlob error = %v, want ErrBlobNotFound", err)
	}
	metadata := database.SyncBlob{UserID: 42, SHA256: sha, Size: int64(len(content)), CreatedAt: time.Now()}
	if err := db.Create(&metadata).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(storage.BlobPath(42, sha)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.LoadBlob(42, sha); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("metadata-only LoadBlob error = %v, want ErrBlobNotFound", err)
	}
}

func TestFailedDuplicateMetadataWritePreservesValidBlob(t *testing.T) {
	service, _, db := newBlobTestService(t)
	content := []byte("valid concurrent blob")
	hash := sha256.Sum256(content)
	sha := hex.EncodeToString(hash[:])
	if _, err := service.SaveBlob(42, sha, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected metadata failure")
	if err := db.Callback().Create().Before("gorm:create").Register("test:fail_blob_metadata", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "sync_blobs" {
			tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SaveBlob(42, sha, bytes.NewReader(content)); !errors.Is(err, injected) {
		t.Fatalf("duplicate SaveBlob error = %v, want injected failure", err)
	}
	stored, err := service.LoadBlob(42, sha)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("stored content = %q, want %q", stored, content)
	}
}

func TestVerifiedUploadRepairsCorruptExistingBlob(t *testing.T) {
	service, storage, db := newBlobTestService(t)
	content := []byte("verified replacement")
	hash := sha256.Sum256(content)
	sha := hex.EncodeToString(hash[:])
	if err := os.MkdirAll(filepath.Dir(storage.BlobPath(42, sha)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storage.BlobPath(42, sha), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&database.SyncBlob{UserID: 42, SHA256: sha, Size: 7, CreatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}

	result, err := service.SaveBlob(42, sha, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if result.Size != int64(len(content)) {
		t.Fatalf("repaired size = %d, want %d", result.Size, len(content))
	}
	stored, err := os.ReadFile(storage.BlobPath(42, sha))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("repaired content = %q, want %q", stored, content)
	}
}

func TestLoadBlobDeletesCorruptFileAndMetadata(t *testing.T) {
	service, storage, db := newBlobTestService(t)
	content := []byte("expected content")
	hash := sha256.Sum256(content)
	sha := hex.EncodeToString(hash[:])
	if err := os.MkdirAll(filepath.Dir(storage.BlobPath(42, sha)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storage.BlobPath(42, sha), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&database.SyncBlob{UserID: 42, SHA256: sha, Size: 7, CreatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := service.LoadBlob(42, sha); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("LoadBlob error = %v, want ErrBlobNotFound", err)
	}
	if _, err := os.Stat(storage.BlobPath(42, sha)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt file stat error = %v, want not exist", err)
	}
	var count int64
	if err := db.Model(&database.SyncBlob{}).Where("user_id = ? AND sha256 = ?", 42, sha).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("corrupt metadata count = %d, want 0", count)
	}
}

func TestReconcileBlobsConservativelyCleansOrphans(t *testing.T) {
	service, storage, db := newBlobTestService(t)
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	service.replayRetention = 30 * time.Minute

	tempDir := storage.BlobPath(42, "")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(tempDir, ".upload-stale")
	if err := os.WriteFile(tempPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tempPath, old, old); err != nil {
		t.Fatal(err)
	}

	orphanContent := []byte("orphan file")
	orphanHash := sha256.Sum256(orphanContent)
	orphanSHA := hex.EncodeToString(orphanHash[:])
	if _, err := storage.SaveBlob(42, orphanSHA, bytes.NewReader(orphanContent)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(storage.BlobPath(42, orphanSHA), old, old); err != nil {
		t.Fatal(err)
	}
	missing := database.SyncBlob{UserID: 42, SHA256: hex.EncodeToString(make([]byte, 32)), Size: 10, CreatedAt: old}
	if err := db.Create(&missing).Error; err != nil {
		t.Fatal(err)
	}

	result, err := service.ReconcileBlobs(now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if result.StaleTemps != 1 || result.OrphanMetadata != 1 || result.OrphanFiles != 1 {
		t.Fatalf("reconcile result = %+v", result)
	}
	if _, err := os.Stat(storage.BlobPath(42, orphanSHA)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan file stat error = %v, want not exist", err)
	}
}

func TestReconcileBlobsKeepsRecentOrphans(t *testing.T) {
	service, storage, db := newBlobTestService(t)
	now := time.Now()
	content := []byte("recent orphan")
	hash := sha256.Sum256(content)
	sha := hex.EncodeToString(hash[:])
	if _, err := storage.SaveBlob(42, sha, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	metadata := database.SyncBlob{UserID: 43, SHA256: sha, Size: int64(len(content)), CreatedAt: now}
	if err := db.Create(&metadata).Error; err != nil {
		t.Fatal(err)
	}
	result, err := service.ReconcileBlobs(now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if result.OrphanMetadata != 0 || result.OrphanFiles != 0 {
		t.Fatalf("recent reconcile result = %+v", result)
	}
}
