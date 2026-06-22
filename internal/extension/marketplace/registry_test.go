package marketplace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()

	plugin1 := &Plugin{
		Manifest: protocol.PluginJSON{Name: "p1"},
		Enabled:  true,
	}

	err := r.Register(plugin1)
	if err != nil {
		t.Fatal(err)
	}

	err = r.Register(plugin1)
	if err == nil {
		t.Fatal("expected error on duplicate register")
	}

	p, ok := r.Get("p1")
	if !ok || p.Manifest.Name != "p1" {
		t.Errorf("expected to get p1")
	}

	enabled := r.ListEnabled()
	if len(enabled) != 1 || enabled[0].Manifest.Name != "p1" {
		t.Errorf("unexpected enabled list: %+v", enabled)
	}

	err = r.SetEnabled("p1", false)
	if err != nil {
		t.Fatal(err)
	}

	enabled = r.ListEnabled()
	if len(enabled) != 0 {
		t.Errorf("expected empty enabled list, got %d", len(enabled))
	}

	err = r.SetEnabled("missing", true)
	if err == nil {
		t.Fatal("expected error on missing plugin")
	}

	r.Unregister("p1")
	_, ok = r.Get("p1")
	if ok {
		t.Errorf("expected p1 to be unregistered")
	}
}

func TestRegistry_ScanDir(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry()

	// Empty dir
	count, err := r.ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	// Missing dir
	count, err = r.ScanDir(filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	// Valid plugin dir
	p1Dir := filepath.Join(dir, "p1")
	os.MkdirAll(filepath.Join(p1Dir, ".polaris-plugin"), 0755)
	os.WriteFile(filepath.Join(p1Dir, ".polaris-plugin", "plugin.json"), []byte(`{"name":"p1"}`), 0644)

	count, err = r.ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}

	// Duplicate scan
	count, err = r.ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 { // Should skip duplicate
		t.Errorf("expected 0, got %d", count)
	}
}

func TestDefaultScanPaths(t *testing.T) {
	paths := DefaultScanPaths("/data")
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}
	if paths[0] != filepath.Join("/data", "extensions") {
		t.Errorf("unexpected path 0: %s", paths[0])
	}
}
