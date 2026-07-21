package database

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestEmbeddedMigrationsAreOrderedAndChecksummed(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 8 {
		t.Fatalf("migration count = %d, want 8", len(migrations))
	}
	for i, migration := range migrations {
		if migration.version != int64(i+1) {
			t.Fatalf("migration %d version = %d", i, migration.version)
		}
		if len(migration.checksum) != 64 || strings.Trim(migration.checksum, "0123456789abcdef") != "" {
			t.Fatalf("migration %d has invalid checksum %q", migration.version, migration.checksum)
		}
	}
}

func TestRelayProviderSequenceMigrationSQL(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	sql := migrations[5].sql
	for _, fragment := range []string{
		"CREATE SEQUENCE IF NOT EXISTS relay_providers_id_seq",
		"SELECT MAX(id) + 1 FROM relay_providers",
		"SELECT MAX(provider_id) + 1 FROM relay_request_logs",
		"CASE WHEN is_called THEN last_value + 1 ELSE last_value END",
		"false",
		"ALTER COLUMN id SET DEFAULT nextval('relay_providers_id_seq')",
		"OWNED BY relay_providers.id",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration 0006 missing %q", fragment)
		}
	}
}

func TestCommunityMigrationContract(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	sql := migrations[6].sql
	for _, fragment := range []string{
		"pinned_post_id BIGINT",
		"title VARCHAR(120)",
		"owner_user_id BIGINT NOT NULL",
		"attached_at TIMESTAMPTZ",
		"deleted_at TIMESTAMPTZ",
		"UNIQUE (media_id)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration 0007 missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"sha256 VARCHAR(64) NOT NULL UNIQUE", "path TEXT NOT NULL UNIQUE", "pinned_at"} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("migration 0007 contains obsolete fragment %q", forbidden)
		}
	}
}

func TestRelayModelsMigrationValidatesBeforeDroppingLegacyColumn(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	sql := migrations[7].sql
	for _, fragment := range []string{
		"parsed_models := provider.models::jsonb",
		"must be valid JSON",
		"jsonb_typeof(parsed_models) <> 'array'",
		"jsonb_typeof(element) <> 'string'",
		"BTRIM(model.model_id)",
		"relay model expansion verification failed",
		"ALTER TABLE relay_providers DROP COLUMN models",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration 0008 missing %q", fragment)
		}
	}
}

func TestEnsureAdminSeedBehavior(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}); err != nil {
		t.Fatal(err)
	}
	generator := NewSnowflakeGenerator(0)

	if err := EnsureAdminSeed(context.Background(), db, "100", "seed", "hash-1", generator); err != nil {
		t.Fatalf("create seed: %v", err)
	}
	var created User
	if err := db.Where("phone = ?", "100").First(&created).Error; err != nil {
		t.Fatal(err)
	}
	if !created.IsAdmin || created.PasswordHash != "hash-1" {
		t.Fatalf("created seed = %+v", created)
	}

	created.PasswordHash = ""
	if err := db.Save(&created).Error; err != nil {
		t.Fatal(err)
	}
	if err := EnsureAdminSeed(context.Background(), db, "100", "changed", "hash-2", generator); err != nil {
		t.Fatalf("repair admin seed: %v", err)
	}
	if err := db.First(&created, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if created.PasswordHash != "hash-2" || created.DisplayName != "seed" || !created.IsAdmin {
		t.Fatalf("repaired seed = %+v", created)
	}

	normal := User{ID: created.ID + 1, Phone: "200", PasswordHash: "user-hash", DisplayName: "user", IsAdmin: false}
	if err := db.Create(&normal).Error; err != nil {
		t.Fatal(err)
	}
	if err := EnsureAdminSeed(context.Background(), db, normal.Phone, "promoted", "new-hash", generator); !errors.Is(err, ErrAdminSeedConflict) {
		t.Fatalf("non-admin seed error = %v, want ErrAdminSeedConflict", err)
	}
	var unchanged User
	if err := db.First(&unchanged, normal.ID).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.IsAdmin || unchanged.PasswordHash != normal.PasswordHash || unchanged.DisplayName != normal.DisplayName {
		t.Fatalf("non-admin was modified: %+v", unchanged)
	}
}

func TestEnsureAdminSeedConcurrentFirstStart(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "seed.db") + "?_busy_timeout=5000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- EnsureAdminSeed(context.Background(), db.Session(&gorm.Session{NewDB: true}), "100", "seed", "hash", NewSnowflakeGenerator(0))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent seed failed: %v", err)
		}
	}
	var users []User
	if err := db.Where("phone = ?", "100").Find(&users).Error; err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || !users[0].IsAdmin {
		t.Fatalf("seed users = %+v, want one admin", users)
	}
}

func TestCheckAppliedRejectsChecksumAndHistoryProblems(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkApplied(migrations, map[int64]string{1: "wrong"}, false); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
	if err := checkApplied(migrations, map[int64]string{2: migrations[1].checksum}, false); err == nil {
		t.Fatal("non-contiguous migration history was accepted")
	}
	if err := checkApplied(migrations, map[int64]string{1: migrations[0].checksum}, true); err == nil {
		t.Fatal("outdated schema was accepted")
	}
}
