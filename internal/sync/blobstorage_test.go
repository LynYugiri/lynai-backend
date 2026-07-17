package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestBlobStorageVerifiesHashAndCleansTemporaryFile(t *testing.T) {
	storage, err := NewBlobStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = storage.SaveBlob(42, string(bytes.Repeat([]byte("0"), 64)), bytes.NewReader([]byte("content")))
	if !errors.Is(err, ErrBlobHashMismatch) {
		t.Fatalf("SaveBlob error = %v, want ErrBlobHashMismatch", err)
	}
	files, err := filepath.Glob(filepath.Join(storage.baseDir, "42", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("failed upload left files: %v", files)
	}
}

func TestBlobStorageCommitsVerifiedContent(t *testing.T) {
	storage, err := NewBlobStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("verified content")
	hash := sha256.Sum256(content)
	expected := hex.EncodeToString(hash[:])

	commit, err := storage.SaveBlob(42, expected, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if commit.Size != int64(len(content)) || !commit.Created {
		t.Fatalf("commit = %+v, want size %d and created", commit, len(content))
	}
	stored, err := os.ReadFile(storage.BlobPath(42, expected))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("stored content = %q, want %q", stored, content)
	}
}

func TestBlobStorageConcurrentSameHashPublishesOnce(t *testing.T) {
	storage, err := NewBlobStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("concurrent verified content")
	hash := sha256.Sum256(content)
	expected := hex.EncodeToString(hash[:])

	var wg sync.WaitGroup
	commits := make(chan BlobCommit, 16)
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			commit, err := storage.SaveBlob(42, expected, bytes.NewReader(content))
			commits <- commit
			errs <- err
		}()
	}
	wg.Wait()
	close(commits)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	created := 0
	for commit := range commits {
		if commit.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created commits = %d, want 1", created)
	}
	stored, err := os.ReadFile(storage.BlobPath(42, expected))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("stored content = %q, want %q", stored, content)
	}
}
