package database_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/pgtest"
	"gorm.io/gorm"
)

func TestPostgresEmbeddedMigrationsAndValidation(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()

	if err := database.ValidateSchema(ctx, db); err == nil {
		t.Fatal("ValidateSchema accepted an unmigrated schema")
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("apply embedded migrations: %v", err)
	}
	if err := database.ValidateSchema(ctx, db); err != nil {
		t.Fatalf("validate migrated schema: %v", err)
	}

	var count int64
	if err := db.Table("schema_migrations").Count(&count).Error; err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if count != 8 {
		t.Fatalf("applied migration count = %d, want 8", count)
	}
	for _, index := range []string{"idx_user_devices_device_id_global", "idx_user_devices_public_key_global"} {
		var exists bool
		if err := db.Raw("SELECT to_regclass(?) IS NOT NULL", index).Scan(&exists).Error; err != nil {
			t.Fatalf("look up index %s: %v", index, err)
		}
		if !exists {
			t.Errorf("embedded migrations did not create index %s", index)
		}
	}
	for _, table := range []string{"community_profiles", "community_posts", "community_comments", "community_likes", "community_favorites", "community_media", "community_post_media", "community_audit_records"} {
		var exists bool
		if err := db.Raw("SELECT to_regclass(?) IS NOT NULL", table).Scan(&exists).Error; err != nil {
			t.Fatalf("look up table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("embedded migrations did not create table %s", table)
		}
	}
}

func TestPostgresRelayProviderSequenceAdvancesPastExistingRows(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	if err := db.Exec(`CREATE TABLE relay_providers (
		id BIGINT PRIMARY KEY,
		name TEXT NOT NULL,
		endpoint TEXT NOT NULL,
		api_key TEXT NOT NULL,
		api_format TEXT NOT NULL,
		config TEXT,
		models TEXT,
		enabled BOOLEAN DEFAULT TRUE,
		created_at TIMESTAMPTZ,
		updated_at TIMESTAMPTZ
	)`).Error; err != nil {
		t.Fatalf("create legacy relay_providers: %v", err)
	}
	if err := db.Exec(`INSERT INTO relay_providers (id, name, endpoint, api_key, api_format, models)
		VALUES (41, 'legacy', 'https://example.com', 'key', 'openai', '["legacy-a", "legacy-b"]')`).Error; err != nil {
		t.Fatalf("insert legacy provider: %v", err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate legacy relay providers: %v", err)
	}
	var id int64
	if err := db.Raw("INSERT INTO relay_providers (name, endpoint, api_key, api_format) VALUES ('new', 'https://example.com', 'key', 'openai') RETURNING id").Scan(&id).Error; err != nil {
		t.Fatalf("insert provider using default ID: %v", err)
	}
	if id != 42 {
		t.Fatalf("generated relay provider ID = %d, want 42", id)
	}
	var models []string
	if err := db.Raw("SELECT model_id FROM relay_models WHERE provider_id = 41 ORDER BY model_id").Scan(&models).Error; err != nil {
		t.Fatalf("list expanded relay models: %v", err)
	}
	if len(models) != 2 || models[0] != "legacy-a" || models[1] != "legacy-b" {
		t.Fatalf("expanded relay models = %#v", models)
	}
	var modelsColumnExists bool
	if err := db.Raw(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'relay_providers' AND column_name = 'models'
	)`).Scan(&modelsColumnExists).Error; err != nil {
		t.Fatalf("check relay_providers.models: %v", err)
	}
	if modelsColumnExists {
		t.Fatal("relay_providers.models still exists after migration")
	}
}

func TestPostgresRelayModelsMigrationRejectsInvalidJSONAndRollsBack(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	if err := db.Exec(`CREATE TABLE relay_providers (
		id BIGINT PRIMARY KEY,
		name TEXT NOT NULL,
		endpoint TEXT NOT NULL,
		api_key TEXT NOT NULL,
		api_format TEXT NOT NULL,
		config TEXT,
		models TEXT,
		enabled BOOLEAN DEFAULT TRUE,
		created_at TIMESTAMPTZ,
		updated_at TIMESTAMPTZ
	)`).Error; err != nil {
		t.Fatalf("create legacy relay_providers: %v", err)
	}
	if err := db.Exec(`INSERT INTO relay_providers (id, name, endpoint, api_key, api_format, models)
		VALUES (1, 'invalid', 'https://example.com', 'key', 'openai', '["valid", 2]')`).Error; err != nil {
		t.Fatalf("insert invalid legacy provider: %v", err)
	}
	if err := database.Migrate(ctx, db); err == nil {
		t.Fatal("migration accepted a non-string legacy model")
	}
	var modelsColumnExists bool
	if err := db.Raw(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'relay_providers' AND column_name = 'models'
	)`).Scan(&modelsColumnExists).Error; err != nil {
		t.Fatalf("check relay_providers.models: %v", err)
	}
	if !modelsColumnExists {
		t.Fatal("failed migration did not roll back relay_providers.models")
	}
	var migrationCount int64
	if err := db.Table("schema_migrations").Where("version = ?", 8).Count(&migrationCount).Error; err != nil {
		t.Fatalf("count migration 0008: %v", err)
	}
	if migrationCount != 0 {
		t.Fatalf("migration 0008 record count = %d, want 0", migrationCount)
	}
}

func TestPostgresRelayModelsMigrationTrimsDeduplicatesAndSkipsBlankIDs(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	if err := db.Exec(`CREATE TABLE relay_providers (
		id BIGINT PRIMARY KEY,
		name TEXT NOT NULL,
		endpoint TEXT NOT NULL,
		api_key TEXT NOT NULL,
		api_format TEXT NOT NULL,
		config TEXT,
		models TEXT,
		enabled BOOLEAN DEFAULT TRUE,
		created_at TIMESTAMPTZ,
		updated_at TIMESTAMPTZ
	)`).Error; err != nil {
		t.Fatalf("create legacy relay_providers: %v", err)
	}
	if err := db.Exec(`INSERT INTO relay_providers (id, name, endpoint, api_key, api_format, models)
		VALUES (1, 'legacy', 'https://example.com', 'key', 'openai', '[" model-a ", "model-a", "   ", "model-b"]')`).Error; err != nil {
		t.Fatalf("insert legacy provider: %v", err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate whitespace model IDs: %v", err)
	}
	var models []string
	if err := db.Raw("SELECT model_id FROM relay_models WHERE provider_id = 1 ORDER BY model_id").Scan(&models).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(models, []string{"model-a", "model-b"}) {
		t.Fatalf("normalized models = %#v", models)
	}
}

func TestPostgresRelayProviderSequenceUsesAllHighWatermarks(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	if err := db.Exec(`CREATE TABLE relay_providers (
		id BIGINT PRIMARY KEY,
		name TEXT NOT NULL,
		endpoint TEXT NOT NULL,
		api_key TEXT NOT NULL,
		api_format TEXT NOT NULL,
		config TEXT,
		models TEXT,
		enabled BOOLEAN DEFAULT TRUE,
		created_at TIMESTAMPTZ,
		updated_at TIMESTAMPTZ
	)`).Error; err != nil {
		t.Fatalf("create legacy relay_providers: %v", err)
	}
	if err := db.Exec(`CREATE TABLE relay_request_logs (
		id BIGSERIAL PRIMARY KEY,
		user_id BIGINT NOT NULL,
		username TEXT NOT NULL,
		provider_id BIGINT,
		provider_name TEXT,
		api_type TEXT,
		model_id TEXT,
		category TEXT,
		operation TEXT NOT NULL,
		route TEXT NOT NULL,
		protocol TEXT NOT NULL,
		http_status BIGINT NOT NULL,
		upstream_status BIGINT,
		duration_ms BIGINT NOT NULL,
		request_bytes BIGINT NOT NULL,
		response_bytes BIGINT NOT NULL,
		error_type TEXT,
		created_at TIMESTAMPTZ NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create legacy relay_request_logs: %v", err)
	}
	if err := db.Exec(`INSERT INTO relay_request_logs
		(user_id, username, provider_id, operation, route, protocol, http_status, duration_ms, request_bytes, response_bytes, created_at)
		VALUES (1, 'user', 75, 'chat', '/relay/chat', 'openai', 200, 1, 1, 1, NOW())`).Error; err != nil {
		t.Fatalf("insert legacy relay log: %v", err)
	}
	if err := db.Exec("CREATE SEQUENCE relay_providers_id_seq").Error; err != nil {
		t.Fatalf("create existing provider sequence: %v", err)
	}
	if err := db.Exec("SELECT setval('relay_providers_id_seq', 100, false)").Error; err != nil {
		t.Fatalf("advance existing provider sequence: %v", err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate legacy high watermarks: %v", err)
	}
	var id int64
	if err := db.Raw("INSERT INTO relay_providers (name, endpoint, api_key, api_format) VALUES ('new', 'https://example.com', 'key', 'openai') RETURNING id").Scan(&id).Error; err != nil {
		t.Fatalf("insert provider using preserved sequence: %v", err)
	}
	if id != 100 {
		t.Fatalf("generated relay provider ID = %d, want 100", id)
	}
}

func TestPostgresConcurrentMigrationsAreLocked(t *testing.T) {
	db := pgtest.Open(t)
	const workers = 4
	start := make(chan struct{})
	errs := make(chan error, workers)
	var ready sync.WaitGroup
	ready.Add(workers)

	for range workers {
		go func() {
			ready.Done()
			<-start
			errs <- database.Migrate(context.Background(), db.Session(&gorm.Session{NewDB: true}))
		}()
	}
	ready.Wait()
	close(start)
	for range workers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent migration failed: %v", err)
		}
	}
	if err := database.ValidateSchema(context.Background(), db); err != nil {
		t.Fatalf("validate concurrently migrated schema: %v", err)
	}
}

func TestPostgresEnsureAdminSeedConcurrentFirstStart(t *testing.T) {
	db := pgtest.Open(t)
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- database.EnsureAdminSeed(context.Background(), db.Session(&gorm.Session{NewDB: true}), "100", "seed", "hash", database.NewSnowflakeGenerator(0))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent PostgreSQL seed failed: %v", err)
		}
	}
	var count int64
	if err := db.Model(&database.User{}).Where("phone = ? AND is_admin = ?", "100", true).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("admin seed count = %d, want 1", count)
	}
}
