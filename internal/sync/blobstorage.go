package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

var ErrBlobHashMismatch = errors.New("blob SHA-256 mismatch")

// BlobStorage manages sync blob files on disk.
type BlobStorage struct {
	baseDir string
}

// PreparedBlob is a verified upload that has not yet been published.
type PreparedBlob struct {
	storage *BlobStorage
	userID  int64
	sha256  string
	size    int64
	temp    string
}

// BlobCommit describes whether this upload published the final file.
type BlobCommit struct {
	Size    int64
	Created bool
}

// BlobFile identifies a valid-looking final blob file during reconciliation.
type BlobFile struct {
	UserID  int64
	SHA256  string
	Size    int64
	ModTime time.Time
}

// NewBlobStorage creates a blob storage rooted at baseDir/sync.
func NewBlobStorage(baseDir string) (*BlobStorage, error) {
	dir := filepath.Join(baseDir, "sync")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create sync dir: %w", err)
	}
	return &BlobStorage{baseDir: dir}, nil
}

// PrepareBlob streams and verifies a blob in a private temporary file.
func (s *BlobStorage) PrepareBlob(userID int64, expectedSHA256 string, src io.Reader) (_ *PreparedBlob, err error) {
	subDir := filepath.Join(s.baseDir, strconv.FormatInt(userID, 10))
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		return nil, fmt.Errorf("create user dir: %w", err)
	}
	temp, err := os.CreateTemp(subDir, ".upload-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary blob: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		if err != nil {
			_ = temp.Close()
			_ = os.Remove(tempPath)
		}
	}()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(temp, hash), src)
	if err != nil {
		return nil, fmt.Errorf("write temporary blob: %w", err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedSHA256 {
		return nil, ErrBlobHashMismatch
	}
	if err := temp.Sync(); err != nil {
		return nil, fmt.Errorf("sync temporary blob: %w", err)
	}
	if err := temp.Close(); err != nil {
		return nil, fmt.Errorf("close temporary blob: %w", err)
	}
	return &PreparedBlob{storage: s, userID: userID, sha256: expectedSHA256, size: size, temp: tempPath}, nil
}

// Commit atomically publishes the upload, replacing an existing corrupt file.
func (p *PreparedBlob) Commit() (BlobCommit, error) {
	if p.temp == "" {
		return BlobCommit{}, errors.New("prepared blob is already closed")
	}
	defer p.Close()
	finalPath := p.storage.BlobPath(p.userID, p.sha256)
	if err := os.Link(p.temp, finalPath); err == nil {
		return BlobCommit{Size: p.size, Created: true}, nil
	} else if !errors.Is(err, fs.ErrExist) {
		return BlobCommit{}, fmt.Errorf("commit blob: %w", err)
	}
	info, err := os.Stat(finalPath)
	if err == nil {
		valid, verifyErr := verifyBlobFile(finalPath, p.sha256)
		if verifyErr != nil {
			return BlobCommit{}, verifyErr
		}
		if valid {
			return BlobCommit{Size: info.Size(), Created: false}, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return BlobCommit{}, fmt.Errorf("stat existing blob: %w", err)
	}
	if err := os.Rename(p.temp, finalPath); err != nil {
		return BlobCommit{}, fmt.Errorf("commit blob: %w", err)
	}
	p.temp = ""
	return BlobCommit{Size: p.size, Created: true}, nil
}

// Close discards an unpublished upload.
func (p *PreparedBlob) Close() error {
	if p == nil || p.temp == "" {
		return nil
	}
	temp := p.temp
	p.temp = ""
	if err := os.Remove(temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// SaveBlob verifies and atomically publishes a blob.
func (s *BlobStorage) SaveBlob(userID int64, expectedSHA256 string, src io.Reader) (BlobCommit, error) {
	prepared, err := s.PrepareBlob(userID, expectedSHA256, src)
	if err != nil {
		return BlobCommit{}, err
	}
	return prepared.Commit()
}

// LoadBlob reads blob bytes from disk.
func (s *BlobStorage) LoadBlob(userID int64, sha256 string) ([]byte, error) {
	return os.ReadFile(s.BlobPath(userID, sha256))
}

func verifyBlobFile(path, expectedSHA256 string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open existing blob: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false, fmt.Errorf("hash existing blob: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)) == expectedSHA256, nil
}

// BlobPath returns the full path for a blob.
func (s *BlobStorage) BlobPath(userID int64, sha256 string) string {
	return filepath.Join(s.baseDir, strconv.FormatInt(userID, 10), sha256)
}

// DeleteBlob removes a blob from disk.
func (s *BlobStorage) DeleteBlob(userID int64, sha256 string) error {
	return os.Remove(s.BlobPath(userID, sha256))
}

// CleanupStaleTemps removes private upload files older than cutoff.
func (s *BlobStorage) CleanupStaleTemps(cutoff time.Time) (int, error) {
	removed := 0
	err := filepath.WalkDir(s.baseDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !isUploadTemp(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		removed++
		return nil
	})
	return removed, err
}

// ListBlobFiles returns only canonical <user>/<sha256> final files.
func (s *BlobStorage) ListBlobFiles() ([]BlobFile, error) {
	users, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, err
	}
	var result []BlobFile
	for _, userDir := range users {
		if !userDir.IsDir() {
			continue
		}
		userID, err := strconv.ParseInt(userDir.Name(), 10, 64)
		if err != nil || userID < 1 {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.baseDir, userDir.Name()))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !sha256Pattern.MatchString(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			result = append(result, BlobFile{UserID: userID, SHA256: entry.Name(), Size: info.Size(), ModTime: info.ModTime()})
		}
	}
	return result, nil
}

func isUploadTemp(name string) bool {
	return len(name) > len(".upload-") && name[:len(".upload-")] == ".upload-"
}
