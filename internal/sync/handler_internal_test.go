package sync

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type failOnReadBody struct {
	read bool
}

func (b *failOnReadBody) Read([]byte) (int, error) {
	b.read = true
	return 0, errors.New("body must not be read before signature verification")
}

func (*failOnReadBody) Close() error { return nil }

func TestInvalidBlobSignatureDoesNotStageBody(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.UserDevice{}, &database.SyncBlob{}, &database.SyncRequestReplay{}); err != nil {
		t.Fatal(err)
	}
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	deviceDigest := sha256.Sum256(publicKey)
	deviceID := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(deviceDigest[:]))
	const userID int64 = 42
	const sessionID = "session-1"
	if err := db.Create(&database.UserDevice{UserID: userID, DeviceID: deviceID, SessionID: sessionID, Name: "test", Platform: "linux", Protocol: 1, PublicKey: publicKey}).Error; err != nil {
		t.Fatal(err)
	}
	baseDir := t.TempDir()
	storage, err := NewBlobStorage(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(NewService(db, storage))
	bodyDigest := sha256.Sum256([]byte("blob content"))
	sha := hex.EncodeToString(bodyDigest[:])
	requestID := strings.Repeat("a", 32)
	body := &failOnReadBody{}
	req := httptest.NewRequest(http.MethodPost, "/sync/blobs/"+sha, nil)
	req.Body = body
	req.Header.Set("X-LynAI-Protocol", "1")
	req.Header.Set("X-LynAI-Device-ID", deviceID)
	req.Header.Set("X-LynAI-Timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	req.Header.Set("X-LynAI-Request-ID", requestID)
	req.Header.Set("X-LynAI-Body-SHA256", sha)
	req.Header.Set("X-LynAI-Signature", base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)))
	recorder := httptest.NewRecorder()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", strconv.FormatInt(userID, 10))
		c.Set("sessionID", sessionID)
		c.Next()
	})
	router.POST("/sync/blobs/:sha256", handler.UploadBlob)
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if body.read {
		t.Fatal("invalid signature caused request body read")
	}
	userDir := filepath.Join(baseDir, "sync", strconv.FormatInt(userID, 10))
	if _, err := os.Stat(userDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blob staging directory exists after invalid signature: %v", err)
	}
}

func TestDownloadBlobDistinguishesMissingFromCorruptStorage(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.SyncBlob{}); err != nil {
		t.Fatal(err)
	}
	storage, err := NewBlobStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const userID int64 = 42
	handler := NewHandler(NewService(db, storage))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", strconv.FormatInt(userID, 10))
		c.Next()
	})
	router.GET("/sync/blobs/:sha256", handler.DownloadBlob)

	missingSHA := strings.Repeat("0", 64)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sync/blobs/"+missingSHA, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing blob status = %d, want 404", recorder.Code)
	}

	for _, tc := range []struct {
		name string
		sha  string
		make func(string) error
	}{
		{name: "hash mismatch", sha: strings.Repeat("1", 64), make: func(path string) error {
			return os.WriteFile(path, []byte("corrupt"), 0o600)
		}},
		{name: "oversized", sha: strings.Repeat("2", 64), make: func(path string) error {
			file, err := os.Create(path)
			if err != nil {
				return err
			}
			if err := file.Truncate(MaxBlobBytes + 1); err != nil {
				_ = file.Close()
				return err
			}
			return file.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := storage.BlobPath(userID, tc.sha)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := tc.make(path); err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&database.SyncBlob{UserID: userID, SHA256: tc.sha, Size: 1, CreatedAt: time.Now()}).Error; err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sync/blobs/"+tc.sha, nil))
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", recorder.Code)
			}
		})
	}
}

var _ io.ReadCloser = (*failOnReadBody)(nil)
