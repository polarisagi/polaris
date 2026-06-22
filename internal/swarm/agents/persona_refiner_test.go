package agents

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockProvider struct{}

func (m *mockProvider) Infer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: "A detailed user."}, nil
}
func (m *mockProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
func (m *mockProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *mockProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (m *mockProvider) ModelID() string { return "mock" }
func (m *mockProvider) Close() error    { return nil }

func TestPersonaRefiner(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE preferences (key TEXT PRIMARY KEY, value TEXT)`)
	if err != nil {
		t.Fatal(err)
	}

	pr := NewPersonaRefiner(db, &mockProvider{})

	// Load empty
	err = pr.Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}

	// Update
	pr.Update(map[string]string{
		"language_pref": "en",
		"expertise":     "expert",
	})

	prof := pr.Profile()
	if prof.LanguagePref != "en" || prof.Expertise != "expert" {
		t.Errorf("update failed: %+v", prof)
	}

	// Save
	err = pr.Save(context.Background())
	if err != nil {
		t.Fatalf("unexpected save error: %v", err)
	}

	// Refine
	err = pr.RefineAtSessionEnd(context.Background(), []types.Message{
		{Role: "user", Content: "Hello"},
	})
	if err != nil {
		t.Fatalf("unexpected refine error: %v", err)
	}

	prof = pr.Profile()
	if prof.InteractionSummary != "A detailed user." {
		t.Errorf("refine failed: %s", prof.InteractionSummary)
	}

	// ToUserPreferences
	prefs := pr.ToUserPreferences()
	if len(prefs) != 5 {
		t.Errorf("expected 5 prefs, got %d", len(prefs))
	}
}

func TestPersonaRefiner_NilProvider(t *testing.T) {
	pr := NewPersonaRefiner(nil, nil)
	err := pr.RefineAtSessionEnd(context.Background(), []types.Message{{Role: "user", Content: "x"}})
	if err != nil {
		t.Fatalf("expected nil")
	}
}
