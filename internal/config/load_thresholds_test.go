package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			name: "valid tier",
			config: Config{
				System: SystemConfig{Tier: 1},
			},
			wantErr: false,
		},
		{
			name: "invalid tier",
			config: Config{
				System: SystemConfig{Tier: 4},
			},
			wantErr: true,
		},
		{
			name: "valid gomemlimit",
			config: Config{
				System: SystemConfig{Tier: 0, GoMemLimitMB: 128},
			},
			wantErr: false,
		},
		{
			name: "invalid gomemlimit",
			config: Config{
				System: SystemConfig{Tier: 0, GoMemLimitMB: 32},
			},
			wantErr: true,
		},
		{
			name: "zero gomemlimit",
			config: Config{
				System: SystemConfig{Tier: 0, GoMemLimitMB: 0},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetThresholds(t *testing.T) {
	tmpDir := t.TempDir()

	// Valid TOML
	validTomlPath := filepath.Join(tmpDir, "m1_router.toml")
	os.WriteFile(validTomlPath, []byte("\"circuit_breaker.failure_count\" = 10"), 0644)

	// Set env
	os.Setenv("POLARIS_THRESHOLDS_DIR", tmpDir)
	defer os.Unsetenv("POLARIS_THRESHOLDS_DIR")

	thresholds, err := GetThresholds(tmpDir)
	if err != nil {
		t.Fatalf("GetThresholds failed: %v", err)
	}

	if thresholds.M1Router.CircuitBreakerFailureCount != 10 {
		t.Errorf("Expected 10, got %d", thresholds.M1Router.CircuitBreakerFailureCount)
	}

	// Missing file is ignored
	os.Remove(validTomlPath)
	_, err = GetThresholds(tmpDir)
	if err != nil {
		t.Errorf("Expected nil error for missing files, got %v", err)
	}

	// Invalid TOML
	os.WriteFile(validTomlPath, []byte(`invalid toml format`), 0644)
	_, err = GetThresholds(tmpDir)
	if err == nil {
		t.Errorf("Expected error for invalid toml, got nil")
	}
}

func TestLoadModuleTOML(t *testing.T) {
	tmpDir := t.TempDir()

	// Missing file
	var target any
	err := loadModuleTOML(filepath.Join(tmpDir, "missing.toml"), &target)
	if err != nil {
		t.Errorf("Expected nil error for missing file, got %v", err)
	}

	// Unreadable file
	unreadableFile := filepath.Join(tmpDir, "unreadable.toml")
	os.WriteFile(unreadableFile, []byte(`content`), 0000)
	_ = loadModuleTOML(unreadableFile, &target)

	// Invalid TOML
	invalidFile := filepath.Join(tmpDir, "invalid.toml")
	os.WriteFile(invalidFile, []byte(`not toml`), 0644)
	err = loadModuleTOML(invalidFile, &target)
	if err == nil {
		t.Errorf("Expected error for invalid toml, got nil")
	}
}
