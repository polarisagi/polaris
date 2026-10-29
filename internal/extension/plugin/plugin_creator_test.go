package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
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

func (m *MockLLMClient) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return m.Generate(ctx, systemPrompt, userPrompt)
}

func TestPluginCreator(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "plugin-creator-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	creator := NewPluginCreator(&MockLLMClient{}, tempDir)

	pluginDir, err := creator.GeneratePlugin(context.Background(), "create a test plugin", 1)
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

	// Verify deno.json exists
	if _, err := os.Stat(filepath.Join(expectedDir, "deno.json")); os.IsNotExist(err) {
		t.Errorf("deno.json was not created")
	}

	// 产物须是 agent-plugins 1.0 布局，且能被统一解析器完整读回（ADR-0103 决策二）。
	p, err := pluginspec.Load(expectedDir, pluginspec.LoadOptions{})
	if err != nil {
		t.Fatalf("generated plugin must load via pluginspec: %v", err)
	}
	if len(p.Formats) != 1 || p.Formats[0] != pluginspec.FormatAgentPlugins || len(p.MCPServers) != 1 {
		t.Fatalf("unexpected generated plugin: formats=%v servers=%+v", p.Formats, p.MCPServers)
	}
	if p.HasErrors() {
		t.Fatalf("generated plugin has errors: %v", p.Diagnostics)
	}
}
