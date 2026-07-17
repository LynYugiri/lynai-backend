package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
)

// Open creates a connection scoped to a disposable schema. It skips when
// TEST_POSTGRES_DSN is unset so the default local test suite needs no database.
func Open(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generate PostgreSQL test schema: %v", err)
	}
	schema := "lynai_test_" + hex.EncodeToString(suffix[:])

	admin, err := database.Connect(dsn)
	if err != nil {
		t.Fatalf("connect to test PostgreSQL: %v", err)
	}
	adminSQL, err := admin.DB()
	if err != nil {
		t.Fatalf("get test PostgreSQL handle: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		_ = adminSQL.Close()
		t.Fatalf("create PostgreSQL test schema: %v", err)
	}

	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		_ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		_ = adminSQL.Close()
		t.Fatalf("parse TEST_POSTGRES_DSN: %v", err)
	}
	config.RuntimeParams["search_path"] = schema
	db, err := database.Connect(config.ConnString())
	if err != nil {
		_ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		_ = adminSQL.Close()
		t.Fatalf("connect to PostgreSQL test schema: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		_ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		_ = adminSQL.Close()
		t.Fatalf("get scoped PostgreSQL handle: %v", err)
	}

	t.Cleanup(func() {
		_ = sqlDB.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = admin.WithContext(ctx).Exec("DROP SCHEMA " + schema + " CASCADE").Error
		_ = adminSQL.Close()
	})
	return db
}
