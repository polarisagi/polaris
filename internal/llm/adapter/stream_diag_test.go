package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewRawDumpSecuresExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "diagnostics")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dump := newRawDump(dir, &OpenAIRequest{Model: "test-model"})
	if dump == nil {
		t.Fatal("expected a dump in a writable directory")
	}
	defer dump.Close()

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode: want 0700, got %04o", got)
	}
	fileInfo, err := os.Stat(dump.f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("dump mode: want 0600, got %04o", got)
	}
}

func TestNewRawDumpDisablesInvalidPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if dump := newRawDump(file, &OpenAIRequest{Model: "test-model"}); dump != nil {
		t.Fatal("expected dump to be disabled for a file path")
	}
}
