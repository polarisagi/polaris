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

func TestSafeJoin(t *testing.T) {
	tmpDir := t.TempDir()
	var err error
	tmpDir, err = filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	// Safe join
	res, ok := safeJoin(tmpDir, "some/file.txt")
	if !ok || res != filepath.Join(tmpDir, "some/file.txt") {
		t.Errorf("expected success and correct path: %s", res)
	}

	// Unsafe join (path traversal) - actually gets contained within base
	res, ok = safeJoin(tmpDir, "../outside.txt")
	if !ok || res != filepath.Join(tmpDir, "outside.txt") {
		t.Errorf("expected to be contained: %s", res)
	}

	// Unsafe join (absolute path)
	res, ok = safeJoin(tmpDir, "/etc/passwd")
	// filepath.Clean("/" + "/etc/passwd") -> "/etc/passwd", then joined with tmpDir -> tmpDir/etc/passwd
	// It is safe because we strip the leading slash logically
	if !ok || res != filepath.Join(tmpDir, "etc/passwd") {
		t.Errorf("expected success for absolute path resolution: %s", res)
	}
}
