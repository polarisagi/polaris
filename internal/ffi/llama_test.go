package ffi

import (
	"context"
	"testing"
)

// 本文件测试 llama_infer purego 绑定的优雅降级路径——不依赖 dylib 是否以
// --features tier1 构建（CI/开发机状态不定）。无论 llama_infer_* 符号是否
// 存在，调用方都应得到明确的 Go error，而不是 panic 或裸崩溃：
//   - 符号存在（tier1 构建）：走真实 FFI，返回"未加载模型"一类的业务错误。
//   - 符号不存在（非 tier1 构建）：bindLlamaInfer 通过 recover() 捕获
//     purego.RegisterLibFunc 的 panic，转为 apperr，同样是明确的 Go error。
// 真实加载 GGUF 权重进行端到端生成测试需要模型文件 fixture（MB~GB 级），
// 不适合作为默认 `make test` 的一部分，留作手动/opt-in 集成测试。

func TestLlamaAvailable_DoesNotPanic(t *testing.T) {
	// 无论底层 dylib 是否以 tier1 构建，调用本身必须安全返回，不 panic。
	_ = LlamaAvailable()
}

func TestLlamaStatus_GracefulWhenUnavailableOrNoModel(t *testing.T) {
	resp, err := LlamaStatus(context.Background())
	if !LlamaAvailable() {
		if err == nil {
			t.Fatal("expected error when llama_infer symbols unavailable")
		}
		return
	}
	// 符号可用时，未加载模型应返回 loaded=false 而非报错。
	if err != nil {
		t.Fatalf("LlamaStatus should not error when symbols available: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil status response")
	}
}

func TestLlamaGenerate_GracefulWhenNoModelLoaded(t *testing.T) {
	_, err := LlamaGenerate(context.Background(), LlamaGenerateRequest{
		Messages: []LlamaChatMessage{{Role: "user", Content: "hi"}},
	})
	// 无论符号是否存在，未加载模型时都必须返回非 nil error（不可能成功）。
	if err == nil {
		t.Fatal("expected error: no model loaded (or symbols unavailable)")
	}
}

func TestLlamaEvictKVCache_GracefulWhenNoModelLoaded(t *testing.T) {
	err := LlamaEvictKVCache(context.Background())
	if err == nil {
		t.Fatal("expected error: no model loaded (or symbols unavailable)")
	}
}

func TestLlamaUnload_IdempotentNoop(t *testing.T) {
	// unload 在未加载模型时应是幂等 no-op（符号可用时）；符号不可用时返回明确错误。
	err := LlamaUnload(context.Background())
	if !LlamaAvailable() {
		if err == nil {
			t.Fatal("expected error when llama_infer symbols unavailable")
		}
		return
	}
	if err != nil {
		t.Fatalf("unload without loaded model should be a no-op, got: %v", err)
	}
}
