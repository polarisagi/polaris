package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store"
)

type mockSurreal struct {
	indexed   map[string]string
	vectors   map[string][]float32
	relations []string
}

func (m *mockSurreal) FTSIndex(id, text string) error {
	m.indexed[id] = text
	return nil
}
func (m *mockSurreal) GraphRelate(from, relation, to string, weight float64) error {
	m.relations = append(m.relations, from+"->"+relation+"->"+to)
	return nil
}
func (m *mockSurreal) VecUpsert(id string, vec []float32) error {
	m.vectors[id] = vec
	return nil
}

func TestExtensionLibrarianHandler(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE extension_instances (
			id TEXT PRIMARY KEY,
			name TEXT,
			publisher TEXT,
			install_path TEXT,
			config TEXT,
			meta TEXT
		)
	`)
	if err != nil {
		t.Fatal(err)
	}

	extDir := t.TempDir()
	os.WriteFile(filepath.Join(extDir, "README.md"), []byte("This is a test extension."), 0644)

	_, err = db.Exec(`
		INSERT INTO extension_instances (id, name, publisher, install_path, config, meta)
		VALUES ('ext123', 'test-ext', 'test-pub', ?, '{}', '{}')
	`, extDir)
	if err != nil {
		t.Fatal(err)
	}

	ms := &mockSurreal{
		indexed:   make(map[string]string),
		vectors:   make(map[string][]float32),
		relations: make([]string, 0),
	}

	llm := func(ctx context.Context, prompt string, opts ...types.InferOption) (string, error) {
		return `{"summary": "A test extension", "capabilities": ["cap1", "cap2"]}`, nil
	}
	embed := func(ctx context.Context, text string) ([]float32, error) {
		return []float32{0.1, 0.2}, nil
	}

	handler := NewExtensionLibrarianHandler(db, ms, llm, embed)

	payload, _ := json.Marshal(map[string]string{"extension_id": "ext123"})
	err = handler.Handle(context.Background(), &store.OutboxRecord{Payload: payload})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ms.indexed["ext_ext123"] == "" {
		t.Errorf("expected text to be indexed")
	}
	if len(ms.vectors["ext_ext123"]) == 0 {
		t.Errorf("expected vector to be upserted")
	}
	if len(ms.relations) != 2 {
		t.Errorf("expected 2 relations, got %d", len(ms.relations))
	}

	// Check DB update
	var meta string
	err = db.QueryRow("SELECT meta FROM extension_instances WHERE id = 'ext123'").Scan(&meta)
	if err != nil {
		t.Fatal(err)
	}
	if meta != `{"librarian_indexed":true}` {
		t.Errorf("expected meta to be updated, got %s", meta)
	}
}
