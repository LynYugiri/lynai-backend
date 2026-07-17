package relay

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestServiceNonStreamingTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewServiceWithClient(db, upstream.Client())
	svc.setTimeouts(20*time.Millisecond, 20*time.Millisecond, time.Second)
	_, err = svc.ForwardChat(t.Context(), &database.RelayProvider{Endpoint: upstream.URL}, []byte(`{"stream":false}`))
	if !errors.Is(err, ErrUpstreamTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestServiceStreamingIdleTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: first\n\n"))
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte("data: second\n\n"))
	}))
	defer upstream.Close()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewServiceWithClient(db, upstream.Client())
	svc.setTimeouts(time.Second, 20*time.Millisecond, time.Second)
	resp, err := svc.ForwardChat(t.Context(), &database.RelayProvider{Endpoint: upstream.URL}, []byte(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	if !errors.Is(err, ErrUpstreamTimeout) {
		t.Fatalf("idle timeout error = %v", err)
	}
}

func TestWriteForwardTimeoutReturns504(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	writeForwardError(ctx, ErrUpstreamTimeout)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", recorder.Code)
	}
}

func TestLogServiceCloseDrainsQueue(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.RelayRequestLog{}); err != nil {
		t.Fatal(err)
	}
	logs := NewLogService(db)
	for i := 0; i < 50; i++ {
		if !logs.Enqueue(database.RelayRequestLog{UserID: 1, Username: "u", Operation: "chat", Route: "/relay/chat", Protocol: "v1", HTTPStatus: 200, CreatedAt: time.Now()}) {
			t.Fatal("enqueue failed")
		}
	}
	logs.Close()
	var count int64
	if err := db.Model(&database.RelayRequestLog{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 50 {
		t.Fatalf("drained logs = %d, want 50", count)
	}
	if logs.Enqueue(database.RelayRequestLog{}) {
		t.Fatal("enqueue succeeded after close")
	}
}
