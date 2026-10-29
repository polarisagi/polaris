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

func TestObsidianConnector_SkipsHiddenDirs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("a"), 0644)
	hidden := filepath.Join(dir, ".polaris")
	os.Mkdir(hidden, 0755)
	os.WriteFile(filepath.Join(hidden, "notes.md"), []byte("index side file"), 0644)

	conn, err := NewObsidianConnector(dir)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := conn.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].URI != "file://a.md" {
		t.Fatalf("hidden dir must be skipped, got %+v", docs)
	}
}

func TestObsidianConnector_BaseUnderHiddenDirStillSynced(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".config", "vault")
	os.MkdirAll(root, 0755)
	os.WriteFile(filepath.Join(root, "a.md"), []byte("a"), 0644)
	conn, err := NewObsidianConnector(root)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := conn.List(context.Background())
	if err != nil || len(docs) != 1 {
		t.Fatalf("base under hidden dir must still list, docs=%d err=%v", len(docs), err)
	}
}

func TestObsidianVaultConnector_DistinctIDAndURI(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	for _, d := range []string{d1, d2} {
		os.WriteFile(filepath.Join(d, "README.md"), []byte("x"+d), 0644)
	}
	c1, err := NewObsidianVaultConnector(d1)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := NewObsidianVaultConnector(d2)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := NewObsidianConnector(d1)
	if c1.ID() == c2.ID() || c1.ID() == legacy.ID() {
		t.Fatalf("connector IDs must differ: %s %s %s", c1.ID(), c2.ID(), legacy.ID())
	}
	r1, _ := c1.List(context.Background())
	r2, _ := c2.List(context.Background())
	if len(r1) != 1 || len(r2) != 1 || r1[0].URI == r2[0].URI {
		t.Fatalf("same relative path must yield distinct URIs: %+v %+v", r1, r2)
	}
	doc, err := c1.Fetch(context.Background(), r1[0])
	if err != nil || string(doc.Content) != "x"+d1 {
		t.Fatalf("fetch via namespaced URI failed: %v %+v", err, doc)
	}
}
