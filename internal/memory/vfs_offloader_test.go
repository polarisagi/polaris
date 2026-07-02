package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/internal/vfs"
)

func TestToolRefOffloader_Offload_Success(t *testing.T) {
	// Setup DB with workspace_vfs table
	store := testutil.NewMockStore()
	db := store.DB()
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS workspace_vfs (
			id         TEXT PRIMARY KEY,
			task_id    TEXT NOT NULL,
			file_path  TEXT NOT NULL,
			size       INTEGER NOT NULL,
			meta       TEXT,
			created_at INTEGER NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("failed to setup db: %v", err)
	}

	// Setup WorkspaceManager
	tmpDir := t.TempDir()
	wm := vfs.NewWorkspaceManager(tmpDir, 50*1024*1024)

	offloader := NewToolRefOffloader(db, wm)

	// Offload content
	taskID := "task-123"
	content := []byte("large tool output content")
	ctx := context.Background()
	id, err := offloader.Offload(ctx, taskID, content)
	if err != nil {
		t.Fatalf("Offload failed: %v", err)
	}
	if id == "" {
		t.Fatalf("Offload returned empty id")
	}

	// Verify file was written
	relPath := filepath.Join(taskID, "tool_refs", id+".log")
	fullPath := filepath.Join(tmpDir, relPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("failed to read offloaded file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("file content mismatch, got %q, want %q", string(data), string(content))
	}

	// Verify DB record
	var dbFilePath string
	var dbSize int64
	err = db.QueryRow("SELECT file_path, size FROM workspace_vfs WHERE id = ?", id).Scan(&dbFilePath, &dbSize)
	if err != nil {
		t.Fatalf("failed to query db: %v", err)
	}
	if dbFilePath != relPath {
		t.Errorf("db file_path mismatch, got %q, want %q", dbFilePath, relPath)
	}
	if dbSize != int64(len(content)) {
		t.Errorf("db size mismatch, got %d, want %d", dbSize, len(content))
	}
}

func TestToolRefOffloader_Offload_Failure(t *testing.T) {
	// Use an uninitialized DB to force an error on insert
	store := testutil.NewMockStore()
	db := store.DB()

	tmpDir := t.TempDir()
	wm := vfs.NewWorkspaceManager(tmpDir, 50*1024*1024)

	offloader := NewToolRefOffloader(db, wm)

	taskID := "task-456"
	content := []byte("content that will fail to record in db")
	ctx := context.Background()
	id, err := offloader.Offload(ctx, taskID, content)
	if err == nil {
		t.Fatalf("expected offload to fail due to missing table")
	}
	if id != "" {
		t.Errorf("expected empty id on failure, got %q", id)
	}
}
