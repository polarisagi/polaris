package repo

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestGetPreference_ErrNoRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer db.Close()

	repo := NewSQLiteSystemRepository(db)

	mock.ExpectQuery("SELECT value FROM preferences WHERE key = \\?").
		WithArgs("nonexistent").
		WillReturnError(sql.ErrNoRows)

	val, err := repo.GetPreference(context.Background(), "nonexistent")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if val != "" {
		t.Errorf("expected empty string, got %v", val)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}
