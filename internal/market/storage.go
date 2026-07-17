package market

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Storage manages plugin ZIP files on disk.
type Storage struct {
	baseDir string
}

// NewStorage creates a storage instance rooted at baseDir/plugins.
func NewStorage(baseDir string) (*Storage, error) {
	pluginsDir := filepath.Join(baseDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create plugins dir: %w", err)
	}
	return &Storage{baseDir: pluginsDir}, nil
}

// StagePluginZip writes an upload to a temporary file in the plugin filesystem.
func (s *Storage) StagePluginZip(src io.Reader) (string, error) {
	temp, err := os.CreateTemp(s.baseDir, ".plugin-upload-*")
	if err != nil {
		return "", fmt.Errorf("create temporary zip: %w", err)
	}
	tempPath := temp.Name()
	if _, err := io.Copy(temp, src); err != nil {
		temp.Close()
		os.Remove(tempPath)
		return "", fmt.Errorf("write temporary zip: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		os.Remove(tempPath)
		return "", fmt.Errorf("sync temporary zip: %w", err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(tempPath)
		return "", fmt.Errorf("close temporary zip: %w", err)
	}
	return tempPath, nil
}

// PublishPluginZip atomically links a staged upload to a new path without replacing an existing file.
func (s *Storage) PublishPluginZip(tempPath, fullPath string) error {
	if !isWithinDir(s.baseDir, tempPath) || !isWithinDir(s.baseDir, fullPath) {
		return fmt.Errorf("invalid plugin path")
	}
	if err := os.Link(tempPath, fullPath); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(fullPath)); err != nil {
		_ = os.Remove(fullPath)
		return err
	}
	if err := os.Remove(tempPath); err != nil {
		_ = os.Remove(fullPath)
		return err
	}
	if err := syncDir(filepath.Dir(tempPath)); err != nil {
		_ = os.Remove(fullPath)
		return err
	}
	return nil
}

// PluginZipPath returns a new immutable relative and absolute path for a plugin ZIP.
func (s *Storage) PluginZipPath(pluginID, version string) (string, string, error) {
	subDir := filepath.Join(s.baseDir, pluginID)
	if !isWithinDir(s.baseDir, subDir) {
		return "", "", fmt.Errorf("invalid plugin path")
	}
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create plugin dir: %w", err)
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", fmt.Errorf("generate plugin path: %w", err)
	}
	fileName := fmt.Sprintf("%s-%s.zip", version, hex.EncodeToString(suffix[:]))
	fullPath := filepath.Join(subDir, fileName)
	if !isWithinDir(subDir, fullPath) {
		return "", "", fmt.Errorf("invalid plugin path")
	}
	return filepath.Join(pluginID, fileName), fullPath, nil
}

// FullPath converts a stored relative path to a full filesystem path.
func (s *Storage) FullPath(relPath string) string {
	return filepath.Join(s.baseDir, relPath)
}

// DeleteTemp removes a staged upload. Missing files are ignored.
func (s *Storage) DeleteTemp(path string) {
	if path != "" && isWithinDir(s.baseDir, path) {
		_ = os.Remove(path)
	}
}

func moveAside(path string) (string, bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	backup, err := os.CreateTemp(filepath.Dir(path), ".plugin-backup-*")
	if err != nil {
		return "", false, err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		os.Remove(backupPath)
		return "", false, err
	}
	if err := os.Remove(backupPath); err != nil {
		return "", false, err
	}
	if err := os.Rename(path, backupPath); err != nil {
		return "", false, err
	}
	return backupPath, true, nil
}

func isWithinDir(baseDir, path string) bool {
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return false
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// DeletePluginZip removes a stored plugin ZIP. Missing files are ignored.
func (s *Storage) DeletePluginZip(relPath string) error {
	if relPath == "" {
		return nil
	}
	if err := os.Remove(s.FullPath(relPath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete zip: %w", err)
	}
	return nil
}
