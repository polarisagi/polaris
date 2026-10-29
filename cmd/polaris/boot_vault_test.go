package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveVaultDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := filepath.Join(home, "ws")
	vault := filepath.Join(home, "Polaris", "Vault")
	for _, d := range []string{ws, vault} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := resolveVaultDirs([]string{
		"~/Polaris/Vault", vault + "/", ws + "/.", "~/missing", "",
	}, ws)
	if len(got) != 1 || got[0] != vault {
		t.Fatalf("want [%s], got %v", vault, got)
	}
}
