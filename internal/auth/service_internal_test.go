package auth

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

func TestDuplicateKeyDetection(t *testing.T) {
	for _, err := range []error{
		&pgconn.PgError{Code: "23505", ConstraintName: "idx_users_phone"},
		errors.New("UNIQUE constraint failed: users.phone"),
	} {
		if !isPhoneUniqueViolation(err) {
			t.Fatalf("isPhoneUniqueViolation(%v) = false", err)
		}
	}
	for _, err := range []error{
		gorm.ErrDuplicatedKey,
		&pgconn.PgError{Code: "23505", ConstraintName: "users_pkey"},
		errors.New("UNIQUE constraint failed: users.id"),
		errors.New("connection lost"),
	} {
		if isPhoneUniqueViolation(err) {
			t.Fatalf("isPhoneUniqueViolation(%v) = true", err)
		}
	}
}
