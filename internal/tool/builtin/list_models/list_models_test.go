package list_models

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
)

type mockSwitcher struct {
	current    string
	userLocked string
}

func (m *mockSwitcher) GetModelStatus() (string, string) {
	return m.current, m.userLocked
}

func (m *mockSwitcher) SwitchModel(_, _, _ string) (string, error) {
	return "", nil
}

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE providers (id TEXT, name TEXT, type TEXT, enabled INTEGER, is_local INTEGER DEFAULT 0);
		CREATE TABLE provider_models (id TEXT, provider_id TEXT, model_id TEXT, name TEXT, role TEXT, enabled INTEGER);
		CREATE TABLE provider_discovered_models (
			provider_id TEXT, model_id TEXT, display_name TEXT,
			context_window INTEGER, max_output INTEGER,
			supports_thinking INTEGER, supports_vision INTEGER, supports_tools INTEGER,
			input_price_per_mtok REAL, output_price_per_mtok REAL
		);

		INSERT INTO providers VALUES ('prov_openai', 'OpenAI', 'openai_compat', 1, 0);
		INSERT INTO providers VALUES ('prov_ollama', 'Local Ollama', 'ollama', 1, 1);

		INSERT INTO provider_models VALUES ('m1', 'prov_openai', 'gpt-4o', 'GPT-4o', 'general', 1);
		INSERT INTO provider_discovered_models VALUES (
			'prov_openai', 'gpt-4o', 'GPT-4o Real',
			128000, 4096, 0, 1, 1, 2.5, 10.0
		);

		INSERT INTO provider_models VALUES ('m2', 'prov_ollama', 'llama3', 'Llama 3 8B', 'default', 1);
	`)
	if err != nil {
		t.Fatalf("failed to setup db: %v", err)
	}
	return db
}

func TestListModels_Success(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	fn := MakeListModelsFn(db, nil)
	switcher := &mockSwitcher{current: "prov_openai/gpt-4o", userLocked: "prov_openai/gpt-4o"}
	ctx := context.WithValue(context.Background(), protocol.CtxModelSwitcherKey{}, switcher)

	outBytes, err := fn(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res struct {
		Models       []ModelInfo `json:"models"`
		Count        int         `json:"count"`
		LockedByUser bool        `json:"locked_by_user"`
		CurrentModel string      `json:"current_model"`
	}
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if res.Count != 2 {
		t.Fatalf("expected 2 models, got %d", res.Count)
	}
	if !res.LockedByUser {
		t.Fatalf("expected locked_by_user=true")
	}

	// 验证能力的合并
	var gpt4o, llama3 *ModelInfo
	for i := range res.Models {
		switch res.Models[i].Ref {
		case "prov_openai/gpt-4o":
			gpt4o = &res.Models[i]
		case "prov_ollama/llama3":
			llama3 = &res.Models[i]
		}
	}

	if gpt4o == nil || llama3 == nil {
		t.Fatalf("missing expected models in list")
	}

	if !gpt4o.CurrentModel || !gpt4o.LockedByUser {
		t.Errorf("gpt4o status flags mismatch: %+v", gpt4o)
	}
	if gpt4o.ContextWindow != 128000 || !gpt4o.SupportsVision || !gpt4o.SupportsTools {
		t.Errorf("gpt4o capabilities mismatch: %+v", gpt4o)
	}
	if gpt4o.IsLocal {
		t.Errorf("gpt4o should not be local")
	}

	if !llama3.IsLocal {
		t.Errorf("llama3 should be local")
	}
}

func TestListModels_QueryFilter(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	fn := MakeListModelsFn(db, nil)
	ctx := context.Background()

	outBytes, err := fn(ctx, []byte(`{"query":"ollama"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res struct {
		Models []ModelInfo `json:"models"`
		Count  int         `json:"count"`
	}
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if res.Count != 1 || res.Models[0].Ref != "prov_ollama/llama3" {
		t.Fatalf("expected 1 ollama model, got %+v", res)
	}
}
