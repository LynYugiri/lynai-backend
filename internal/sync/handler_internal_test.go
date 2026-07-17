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

var _ io.ReadCloser = (*failOnReadBody)(nil)
