package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	_ "github.com/mattn/go-sqlite3"
)

func TestSQLNotesStore(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS notes (
			id TEXT PRIMARY KEY,
			key TEXT UNIQUE,
			content TEXT,
			version INTEGER,
			size_bytes INTEGER,
			tags_json TEXT,
			created_at INTEGER,
			updated_at INTEGER,
			expires_at INTEGER
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	store := NewSQLNotesStore(db)
	ctx := context.Background()

	// Test Set (Insert)
	err = store.Set(ctx, "test_key", "test content", []string{"tag1", "tag2"}, -1)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Test Get
	note, err := store.Get(ctx, "test_key")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if note == nil || note.Content != "test content" {
		t.Errorf("Unexpected Get result: %v", note)
	}

	// Test Set (Update with CAS)
	err = store.Set(ctx, "test_key", "updated content", []string{"tag1"}, note.Version)
	if err != nil {
		t.Fatalf("Set (CAS) failed: %v", err)
	}

	// Test CAS Conflict
	err = store.Set(ctx, "test_key", "fail content", nil, note.Version)
	if err == nil {
		t.Errorf("Expected CAS conflict error, got nil")
	}

	// Test List
	notes, err := store.List(ctx, "")
	if err != nil || len(notes) != 1 {
		t.Fatalf("List failed: %v, len: %d", err, len(notes))
	}

	// Test List with tag
	notes, err = store.List(ctx, "tag1")
	if err != nil || len(notes) != 1 {
		t.Fatalf("List with tag failed: %v, len: %d", err, len(notes))
	}

	notes, err = store.List(ctx, "nonexistent")
	if err != nil || len(notes) != 0 {
		t.Fatalf("List nonexistent tag failed: %v, len: %d", err, len(notes))
	}

	// Test ListByTask
	err = store.Set(ctx, "task_note", "task content", []string{"task:123"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	notes, err = store.ListByTask(ctx, "123")
	if err != nil || len(notes) != 1 || notes[0].Content != "task content" {
		t.Fatalf("ListByTask failed")
	}

	// Test Delete
	err = store.Delete(ctx, "test_key")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	note, _ = store.Get(ctx, "test_key")
	if note != nil {
		t.Errorf("Expected note to be deleted")
	}

	// Test GC
	_, err = db.Exec("UPDATE notes SET expires_at = ? WHERE key = ?", time.Now().Unix()-1000, "task_note")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := store.GC(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("GC failed: %v, removed: %d", err, removed)
	}
}

func TestInMemNotesStore(t *testing.T) {
	store := NewInMemNotesStore()
	ctx := context.Background()

	// Test Set
	err := store.Set(ctx, "key1", "content1", []string{"t1"}, -1)
	if err != nil {
		t.Fatal(err)
	}

	// Test Get
	n, err := store.Get(ctx, "key1")
	if err != nil || n == nil || n.Content != "content1" {
		t.Fatalf("Get failed")
	}

	// Test Update CAS
	err = store.Set(ctx, "key1", "content2", []string{"t2"}, n.Version)
	if err != nil {
		t.Fatal(err)
	}

	// Test Conflict
	err = store.Set(ctx, "key1", "fail", nil, n.Version)
	if err == nil {
		t.Fatal("Expected conflict")
	}

	// Test List
	list, _ := store.List(ctx, "")
	if len(list) != 1 {
		t.Fatalf("List failed")
	}

	list, _ = store.List(ctx, "t2")
	if len(list) != 1 {
		t.Fatalf("List with tag failed")
	}

	// Test ListByTask
	_ = store.Set(ctx, "task2", "task", []string{"task:456"}, -1)
	list, _ = store.ListByTask(ctx, "456")
	if len(list) != 1 {
		t.Fatalf("ListByTask failed")
	}

	// Test Delete
	_ = store.Delete(ctx, "key1")
	n, _ = store.Get(ctx, "key1")
	if n != nil {
		t.Fatal("Should be deleted")
	}

	// Test GC
	store.notes["expired"] = &types.Note{
		ExpiresAt: func() *time.Time { t := time.Now().Add(-time.Hour); return &t }(),
	}
	store.GC(ctx)
	if len(store.notes) != 1 { // task2 still exists
		t.Fatal("GC failed")
	}
}
