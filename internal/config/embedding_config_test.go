package config

import (
	"os"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestEmbeddingConfig_Defaults(t *testing.T) {
	cfg := loadCfg(t, "")
	emb := cfg.Embedding
	if emb.Backend != "auto" {
		t.Errorf("expected backend 'auto', got %q", emb.Backend)
	}
	if emb.ONNXModel != "auto" {
		t.Errorf("expected onnx_model 'auto', got %q", emb.ONNXModel)
	}
	if emb.Dim != 0 {
		t.Errorf("expected dim 0, got %d", emb.Dim)
	}
}

func TestEmbeddingConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr bool
	}{
		{
			name: "valid auto",
			toml: `[embedding]
backend = "auto"
onnx_model = "auto"`,
			wantErr: false,
		},
		{
			name: "valid remote",
			toml: `[embedding]
base_url = "https://api.openai.com/v1"
model = "text-embedding-3-small"`,
			wantErr: false,
		},
		{
			name: "valid explicit ollama",
			toml: `[embedding]
backend = "ollama"
model = "qwen3-embedding:0.6b"
dim = 1024`,
			wantErr: false,
		},
		{
			// llama_server 尚未接线（ADR-0109 P2–P4），显式拒绝而不是挂一个永远没有引擎的嵌入器。
			name: "llama_server rejected until implemented",
			toml: `[embedding]
backend = "llama_server"
model = "qwen3-embedding:0.6b"
dim = 1024`,
			wantErr: true,
		},
		{
			name: "invalid backend",
			toml: `[embedding]
backend = "invalid_backend"`,
			wantErr: true,
		},
		{
			name: "invalid onnx_model",
			toml: `[embedding]
onnx_model = "invalid_model"`,
			wantErr: true,
		},
		{
			name: "ollama missing model",
			toml: `[embedding]
backend = "ollama"
dim = 1024`,
			wantErr: true,
		},
		{
			name: "ollama missing dim",
			toml: `[embedding]
backend = "ollama"
model = "qwen3-embedding:0.6b"`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := t.TempDir() + "/config.toml"
			if err := writeTestFile(p, tc.toml); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				// Verify the underlying validation error has CodeInvalidInput
				if !apperr.IsCode(err, apperr.CodeInvalidInput) && !strings.Contains(err.Error(), "INVALID_INPUT") {
					t.Errorf("expected INVALID_INPUT in error, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func writeTestFile(path, content string) error {
	return osWrite(path, []byte(content))
}

func osWrite(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
