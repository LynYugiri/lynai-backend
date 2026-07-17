package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const migrationLockID int64 = 0x4c796e41494d6967

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  int64
	name     string
	checksum string
	sql      string
}

type appliedMigration struct {
	Version  int64  `gorm:"primaryKey"`
	Checksum string `gorm:"not null;size:64"`
}

// Connect opens a PostgreSQL connection without changing its schema.
func Connect(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}

	return db, nil
}

// Migrate applies every embedded PostgreSQL migration under an advisory lock.
func Migrate(ctx context.Context, db *gorm.DB) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("database handle: %w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID)

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
version BIGINT PRIMARY KEY,
name TEXT NOT NULL,
checksum VARCHAR(64) NOT NULL,
applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := readApplied(ctx, conn)
	if err != nil {
		return err
	}
	if err := checkApplied(migrations, applied, false); err != nil {
		return err
	}
	for _, migration := range migrations {
		if _, ok := applied[migration.version]; ok {
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", migration.version, err)
		}
		if _, err = tx.ExecContext(ctx, migration.sql); err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)", migration.version, migration.name, migration.checksum)
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %04d_%s: %w", migration.version, migration.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", migration.version, err)
		}
	}
	return nil
}

// ValidateSchema verifies that the database exactly matches the known migrations.
func ValidateSchema(ctx context.Context, db *gorm.DB) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	var exists bool
	if err := db.WithContext(ctx).Raw("SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists).Error; err != nil {
		return fmt.Errorf("check schema_migrations: %w", err)
	}
	if !exists {
		return errors.New("database is not migrated; run `lynai-backend migrate`")
	}
	rows, err := db.WithContext(ctx).Raw("SELECT version, checksum FROM schema_migrations ORDER BY version").Rows()
	if err != nil {
		return fmt.Errorf("read schema migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int64]string{}
	for rows.Next() {
		var row appliedMigration
		if err := rows.Scan(&row.Version, &row.Checksum); err != nil {
			return fmt.Errorf("scan schema migration: %w", err)
		}
		applied[row.Version] = row.Checksum
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schema migrations: %w", err)
	}
	return checkApplied(migrations, applied, true)
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	result := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		parts := strings.SplitN(strings.TrimSuffix(entry.Name(), ".sql"), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid migration version %q: %w", parts[0], err)
		}
		contents, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		digest := sha256.Sum256(contents)
		result = append(result, migration{version: version, name: parts[1], checksum: hex.EncodeToString(digest[:]), sql: string(contents)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].version < result[j].version })
	for i := 1; i < len(result); i++ {
		if result[i-1].version == result[i].version {
			return nil, fmt.Errorf("duplicate migration version %d", result[i].version)
		}
	}
	return result, nil
}

func readApplied(ctx context.Context, conn *sql.Conn) (map[int64]string, error) {
	rows, err := conn.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read schema migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int64]string{}
	for rows.Next() {
		var version int64
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema migration: %w", err)
		}
		applied[version] = checksum
	}
	return applied, rows.Err()
}

func checkApplied(migrations []migration, applied map[int64]string, requireCurrent bool) error {
	known := make(map[int64]migration, len(migrations))
	for _, migration := range migrations {
		known[migration.version] = migration
	}
	for version, checksum := range applied {
		migration, ok := known[version]
		if !ok {
			return fmt.Errorf("database has unknown migration version %d", version)
		}
		if checksum != migration.checksum {
			return fmt.Errorf("migration %d checksum mismatch", version)
		}
	}
	missing := false
	for _, migration := range migrations {
		_, ok := applied[migration.version]
		if !ok {
			missing = true
			continue
		}
		if missing {
			return fmt.Errorf("database migration history is not contiguous before version %d", migration.version)
		}
	}
	if requireCurrent && len(applied) != len(migrations) {
		return fmt.Errorf("database schema is behind: applied %d of %d migrations; run `lynai-backend migrate`", len(applied), len(migrations))
	}
	return nil
}
