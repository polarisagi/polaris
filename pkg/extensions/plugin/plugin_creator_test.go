package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// MockLLMClient is a mock implementation of LLMClient for testing.
type MockLLMClient struct{}

func (m *MockLLMClient) Generate(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return `{
  "name": "test-plugin",
  "description": "A simple test plugin",
  "typescript_code": "console.log('hello');"
}`, nil
}

func TestPluginCreator(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "plugin-creator-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	creator := NewPluginCreator(&MockLLMClient{}, tempDir)

	pluginDir, err := creator.GeneratePlugin(context.Background(), "create a test plugin")
	if err != nil {
		t.Fatalf("GeneratePlugin failed: %v", err)
	}

	expectedDir := filepath.Join(tempDir, "test-plugin")
	if pluginDir != expectedDir {
		t.Errorf("Expected plugin dir %s, got %s", expectedDir, pluginDir)
	}

	// Verify src/index.ts exists
	if _, err := os.Stat(filepath.Join(expectedDir, "src", "index.ts")); os.IsNotExist(err) {
		t.Errorf("src/index.ts was not created")
	}

	// Verify package.json exists
	if _, err := os.Stat(filepath.Join(expectedDir, "package.json")); os.IsNotExist(err) {
		t.Errorf("package.json was not created")
	}

	// Verify plugin.json exists
	if _, err := os.Stat(filepath.Join(expectedDir, ".codex-plugin", "plugin.json")); os.IsNotExist(err) {
		t.Errorf("plugin.json was not created")
	}

	// Verify .mcp.json exists
	if _, err := os.Stat(filepath.Join(expectedDir, ".mcp.json")); os.IsNotExist(err) {
		t.Errorf(".mcp.json was not created")
	}
}
