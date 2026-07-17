package database

import (
	"strings"
	"testing"
)

func TestEmbeddedMigrationsAreOrderedAndChecksummed(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 5 {
		t.Fatalf("migration count = %d, want 5", len(migrations))
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
