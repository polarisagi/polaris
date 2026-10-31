package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func newAuditRepoTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE events (
		offset INTEGER PRIMARY KEY AUTOINCREMENT,
		id TEXT NOT NULL UNIQUE,
		topic TEXT NOT NULL,
		actor TEXT NOT NULL,
		type TEXT NOT NULL,
		payload BLOB NOT NULL,
		created_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLiteAuditRepository_ListAuditTailUsesRowIDCursor(t *testing.T) {
	db := newAuditRepoTestDB(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		if _, err := db.Exec(`INSERT INTO events(id,topic,actor,type,payload,created_at) VALUES(?, 'audit.policy', 'system', 'system', '{}', 1234)`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO events(id,topic,actor,type,payload,created_at) VALUES('other', 'agent.task', 'system', 'system', '{}', 1234)`); err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteAuditRepository(db, nil)
	ctx := context.Background()

	first, err := repository.ListAuditTail(ctx, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].ID != "d" || first[1].ID != "c" {
		t.Fatalf("first page must be descending physical order: %+v", first)
	}
	second, err := repository.ListAuditTail(ctx, 2, first[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].ID != "b" || second[1].ID != "a" {
		t.Fatalf("ID cursor must continue through same-millisecond rows: %+v", second)
	}
	if _, err := repository.ListAuditTail(ctx, 1, "missing"); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("missing cursor must fail explicitly, got %v", err)
	}
}

func TestSQLiteAuditRepository_GetAuditEventExactReadback(t *testing.T) {
	db := newAuditRepoTestDB(t)
	if _, err := db.Exec(`INSERT INTO events(id,topic,actor,type,payload,created_at) VALUES('audit-1', 'audit.policy', 'agent', 'system', '{"same":true}', 5678)`); err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteAuditRepository(db, nil)
	row, err := repository.GetAuditEvent(context.Background(), "audit-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != "audit-1" || row.Meta != `{"same":true}` || row.Actor != "agent" || row.Resource != "audit.policy" {
		t.Fatalf("unexpected exact row: %+v", row)
	}
	if _, err := repository.GetAuditEvent(context.Background(), "missing"); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("missing event must return CodeNotFound, got %v", err)
	}
}
