package repo

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/polarisagi/polaris/pkg/types"
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

// created_at 为空会让卸载幂等键失去代次区分度，Upsert 必须补真实时间。
func TestUpsertInstance_FillsEmptyTimestamps(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewSQLiteExtensionRepository(db)

	nonEmpty := &nonEmptyArg{}
	mock.ExpectExec("INSERT INTO extension_instances").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), nonEmpty, nonEmpty).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := r.UpsertInstance(context.Background(), types.ExtInstanceRow{ID: "ext_x"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

type nonEmptyArg struct{}

func (nonEmptyArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && s != ""
}
