package market

import (
	"fmt"
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

// SavePluginZip writes the given ZIP bytes to disk and returns the relative path.
// The path is <pluginID>/<version>.zip under the plugins directory.
func (s *Storage) SavePluginZip(pluginID, version string, data []byte) (string, error) {
	subDir := filepath.Join(s.baseDir, pluginID)
	if !isWithinDir(s.baseDir, subDir) {
		return "", fmt.Errorf("invalid plugin path")
	}
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		return "", fmt.Errorf("create plugin dir: %w", err)
	}
	fileName := fmt.Sprintf("%s.zip", version)
	fullPath := filepath.Join(subDir, fileName)
	if !isWithinDir(s.baseDir, fullPath) {
		return "", fmt.Errorf("invalid plugin path")
	}
	if err := os.WriteFile(fullPath, data, 0o644); err != nil {
		return "", fmt.Errorf("write zip: %w", err)
	}
	return filepath.Join(pluginID, fileName), nil
}

// FullPath converts a stored relative path to a full filesystem path.
func (s *Storage) FullPath(relPath string) string {
	return filepath.Join(s.baseDir, relPath)
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
