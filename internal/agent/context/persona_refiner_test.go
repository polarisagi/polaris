package agentctx

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestPersonaRefiner(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE preferences (key TEXT PRIMARY KEY, value TEXT)`)
	if err != nil {
		t.Fatal(err)
	}

	pr := NewPersonaRefiner(db)

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

	block := pr.PreferencesBlock()
	if !strings.Contains(block, "- language_pref: en") || !strings.Contains(block, "- expertise: expert") {
		t.Errorf("PreferencesBlock missing dims: %q", block)
	}
	if strings.Contains(block, "interaction_summary") {
		t.Errorf("PreferencesBlock must not include interaction_summary: %q", block)
	}
}

func TestPersonaRefiner_EmptyDims(t *testing.T) {
	pr := NewPersonaRefiner(nil)
	pr.Update(map[string]string{"language_pref": "", "response_style": "", "output_format": "", "expertise": ""})
	if got := pr.PreferencesBlock(); got != "" {
		t.Fatalf("all-empty dims should render empty, got %q", got)
	}
}
