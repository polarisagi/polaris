package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyKernelIntegrity_ReleaseMode(t *testing.T) {
	// Create a temporary directory and change to it to ensure ImmutableKernelPackages() directories don't exist
	tmpDir := t.TempDir()
	originalWD, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(originalWD)

	// In release mode, ImmutableKernelPackages() directories don't exist.
	// It calls verifyBinarySeal()
	// Let's create a seal file for the current executable to make verifyBinarySeal pass.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Failed to get executable: %v", err)
	}

	// Create sidecar seal
	sidecar := exe + ".sha256"

	f, err := os.Open(exe)
	if err == nil {
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			expectedHash := hex.EncodeToString(h.Sum(nil))
			os.WriteFile(sidecar, []byte(expectedHash), 0644)
			defer os.Remove(sidecar) // Cleanup after test
		}
	}

	// It should pass because release mode is triggered and seal matches (if it exists)
	err = VerifyKernelIntegrity()
	if err != nil {
		t.Errorf("Expected nil error in release mode with valid seal, got: %v", err)
	}

	// Test with invalid seal
	os.WriteFile(sidecar, []byte("invalidhash"), 0644)
	err = VerifyKernelIntegrity()
	if err == nil || !strings.Contains(err.Error(), "binary seal mismatch") {
		t.Errorf("Expected binary seal mismatch error, got: %v", err)
	}
	os.Remove(sidecar) // Remove for next test
}

func TestVerifyKernelIntegrity_DevMode(t *testing.T) {
	tmpDir := t.TempDir()
	originalWD, _ := os.Getwd()

	// Instead of changing dir to tmpDir, we can just temporarily override ImmutableKernelPackages inside test if we could,
	// but it's a function. Let's create the directories that ImmutableKernelPackages() returns in the tmpDir and chdir there.
	os.Chdir(tmpDir)
	defer os.Chdir(originalWD)

	pkgs := ImmutableKernelPackages()
	for _, pkg := range pkgs {
		os.MkdirAll(filepath.Join(tmpDir, pkg), 0755)
	}

	// Add a dummy go file to one of the packages
	testFile := filepath.Join(tmpDir, pkgs[0], "test.go")
	os.WriteFile(testFile, []byte("package main"), 0644)

	// Calculate its hash
	h := sha256.New()
	h.Write([]byte("package main"))
	hashStr := hex.EncodeToString(h.Sum(nil))

	// Backup original manifest
	origManifest := kernelManifestJSON
	defer func() { kernelManifestJSON = origManifest }()

	// Valid manifest
	validManifestMap := map[string]string{
		filepath.Join(pkgs[0], "test.go"): hashStr,
	}
	validManifestJSON, _ := json.Marshal(validManifestMap)
	kernelManifestJSON = validManifestJSON

	err := VerifyKernelIntegrity()
	if err != nil {
		t.Errorf("Expected nil error for valid manifest, got: %v", err)
	}

	// Missing file in manifest
	invalidManifestMap1 := map[string]string{
		filepath.Join(pkgs[0], "test.go"): hashStr,
		"missing.go":                      "somehash",
	}
	invalidManifestJSON1, _ := json.Marshal(invalidManifestMap1)
	kernelManifestJSON = invalidManifestJSON1
	err = VerifyKernelIntegrity()
	if err == nil || !strings.Contains(err.Error(), "missing immutable kernel file") {
		t.Errorf("Expected missing file error, got: %v", err)
	}

	// Unexpected file
	validManifestJSON2, _ := json.Marshal(map[string]string{}) // Empty manifest, but we have test.go
	kernelManifestJSON = validManifestJSON2
	err = VerifyKernelIntegrity()
	if err == nil || !strings.Contains(err.Error(), "unexpected new file") {
		t.Errorf("Expected unexpected file error, got: %v", err)
	}

	// Hash mismatch
	invalidManifestMap3 := map[string]string{
		filepath.Join(pkgs[0], "test.go"): "wronghash",
	}
	invalidManifestJSON3, _ := json.Marshal(invalidManifestMap3)
	kernelManifestJSON = invalidManifestJSON3
	err = VerifyKernelIntegrity()
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("Expected hash mismatch error, got: %v", err)
	}
}

func TestVerifyBinarySeal_NoSeal(t *testing.T) {
	// Remove sidecar if it exists
	exe, _ := os.Executable()
	os.Remove(exe + ".sha256")

	err := verifyBinarySeal()
	if err != nil {
		t.Errorf("Expected nil error when no seal exists, got: %v", err)
	}
}

func TestHashPackageDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file
	testFile := filepath.Join(tmpDir, "test.go")
	os.WriteFile(testFile, []byte("test"), 0644)

	manifest := make(map[string]string)
	err := hashPackageDir(tmpDir, manifest)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if len(manifest) != 1 {
		t.Errorf("Expected 1 file in manifest, got: %d", len(manifest))
	}

	// Check hash
	h := sha256.New()
	h.Write([]byte("test"))
	expectedHash := hex.EncodeToString(h.Sum(nil))

	if manifest[testFile] != expectedHash {
		t.Errorf("Hash mismatch. Expected %s, got %s", expectedHash, manifest[testFile])
	}
}
