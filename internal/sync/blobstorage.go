package sync

import (
	"fmt"
	"os"
	"path/filepath"
)

// BlobStorage manages sync blob files on disk.
type BlobStorage struct {
	baseDir string
}

// NewBlobStorage creates a blob storage rooted at baseDir/sync.
func NewBlobStorage(baseDir string) (*BlobStorage, error) {
	dir := filepath.Join(baseDir, "sync")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create sync dir: %w", err)
	}
	return &BlobStorage{baseDir: dir}, nil
}

// SaveBlob writes blob bytes to disk under <userID>/<sha256>.
func (s *BlobStorage) SaveBlob(userID int64, sha256 string, data []byte) error {
	subDir := filepath.Join(s.baseDir, fmt.Sprintf("%d", userID))
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		return fmt.Errorf("create user dir: %w", err)
	}
	return os.WriteFile(filepath.Join(subDir, sha256), data, 0o644)
}

// LoadBlob reads blob bytes from disk.
func (s *BlobStorage) LoadBlob(userID int64, sha256 string) ([]byte, error) {
	path := filepath.Join(s.baseDir, fmt.Sprintf("%d", userID), sha256)
	return os.ReadFile(path)
}

// BlobPath returns the full path for a blob (used for existence check).
func (s *BlobStorage) BlobPath(userID int64, sha256 string) string {
	return filepath.Join(s.baseDir, fmt.Sprintf("%d", userID), sha256)
}

// DeleteBlob removes a blob from disk.
func (s *BlobStorage) DeleteBlob(userID int64, sha256 string) error {
	path := s.BlobPath(userID, sha256)
	return os.Remove(path)
}
