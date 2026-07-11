package consolidation

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/vfs"
	"github.com/polarisagi/polaris/pkg/types"

	_ "github.com/mattn/go-sqlite3"
)

func TestSemanticCompressHandler(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE workspace_vfs (
			id TEXT PRIMARY KEY,
			file_path TEXT,
			size INTEGER,
			meta TEXT
		)
	`)
	if err != nil {
		t.Fatal(err)
	}

	vfsDir := t.TempDir()
	os.WriteFile(filepath.Join(vfsDir, "error.log"), []byte("Exception: SegFault at 0x0"), 0644)

	_, err = db.Exec(`
		INSERT INTO workspace_vfs (id, file_path, size, meta)
		VALUES ('vfs123', 'error.log', 28, '{}')
	`)
	if err != nil {
		t.Fatal(err)
	}

	llm := LLMInferFunc(func(ctx context.Context, prompt string, opts ...types.InferOption) (string, error) {
		return `{"root_cause": "SegFault", "error_type": "Memory", "suggest_fix": "Fix C", "affected_file": "error.log"}`, nil
	})

	wm := vfs.NewWorkspaceManager(vfsDir, 1024*1024)
	handler := NewSemanticCompressHandler(db, llm, wm)

	payload, _ := json.Marshal(map[string]string{"vfs_id": "vfs123"})
	err = handler.Handle(context.Background(), &store.OutboxRecord{Payload: payload})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify DB update
	var size int
	var meta string
	err = db.QueryRow("SELECT size, meta FROM workspace_vfs WHERE id = 'vfs123'").Scan(&size, &meta)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(meta, "semantic_compressed") {
		t.Errorf("meta not updated: %s", meta)
	}

	// Verify file
	data, _ := os.ReadFile(filepath.Join(vfsDir, "error.log"))
	if !strings.Contains(string(data), "root_cause") {
		t.Errorf("file not updated: %s", string(data))
	}
}
