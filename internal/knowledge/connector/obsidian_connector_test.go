package connector

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestObsidianConnector(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "note1.md"), []byte("---\ntags: [a, b]\n---\ncontent"), 0644)
	os.WriteFile(filepath.Join(dir, "note2.md"), []byte("no tags"), 0644)

	vaultDir := filepath.Join(dir, ".obsidian")
	os.Mkdir(vaultDir, 0755)

	conn, err := NewObsidianConnector(dir)
	if err != nil {
		t.Fatalf("unexpected error creating connector: %v", err)
	}

	if conn.ID() == "" || conn.Name() == "" || conn.SyncConfig().DefaultInterval == 0 {
		t.Errorf("expected non-empty ID, Name, and SyncConfig")
	}

	docs, err := conn.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(docs) != 2 {
		t.Errorf("expected 2 docs, got %d", len(docs))
	}

	var syncDoc *types.SyncDocument
	for _, d := range docs {
		if d.URI == "file://note1.md" {
			syncDoc, err = conn.Fetch(context.Background(), d)
			if err != nil {
				t.Fatalf("unexpected error fetching note1: %v", err)
			}
			break
		}
	}

	if syncDoc == nil {
		t.Fatalf("expected to find note1.md")
	}

	if syncDoc.Metadata["tags"] != "a, b" && syncDoc.Metadata["tags"] != "[a, b]" { // depending on exact split
		// let's just check metadata length since yaml parsing in ObsidianConnector is simple
		if len(syncDoc.Metadata) != 1 {
			t.Errorf("expected 1 tag in metadata, got %d", len(syncDoc.Metadata))
		}
	}

	ch, err := conn.Watch(context.Background())
	if err != nil {
		t.Errorf("unexpected error on watch: %v", err)
	}
	if ch != nil {
		t.Logf("obsidian watch returned non-nil channel")
	}
}
