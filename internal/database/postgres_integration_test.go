package database_test

import (
	"context"
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
	if count != 5 {
		t.Fatalf("applied migration count = %d, want 5", count)
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
