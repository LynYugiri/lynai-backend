package community

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/webp"
)

const (
	MaxImageBytes  int64 = 8 << 20
	MaxImageSide         = 8192
	MaxImagePixels int64 = 40_000_000
)

var (
	ErrImageTooLarge    = errors.New("image exceeds 8 MiB limit")
	ErrUnsupportedImage = errors.New("image must be JPEG, PNG, WebP, or GIF")
	ErrInvalidImage     = errors.New("invalid image")
)

// StoredImage is the validated immutable image produced by Storage.PutImage.
type StoredImage struct {
	SHA256    string
	Path      string
	MediaType string
	Size      int64
	Width     int
	Height    int
}

// Storage manages content-addressed community images under community/media.
type Storage struct {
	baseDir string
}

// NewStorage creates a community media store rooted under storageDir.
func NewStorage(storageDir string) (*Storage, error) {
	baseDir := filepath.Join(storageDir, "community", "media")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create community media dir: %w", err)
	}
	return &Storage{baseDir: baseDir}, nil
}

// PutImage validates, hashes, and atomically publishes an uploaded image.
func (s *Storage) PutImage(src io.Reader) (StoredImage, error) {
	data, err := io.ReadAll(io.LimitReader(src, MaxImageBytes+1))
	if err != nil {
		return StoredImage{}, fmt.Errorf("read image: %w", err)
	}
	if int64(len(data)) > MaxImageBytes {
		return StoredImage{}, ErrImageTooLarge
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return StoredImage{}, ErrInvalidImage
	}
	mediaType, extension, ok := imageFormat(format)
	if !ok {
		return StoredImage{}, ErrUnsupportedImage
	}
	if config.Width < 1 || config.Height < 1 || config.Width > MaxImageSide || config.Height > MaxImageSide || int64(config.Width)*int64(config.Height) > MaxImagePixels {
		return StoredImage{}, fmt.Errorf("%w: dimensions exceed limits", ErrInvalidImage)
	}
	if _, decodedFormat, err := image.Decode(bytes.NewReader(data)); err != nil || decodedFormat != format {
		return StoredImage{}, ErrInvalidImage
	}

	digest := sha256.Sum256(data)
	sha := hex.EncodeToString(digest[:])
	relPath := filepath.Join(sha[:2], sha[2:4], sha+extension)
	fullPath := filepath.Join(s.baseDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return StoredImage{}, fmt.Errorf("create image hash dir: %w", err)
	}
	if err := publishImmutable(fullPath, data); err != nil {
		return StoredImage{}, err
	}
	return StoredImage{SHA256: sha, Path: relPath, MediaType: mediaType, Size: int64(len(data)), Width: config.Width, Height: config.Height}, nil
}

// Open opens a stored relative media path after verifying it stays in the store.
func (s *Storage) Open(relPath string) (*os.File, error) {
	fullPath := filepath.Join(s.baseDir, relPath)
	rel, err := filepath.Rel(s.baseDir, fullPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, os.ErrNotExist
	}
	return os.Open(fullPath)
}

// Delete removes an unreferenced image. Missing files are ignored.
func (s *Storage) Delete(relPath string) error {
	file, err := s.Open(relPath)
	if err == nil {
		fullPath := file.Name()
		_ = file.Close()
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func imageFormat(format string) (mediaType, extension string, ok bool) {
	switch format {
	case "jpeg":
		return "image/jpeg", ".jpg", true
	case "png":
		return "image/png", ".png", true
	case "webp":
		return "image/webp", ".webp", true
	case "gif":
		return "image/gif", ".gif", true
	default:
		return "", "", false
	}
}

func publishImmutable(fullPath string, data []byte) error {
	if info, err := os.Stat(fullPath); err == nil && info.Mode().IsRegular() {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(fullPath), ".community-image-*")
	if err != nil {
		return fmt.Errorf("create image temp: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write image temp: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync image temp: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close image temp: %w", err)
	}
	if err := os.Link(tempPath, fullPath); err != nil && !os.IsExist(err) {
		return fmt.Errorf("publish image: %w", err)
	}
	return nil
}
