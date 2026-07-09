package repo

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDeleteOrphanCatalogEntries_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer db.Close()

	repo := NewSQLiteExtensionRepository(db)

	mock.ExpectExec("DELETE FROM extension_catalog WHERE marketplace_id != 'builtin' AND marketplace_id NOT IN \\(\\?\\)").
		WithArgs("test_marketplace").
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = repo.DeleteOrphanCatalogEntries(context.Background(), []any{"test_marketplace"})
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestDeleteOrphanCatalogEntries_EmptySuccess(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer db.Close()

	repo := NewSQLiteExtensionRepository(db)

	mock.ExpectExec("DELETE FROM extension_catalog WHERE marketplace_id != 'builtin'").
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = repo.DeleteOrphanCatalogEntries(context.Background(), []any{})
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}
