package main

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
)

func TestChooseEmbedding_TableDriven(t *testing.T) {
	tests := []struct {
		name          string
		cfg           config.EmbeddingConfig
		inferDim      int
		onnxAvailable bool
		wantKind      string
		wantDim       int
	}{
		{
			name: "none explicit",
			cfg: config.EmbeddingConfig{
				Backend: "none",
			},
			inferDim:      1536,
			onnxAvailable: true,
			wantKind:      "none",
			wantDim:       0,
		},
		{
			name: "remote only",
			cfg: config.EmbeddingConfig{
				BaseURL: "https://api.openai.com/v1",
				Model:   "text-embedding-3-small",
			},
			inferDim:      1536,
			onnxAvailable: false,
			wantKind:      "remote",
			wantDim:       1536,
		},
		{
			name: "remote + local both configured -> remote wins",
			cfg: config.EmbeddingConfig{
				BaseURL: "https://api.openai.com/v1",
				Backend: "ollama",
				Model:   "qwen3-embedding:4b",
				Dim:     2560,
			},
			inferDim:      1536,
			onnxAvailable: true,
			wantKind:      "remote",
			wantDim:       1536,
		},
		{
			name: "backend=ollama without model -> config invalid / returns none",
			cfg: config.EmbeddingConfig{
				Backend: "ollama",
				Model:   "",
				Dim:     1024,
			},
			inferDim:      1536,
			onnxAvailable: true,
			wantKind:      "none",
			wantDim:       0,
		},
		{
			name: "backend=ollama with model",
			cfg: config.EmbeddingConfig{
				Backend: "ollama",
				Model:   "qwen3-embedding:0.6b",
				Dim:     1024,
			},
			inferDim:      1536,
			onnxAvailable: false,
			wantKind:      "ollama",
			wantDim:       1024,
		},
		{
			name: "backend=llama_server with model",
			cfg: config.EmbeddingConfig{
				Backend: "llama_server",
				Model:   "qwen3-embedding:0.6b",
				Dim:     1024,
			},
			inferDim:      1536,
			onnxAvailable: false,
			wantKind:      "llama_server",
			wantDim:       1024,
		},
		{
			name: "auto + no remote + onnx available -> onnx / 512",
			cfg: config.EmbeddingConfig{
				Backend:   "auto",
				ONNXModel: "auto",
			},
			inferDim:      1536,
			onnxAvailable: true,
			wantKind:      "onnx",
			wantDim:       512,
		},
		{
			name: "auto + no remote + onnx unavailable -> none",
			cfg: config.EmbeddingConfig{
				Backend: "auto",
			},
			inferDim:      1536,
			onnxAvailable: false,
			wantKind:      "none",
			wantDim:       0,
		},
		{
			name: "onnx explicit + onnx available -> onnx / 512",
			cfg: config.EmbeddingConfig{
				Backend:   "onnx",
				ONNXModel: "embeddinggemma",
			},
			inferDim:      1536,
			onnxAvailable: true,
			wantKind:      "onnx",
			wantDim:       512,
		},
		{
			name: "onnx explicit + onnx unavailable -> none",
			cfg: config.EmbeddingConfig{
				Backend:   "onnx",
				ONNXModel: "bge-small-zh",
			},
			inferDim:      1536,
			onnxAvailable: false,
			wantKind:      "none",
			wantDim:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chooseEmbedding(tt.cfg, tt.inferDim, tt.onnxAvailable)
			if got.Kind != tt.wantKind {
				t.Errorf("chooseEmbedding Kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if got.Dim != tt.wantDim {
				t.Errorf("chooseEmbedding Dim = %d, want %d", got.Dim, tt.wantDim)
			}
		})
	}
}

func TestLocalBackendEnabled(t *testing.T) {
	if localBackendEnabled(nil) {
		t.Error("nil config should return false")
	}

	cfg := &config.Config{}
	cfg.Embedding.Backend = "auto"
	if localBackendEnabled(cfg) {
		t.Error("auto backend should return false")
	}

	cfg.Embedding.Backend = "ollama"
	if !localBackendEnabled(cfg) {
		t.Error("ollama backend should return true")
	}

	cfg.Embedding.Backend = "llama_server"
	if !localBackendEnabled(cfg) {
		t.Error("llama_server backend should return true")
	}

	cfg.Embedding.Backend = "onnx"
	if localBackendEnabled(cfg) {
		t.Error("onnx backend should return false")
	}

	cfg.Embedding.Backend = "none"
	if localBackendEnabled(cfg) {
		t.Error("none backend should return false")
	}
}

func TestRemotePriorityDoesNotStartOllama(t *testing.T) {
	cfg := config.EmbeddingConfig{
		BaseURL: "https://api.openai.com/v1",
		Model:   "text-embedding-3-small",
		Backend: "ollama", // 即使配置了本地，远程依然绝对优先
	}

	choice := chooseEmbedding(cfg, 1536, false)
	if choice.Kind != "remote" {
		t.Fatalf("expected Kind=remote, got %q", choice.Kind)
	}

	// 远程配置下 Kind 是 remote，initEmbedding 不会执行 Ollama 生命周期
	if choice.Kind == "ollama" {
		t.Fatal("remote configuration must NEVER select ollama")
	}
}

func TestNoAutoRegisteredLocalProviders(t *testing.T) {
	// 构建全新的 ProviderRegistry，验证无默认本地 Provider 自动注册
	th := config.CurrentThresholds().M1Router
	reg := llm.NewProviderRegistry(th)

	for _, name := range []string{"ollama-local", "ollama-large", "llama-local"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("expected provider %q not to be automatically registered", name)
		}
	}
}

func TestFTSModeSkipsReindexer(t *testing.T) {
	sb := &SubstrateBundle{
		Embedder: nil, // FTS 模式
	}
	reindexFn := startOnlineReindexer(context.Background(), sb)
	if reindexFn != nil {
		t.Error("expected startOnlineReindexer to return nil when embedder is nil (FTS mode)")
	}
}
