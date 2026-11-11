package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateDatasetInProject(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "dataset.json")
	if err := os.WriteFile(inside, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateDatasetInProject(inside, root); err != nil {
		t.Fatalf("dataset under project root rejected: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "dataset.json")
	if err := os.WriteFile(outside, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateDatasetInProject(outside, root); err == nil {
		t.Fatal("dataset outside explicit project root must be rejected")
	}
}
