package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultDataDir(t *testing.T) {
	dir, err := DefaultDataDir()
	if err != nil {
		t.Fatalf("DefaultDataDir failed: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir failed: %v", err)
	}
	expected := filepath.Join(home, DefaultDataDirRel)
	if dir != expected {
		t.Errorf("expected %s, got %s", expected, dir)
	}
}

func TestResolveDataDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	defaultDir := filepath.Join(home, DefaultDataDirRel)

	// 1. 无任何输入与环境变量，返回默认目录
	dir, err := ResolveDataDir("")
	if err != nil {
		t.Fatalf("ResolveDataDir failed: %v", err)
	}
	if dir != defaultDir {
		t.Errorf("expected %s, got %s", defaultDir, dir)
	}

	// 2. 传入 DefaultDataDirTilde
	dir, err = ResolveDataDir(DefaultDataDirTilde)
	if err != nil {
		t.Fatalf("ResolveDataDir failed: %v", err)
	}
	if dir != defaultDir {
		t.Errorf("expected %s, got %s", defaultDir, dir)
	}

	// 3. 传入 ~/custom
	dir, err = ResolveDataDir("~/custom/polaris_dir")
	if err != nil {
		t.Fatalf("ResolveDataDir failed: %v", err)
	}
	expectedCustom := filepath.Join(home, "custom/polaris_dir")
	if dir != expectedCustom {
		t.Errorf("expected %s, got %s", expectedCustom, dir)
	}

	// 4. 环境变量覆盖
	testEnvDir := filepath.Join(t.TempDir(), "env_dir")
	t.Setenv(EnvPolarisDataDir, testEnvDir)
	dir, err = ResolveDataDir("~/should_be_ignored")
	if err != nil {
		t.Fatalf("ResolveDataDir with env failed: %v", err)
	}
	if dir != testEnvDir {
		t.Errorf("expected %s, got %s", testEnvDir, dir)
	}
}

func TestDefaultDataLayout(t *testing.T) {
	layout, err := DefaultDataLayout()
	if err != nil {
		t.Fatalf("DefaultDataLayout failed: %v", err)
	}
	home, _ := os.UserHomeDir()
	expectedRoot := filepath.Join(home, DefaultDataDirRel)

	if layout.Root != expectedRoot {
		t.Errorf("expected root %s, got %s", expectedRoot, layout.Root)
	}
	if layout.KillSwitchFile != filepath.Join(expectedRoot, KillSwitchFileName) {
		t.Errorf("unexpected KillSwitchFile: %s", layout.KillSwitchFile)
	}
	if layout.VaultKeyFile != filepath.Join(expectedRoot, VaultKeyFileName) {
		t.Errorf("unexpected VaultKeyFile: %s", layout.VaultKeyFile)
	}
	if layout.SQLiteDB != filepath.Join(expectedRoot, SubdirData, SQLiteDBFileName) {
		t.Errorf("unexpected SQLiteDB: %s", layout.SQLiteDB)
	}
	if layout.CLISessionFile != filepath.Join(expectedRoot, CLISessionFileName) {
		t.Errorf("unexpected CLISessionFile: %s", layout.CLISessionFile)
	}
	if layout.EvalHoldout != filepath.Join(expectedRoot, SubdirEval, "holdout") {
		t.Errorf("unexpected EvalHoldout: %s", layout.EvalHoldout)
	}
}
