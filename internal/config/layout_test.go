package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewDataLayout(t *testing.T) {
	root := "/test/root"
	overrides := DirsConfig{
		LogsDir:      "~/my_logs",
		DBDir:        "/custom/db",
		WorkspaceDir: "",
	}

	layout := NewDataLayout(root, overrides)

	if layout.Root != root {
		t.Errorf("Expected root %s, got %s", root, layout.Root)
	}
	if layout.Data != "/custom/db" {
		t.Errorf("Expected data /custom/db, got %s", layout.Data)
	}
	if layout.Workspace != "/test/root/workspace" {
		t.Errorf("Expected workspace /test/root/workspace, got %s", layout.Workspace)
	}

	home, _ := os.UserHomeDir()
	expectedLogs := filepath.Join(home, "my_logs")
	if layout.Logs != expectedLogs {
		t.Errorf("Expected logs %s, got %s", expectedLogs, layout.Logs)
	}

	// Test derived paths
	expectedSQLite := filepath.Join("/custom/db", "polaris.db")
	if layout.SQLiteDB != expectedSQLite {
		t.Errorf("Expected SQLiteDB %s, got %s", expectedSQLite, layout.SQLiteDB)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	tests := []struct {
		input    string
		expected string
	}{
		{"~/test", filepath.Join(home, "test")},
		{"/abs/path", "/abs/path"},
		{"rel/path", "rel/path"},
		{"~", "~"},
	}

	for _, test := range tests {
		actual := expandHome(test.input)
		if actual != test.expected {
			t.Errorf("expandHome(%q) expected %s, got %s", test.input, test.expected, actual)
		}
	}
}

func TestMkdirAll(t *testing.T) {
	tmpDir := t.TempDir()
	layout := NewDataLayout(tmpDir, DirsConfig{})

	err := layout.MkdirAll()
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	// Check some directories
	dirs := []string{
		layout.Data,
		layout.Logs,
		layout.ConfigPrompt,
	}

	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("Directory %s was not created", dir)
		} else if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}
}

func TestMigrate(t *testing.T) {
	tmpDir := t.TempDir()
	layout := NewDataLayout(tmpDir, DirsConfig{})

	// Create layout dirs so target data/logs dirs exist
	layout.MkdirAll()

	// Create fake old files
	oldSQLite := filepath.Join(tmpDir, "polaris.db")
	os.WriteFile(oldSQLite, []byte("db content"), 0644)

	oldLog := filepath.Join(tmpDir, "polaris.log")
	os.WriteFile(oldLog, []byte("log content"), 0644)

	oldSessions := filepath.Join(tmpDir, "transcripts")
	os.MkdirAll(oldSessions, 0755)

	// Since layout.MkdirAll created layout.Sessions, migrate will skip Sessions if dst exists.
	// Let's remove layout.Sessions to test directory migration
	os.RemoveAll(layout.Sessions)

	layout.Migrate()

	// Check if moved
	if _, err := os.Stat(oldSQLite); !os.IsNotExist(err) {
		t.Errorf("Old SQLite file was not moved")
	}
	if _, err := os.Stat(layout.SQLiteDB); err != nil {
		t.Errorf("New SQLite file not found")
	}

	if _, err := os.Stat(oldLog); !os.IsNotExist(err) {
		t.Errorf("Old log file was not moved")
	}
	if _, err := os.Stat(filepath.Join(layout.Logs, "polaris.log")); err != nil {
		t.Errorf("New log file not found")
	}

	if _, err := os.Stat(oldSessions); !os.IsNotExist(err) {
		t.Errorf("Old transcripts dir was not moved")
	}
	if _, err := os.Stat(layout.Sessions); err != nil {
		t.Errorf("New sessions dir not found")
	}
}
