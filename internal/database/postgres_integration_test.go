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
	if count != 11 {
		t.Fatalf("applied migration count = %d, want 11", count)
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
	for _, table := range []string{"sync_records", "sync_record_blobs", "sync_management_operations"} {
		var exists bool
		if err := db.Raw("SELECT to_regclass(?) IS NOT NULL", table).Scan(&exists).Error; err != nil {
			t.Fatalf("look up table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("embedded migrations did not create table %s", table)
		}
	}
}

func TestPostgresSyncProjectionMigrationClearsLegacySyncState(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	if err := database.MigrateToForTest(ctx, db, 8); err != nil {
		t.Fatalf("apply migrations through 0008: %v", err)
	}
	if err := db.Exec(`INSERT INTO sync_metadata (user_id, last_seq, updated_at) VALUES (42, 1, NOW());
		INSERT INTO sync_changes (user_id, seq, table_name, op, record_id, data, created_at, change_id, client_created_at)
		VALUES (42, 1, 'notes', 'upsert', 'note-1', '{"id":"note-1"}', NOW(), 'legacy-change', NOW());
		INSERT INTO sync_blobs (user_id, sha256, size, created_at) VALUES (42, repeat('a', 64), 1, NOW());
		INSERT INTO sync_request_replays (user_id, request_id, operation, body_hash, response_status, response_content_type, response_body, created_at, expires_at)
		VALUES (42, repeat('A', 32), 'POST /sync/changes', decode(repeat('00', 32), 'hex'), 200, 'application/json', '{}'::bytea, NOW(), NOW() + interval '1 hour')`).Error; err != nil {
		t.Fatalf("seed legacy sync state: %v", err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("apply sync projection migrations: %v", err)
	}
	for _, table := range []string{"sync_metadata", "sync_changes", "sync_blobs", "sync_request_replays", "sync_records"} {
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0", table, count)
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
	if err := db.Raw("INSERT INTO relay_providers (name, endpoint, api_format) VALUES ('new', 'https://example.com', 'openai') RETURNING id").Scan(&id).Error; err != nil {
		t.Fatalf("insert provider using default ID: %v", err)
	}
	if id != 42 {
		t.Fatalf("generated relay provider ID = %d, want 42", id)
	}
	var models []string
	if err := db.Raw(`SELECT model.model_id
		FROM relay_models AS model
		JOIN relay_model_bindings AS binding ON binding.relay_model_id = model.id
		WHERE binding.provider_id = 41 ORDER BY model.model_id`).Scan(&models).Error; err != nil {
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
	if err := db.Raw(`SELECT model.model_id
		FROM relay_models AS model
		JOIN relay_model_bindings AS binding ON binding.relay_model_id = model.id
		WHERE binding.provider_id = 1 ORDER BY model.model_id`).Scan(&models).Error; err != nil {
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
	if err := db.Raw("INSERT INTO relay_providers (name, endpoint, api_format) VALUES ('new', 'https://example.com', 'openai') RETURNING id").Scan(&id).Error; err != nil {
		t.Fatalf("insert provider using preserved sequence: %v", err)
	}
	if id != 100 {
		t.Fatalf("generated relay provider ID = %d, want 100", id)
	}
}

func TestPostgresRelayRoutingUpgrade(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	if err := database.MigrateToForTest(ctx, db, 10); err != nil {
		t.Fatalf("apply migrations through 0010: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO users (id, phone, password_hash, display_name, is_admin, disabled, created_at, updated_at)
		VALUES (1, '100', 'hash', 'user', FALSE, FALSE, NOW(), NOW());
		INSERT INTO relay_providers (id, name, endpoint, api_key, api_format, config, enabled, created_at, updated_at)
		VALUES
			(11, 'first', 'https://first.example', 'first-secret', 'openai', '{"region":"a"}', TRUE, NOW() - interval '2 days', NOW() - interval '1 day'),
			(12, 'second', 'https://second.example', '', 'ollama', '{}', FALSE, NOW(), NOW());
		INSERT INTO relay_models (id, provider_id, model_id, display_name, description, category, capabilities, advanced_params, enabled, created_at, updated_at)
		VALUES
			(101, 11, 'shared', 'Earliest', 'first metadata', '', '{"vision":true}', '{}', FALSE, NOW() - interval '2 days', NOW() - interval '1 day'),
			(102, 12, 'shared', 'Later', 'later metadata', 'chat', '{}', '{}', TRUE, NOW(), NOW()),
			(150, 12, 'local', 'Local', '', 'chat', '{}', '{}', TRUE, NOW(), NOW());
		INSERT INTO relay_speech_sessions (id, user_id, provider_id, model_id, app_id, upstream_audio_id, task_id, expires_at, created_at, updated_at)
		VALUES ('AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', 1, 11, 'shared', 'app', 'audio', 'task', NOW() + interval '1 hour', NOW(), NOW());
		INSERT INTO relay_request_logs (user_id, username, provider_id, provider_name, operation, route, protocol, http_status, duration_ms, request_bytes, response_bytes, created_at)
		VALUES (1, 'user', 11, 'first', 'chat', '/relay/chat', 'openai', 200, 1, 2, 3, NOW())`).Error; err != nil {
		t.Fatalf("seed relay routing upgrade: %v", err)
	}

	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("apply relay routing migration: %v", err)
	}
	if err := database.ValidateSchema(ctx, db); err != nil {
		t.Fatalf("validate relay routing schema: %v", err)
	}

	var apiKeyColumn bool
	if err := db.Raw(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'relay_providers' AND column_name = 'api_key'
	)`).Scan(&apiKeyColumn).Error; err != nil {
		t.Fatal(err)
	}
	if apiKeyColumn {
		t.Fatal("relay_providers.api_key still exists")
	}

	type modelRow struct {
		ID          int64
		DisplayName string
		Category    string
		Enabled     bool
	}
	var shared modelRow
	if err := db.Raw("SELECT id, display_name, category, enabled FROM relay_models WHERE model_id = 'shared'").Scan(&shared).Error; err != nil {
		t.Fatal(err)
	}
	if shared.ID != 101 || shared.DisplayName != "Earliest" || shared.Category != "chat" || !shared.Enabled {
		t.Fatalf("global shared model = %+v", shared)
	}

	var bindingCount, credentialCount int64
	if err := db.Table("relay_model_bindings").Count(&bindingCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("relay_provider_credentials").Count(&credentialCount).Error; err != nil {
		t.Fatal(err)
	}
	if bindingCount != 3 || credentialCount != 2 {
		t.Fatalf("binding count = %d, credential count = %d", bindingCount, credentialCount)
	}

	type sessionRow struct {
		BindingID      int64
		CredentialID   int64
		UpstreamModel  string
		Endpoint       string
		APIFormat      string
		ConfigSnapshot string
	}
	var session sessionRow
	if err := db.Raw(`SELECT binding_id, credential_id, upstream_model, endpoint, api_format, config_snapshot
		FROM relay_speech_sessions WHERE id = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'`).Scan(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.BindingID == 0 || session.CredentialID == 0 || session.UpstreamModel != "shared" ||
		session.Endpoint != "https://first.example" || session.APIFormat != "openai" || session.ConfigSnapshot != `{"region":"a"}` {
		t.Fatalf("migrated speech session = %+v", session)
	}

	var credential database.RelayProviderCredential
	if err := db.Where("provider_id = ?", 12).First(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if credential.Name != "Default" || credential.APIKey != "" || !credential.Enabled || credential.Config != "{}" {
		t.Fatalf("Ollama default credential = %+v", credential)
	}

	var nextModelID int64
	if err := db.Raw("INSERT INTO relay_models (model_id) VALUES ('next') RETURNING id").Scan(&nextModelID).Error; err != nil {
		t.Fatal(err)
	}
	if nextModelID <= 150 {
		t.Fatalf("next relay model ID = %d, want > 150", nextModelID)
	}

	if err := db.Exec("DELETE FROM relay_providers WHERE id = 11").Error; err == nil {
		t.Fatal("provider deletion ignored existing speech session")
	}
	if err := db.Exec("DELETE FROM relay_model_bindings WHERE id = ?", session.BindingID).Error; err == nil {
		t.Fatal("binding deletion ignored existing speech session")
	}
	if err := db.Exec("DELETE FROM relay_provider_credentials WHERE id = ?", session.CredentialID).Error; err == nil {
		t.Fatal("credential deletion ignored existing speech session")
	}
}

func TestPostgresRelayRoutingRejectsCategoryConflictsAndRollsBack(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	if err := database.MigrateToForTest(ctx, db, 10); err != nil {
		t.Fatalf("apply migrations through 0010: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO relay_providers (id, name, endpoint, api_key, api_format, enabled)
		VALUES (1, 'one', 'https://one.example', 'secret', 'openai', TRUE),
		       (2, 'two', 'https://two.example', 'secret', 'openai', TRUE);
		INSERT INTO relay_models (provider_id, model_id, category)
		VALUES (1, 'conflict', ''), (2, 'conflict', 'embedding')`).Error; err != nil {
		t.Fatalf("seed conflicting relay models: %v", err)
	}

	if err := database.Migrate(ctx, db); err == nil {
		t.Fatal("migration accepted conflicting normalized categories")
	}
	var apiKeyColumn, providerIDColumn bool
	if err := db.Raw(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'relay_providers' AND column_name = 'api_key'
	)`).Scan(&apiKeyColumn).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'relay_models' AND column_name = 'provider_id'
	)`).Scan(&providerIDColumn).Error; err != nil {
		t.Fatal(err)
	}
	if !apiKeyColumn || !providerIDColumn {
		t.Fatalf("failed migration did not roll back: api_key=%t provider_id=%t", apiKeyColumn, providerIDColumn)
	}
	var migrationCount int64
	if err := db.Table("schema_migrations").Where("version = ?", 11).Count(&migrationCount).Error; err != nil {
		t.Fatal(err)
	}
	if migrationCount != 0 {
		t.Fatalf("migration 0011 record count = %d, want 0", migrationCount)
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
