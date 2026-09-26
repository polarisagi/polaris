package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsPluginBundleRoot(t *testing.T) {
	tmpDir := t.TempDir()

	p, typ := isPluginBundleRoot(tmpDir)
	if p != "" || typ != "" {
		t.Errorf("expected empty, got %s, %s", p, typ)
	}

	manifestPath := filepath.Join(tmpDir, "plugin.json")
	os.WriteFile(manifestPath, []byte("{}"), 0644)

	p, typ = isPluginBundleRoot(tmpDir)
	if p != manifestPath || typ != "plugin.json" {
		t.Errorf("expected %s, plugin.json, got %s, %s", manifestPath, p, typ)
	}
}

func TestCond(t *testing.T) {
	if cond(true, "a", "b") != "a" {
		t.Errorf("expected a")
	}
	if cond(false, "a", "b") != "b" {
		t.Errorf("expected b")
	}
}
