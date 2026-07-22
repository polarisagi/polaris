// Package ffi — llama.go
// 本地推理（P3-1，tier1 feature 门控）purego 绑定，桥接 rust/substrate 的
// llama_infer_* 导出函数。架构文档: docs/arch/M01-Inference-Runtime.md §8
//
// 调用方: internal/llm/adapter/local.go（LocalProvider 实现）
//
// 设计原则:
//   - Go 侧不使用 build tag 区分 tier1/非 tier1：与 native_sandbox/surreal_store/
//     cedar_ffi 等既有 FFI 模块一致，统一走 sync.Once + purego.RegisterLibFunc +
//     recover() 的运行时优雅降级——dylib 若非 --features tier1 构建，符号不存在，
//     RegisterLibFunc 会 panic，recover 后 bindLlamaInfer 返回明确错误，
//     LlamaAvailable() 报告 false，调用方（LocalAdapter）据此跳过 Provider 注册
//     而不是启动崩溃。
//   - 字符串编解码走 NUL-terminated CString 约定（与 rust_native_sandbox.go 的
//     V2 函数一致），因为 Rust 侧 llama_infer::dispatch 复用了 native_sandbox
//     的 ns_write_cstr/ns_read_cstr 同款约定（*const c_char / *mut *mut c_char），
//     而非 Cedar 那种显式 ptr+len 约定。
package ffi

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// llamaFFITarget 是 InstrFFILatencyMs/InstrFFIErrorTotal 的 ffi_target label 值。
const llamaFFITarget = "llama"

// recordLlamaFFICall 记录一次 llama_infer_* FFI 调用的延迟与失败情况。
// 2026-07-04 审计修复（Task 14）：InstrFFILatencyMs/InstrFFIErrorTotal 此前已定义
// 但本文件（本地推理唯一的高频 FFI 调用路径）从未记录过，FFI 健康度不可观测。
func recordLlamaFFICall(ctx context.Context, start time.Time, err error) {
	metrics.RecordFFICall(ctx, llamaFFITarget, float64(time.Since(start).Milliseconds()), err)
}

// ─── purego 函数指针（懒绑定）────────────────────────────────────────────────

var (
	llamaOnce sync.Once
	llamaErr  error

	llamaInferLoad         func(inputJSON uintptr, outJSON *uintptr, outErr *uintptr) int32
	llamaInferUnload       func(outErr *uintptr) int32
	llamaInferGenerate     func(inputJSON uintptr, outJSON *uintptr, outErr *uintptr) int32
	llamaInferEmbed        func(inputJSON uintptr, outJSON *uintptr, outErr *uintptr) int32
	llamaInferRerank       func(inputJSON uintptr, outJSON *uintptr, outErr *uintptr) int32
	llamaInferEvictKVCache func(outErr *uintptr) int32
	llamaInferStatus       func(outJSON *uintptr, outErr *uintptr) int32
	llamaInferFreeString   func(ptr uintptr)
)

func bindLlamaInfer() error {
	llamaOnce.Do(func() {
		lib, err := Load()
		if err != nil {
			llamaErr = err
			return
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					llamaErr = apperr.New(apperr.CodeInternal,
						fmt.Sprintf("llama_infer 符号未找到（dylib 未以 --features tier1 构建）: %v", r))
				}
			}()
			purego.RegisterLibFunc(&llamaInferLoad, lib, "llama_infer_load")
			purego.RegisterLibFunc(&llamaInferUnload, lib, "llama_infer_unload")
			purego.RegisterLibFunc(&llamaInferGenerate, lib, "llama_infer_generate")
			purego.RegisterLibFunc(&llamaInferEmbed, lib, "llama_infer_embed")
			purego.RegisterLibFunc(&llamaInferRerank, lib, "llama_infer_rerank")
			purego.RegisterLibFunc(&llamaInferEvictKVCache, lib, "llama_infer_evict_kv_cache")
			purego.RegisterLibFunc(&llamaInferStatus, lib, "llama_infer_status")
			purego.RegisterLibFunc(&llamaInferFreeString, lib, "llama_infer_free_string")
		}()
	})
	return llamaErr
}

// LlamaAvailable 探测当前进程加载的 substrate dylib 是否导出 llama_infer_*
// 符号（即是否以 --features tier1 构建）。供硬件门控（FeatureLocalInference）
// 之外再加一层"二进制是否具备本地推理能力"的显式判断，避免 Tier-1 硬件却
// 用 Tier-0 二进制时静默注册一个必然失败的 Provider。
func LlamaAvailable() bool {
	return bindLlamaInfer() == nil
}

// ─── FFI 字符串辅助（NUL-terminated 约定，与 rust_native_sandbox.go 一致）────

func llamaGoStringToC(s string) []byte {
	b := make([]byte, len(s)+1)
	copy(b, s)
	b[len(s)] = 0
	return b
}

func llamaReadAndFreeStr(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	var n int
	for {
		b := *(*byte)(unsafe.Pointer(ptr + uintptr(n)))
		if b == 0 {
			break
		}
		n++
	}
	s := string(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), n))
	llamaInferFreeString(ptr)
	return s
}

// ─── 请求/响应类型（字段与 rust/substrate/src/llama_infer/mod.rs 对齐）──────

type LlamaLoadRequest struct {
	ModelPath  string `json:"model_path"`
	NCtx       uint32 `json:"n_ctx"`
	NGPULayers uint32 `json:"n_gpu_layers"`
	NThreads   int32  `json:"n_threads"`
}

type LlamaLoadResponse struct {
	OK        bool   `json:"ok"`
	NCtx      uint32 `json:"n_ctx"`
	NCtxTrain uint32 `json:"n_ctx_train"`
	NEmbd     int32  `json:"n_embd"`
	NParams   uint64 `json:"n_params"`
}

type LlamaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type LlamaGenerateRequest struct {
	Messages    []LlamaChatMessage `json:"messages"`
	MaxTokens   int32              `json:"max_tokens"`
	Temperature float32            `json:"temperature"`
	TopP        float32            `json:"top_p"`
	Seed        uint32             `json:"seed"`
	Grammar     string             `json:"grammar,omitempty"`
	GrammarRoot string             `json:"grammar_root,omitempty"`
	Stop        []string           `json:"stop,omitempty"`
}

type LlamaGenerateResponse struct {
	Text            string `json:"text"`
	PromptTokens    int32  `json:"prompt_tokens"`
	TokensGenerated int32  `json:"tokens_generated"`
	FinishReason    string `json:"finish_reason"`
}

type LlamaEmbedRequest struct {
	Texts []string `json:"texts"`
}

type LlamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	NEmbd      int32       `json:"n_embd"`
}

type LlamaRerankRequest struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type LlamaRerankResponse struct {
	Scores []float32 `json:"scores"`
}

type LlamaStatusResponse struct {
	Loaded     bool   `json:"loaded"`
	Path       string `json:"path"`
	NCtx       uint32 `json:"n_ctx"`
	NCtxTrain  uint32 `json:"n_ctx_train"`
	NEmbd      int32  `json:"n_embd"`
	NGPULayers uint32 `json:"n_gpu_layers"`
}

// ─── 公开 API ─────────────────────────────────────────────────────────────

// LlamaLoad 加载/热切换本地 GGUF 模型（单槽位，覆盖式替换旧模型）。
func LlamaLoad(ctx context.Context, req LlamaLoadRequest) (result *LlamaLoadResponse, err error) {
	start := time.Now()
	defer func() { recordLlamaFFICall(ctx, start, err) }()
	if err := bindLlamaInfer(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer_load: dylib/符号不可用", err)
	}
	inputCStr, err := marshalLlamaRequest(req)
	if err != nil {
		return nil, err
	}
	var outJSON, outErr uintptr
	code := llamaInferLoad(uintptr(unsafe.Pointer(&inputCStr[0])), &outJSON, &outErr)
	runtime.KeepAlive(inputCStr)
	errStr := llamaReadAndFreeStr(outErr)
	jsonStr := llamaReadAndFreeStr(outJSON)
	if code < 0 {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("llama_infer_load failed (code=%d): %s", code, errStr))
	}
	var resp LlamaLoadResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer_load: unmarshal response: "+jsonStr, err)
	}
	return &resp, nil
}

// LlamaUnload 卸载当前模型，释放所有资源。未加载时是幂等 no-op。
func LlamaUnload(ctx context.Context) (err error) {
	start := time.Now()
	defer func() { recordLlamaFFICall(ctx, start, err) }()
	if err := bindLlamaInfer(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "llama_infer_unload: dylib/符号不可用", err)
	}
	var outErr uintptr
	code := llamaInferUnload(&outErr)
	errStr := llamaReadAndFreeStr(outErr)
	if code < 0 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("llama_infer_unload failed (code=%d): %s", code, errStr))
	}
	return nil
}

// LlamaGenerate 对话生成（chat template + sampler chain + 可选 GBNF grammar）。
func LlamaGenerate(ctx context.Context, req LlamaGenerateRequest) (result *LlamaGenerateResponse, err error) {
	start := time.Now()
	defer func() { recordLlamaFFICall(ctx, start, err) }()
	if err := bindLlamaInfer(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer_generate: dylib/符号不可用", err)
	}
	inputCStr, err := marshalLlamaRequest(req)
	if err != nil {
		return nil, err
	}
	var outJSON, outErr uintptr
	code := llamaInferGenerate(uintptr(unsafe.Pointer(&inputCStr[0])), &outJSON, &outErr)
	runtime.KeepAlive(inputCStr)
	errStr := llamaReadAndFreeStr(outErr)
	jsonStr := llamaReadAndFreeStr(outJSON)
	if code < 0 {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("llama_infer_generate failed (code=%d): %s", code, errStr))
	}
	var resp LlamaGenerateResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer_generate: unmarshal response: "+jsonStr, err)
	}
	return &resp, nil
}

// LlamaEmbed 批量文本嵌入（Mean pooling）。
func LlamaEmbed(ctx context.Context, req LlamaEmbedRequest) (result *LlamaEmbedResponse, err error) {
	start := time.Now()
	defer func() { recordLlamaFFICall(ctx, start, err) }()
	if err := bindLlamaInfer(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer_embed: dylib/符号不可用", err)
	}
	inputCStr, err := marshalLlamaRequest(req)
	if err != nil {
		return nil, err
	}
	var outJSON, outErr uintptr
	code := llamaInferEmbed(uintptr(unsafe.Pointer(&inputCStr[0])), &outJSON, &outErr)
	runtime.KeepAlive(inputCStr)
	errStr := llamaReadAndFreeStr(outErr)
	jsonStr := llamaReadAndFreeStr(outJSON)
	if code < 0 {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("llama_infer_embed failed (code=%d): %s", code, errStr))
	}
	var resp LlamaEmbedResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer_embed: unmarshal response: "+jsonStr, err)
	}
	return &resp, nil
}

// LlamaRerank 重排打分。
func LlamaRerank(ctx context.Context, req LlamaRerankRequest) (result *LlamaRerankResponse, err error) {
	start := time.Now()
	defer func() { recordLlamaFFICall(ctx, start, err) }()
	if err := bindLlamaInfer(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer_rerank: dylib/符号不可用", err)
	}
	inputCStr, err := marshalLlamaRequest(req)
	if err != nil {
		return nil, err
	}
	var outJSON, outErr uintptr
	code := llamaInferRerank(uintptr(unsafe.Pointer(&inputCStr[0])), &outJSON, &outErr)
	runtime.KeepAlive(inputCStr)
	errStr := llamaReadAndFreeStr(outErr)
	jsonStr := llamaReadAndFreeStr(outJSON)
	if code < 0 {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("llama_infer_rerank failed (code=%d): %s", code, errStr))
	}
	var resp LlamaRerankResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer_rerank: unmarshal response: "+jsonStr, err)
	}
	return &resp, nil
}

// LlamaEvictKVCache 强制清空 KV Cache。
func LlamaEvictKVCache(ctx context.Context) (err error) {
	start := time.Now()
	defer func() { recordLlamaFFICall(ctx, start, err) }()
	if err := bindLlamaInfer(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "llama_infer_evict_kv_cache: dylib/符号不可用", err)
	}
	var outErr uintptr
	code := llamaInferEvictKVCache(&outErr)
	errStr := llamaReadAndFreeStr(outErr)
	if code < 0 {
		return apperr.New(apperr.CodeInternal,
			fmt.Sprintf("llama_infer_evict_kv_cache failed (code=%d): %s", code, errStr))
	}
	return nil
}

// LlamaStatus 获取当前引擎与模型状态（内存、负载、KV 碎片率等）。
func LlamaStatus(ctx context.Context) (result *LlamaStatusResponse, err error) {
	start := time.Now()
	defer func() { recordLlamaFFICall(ctx, start, err) }()
	if err := bindLlamaInfer(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer_status: dylib/符号不可用", err)
	}
	var outJSON, outErr uintptr
	code := llamaInferStatus(&outJSON, &outErr)
	errStr := llamaReadAndFreeStr(outErr)
	jsonStr := llamaReadAndFreeStr(outJSON)
	if code < 0 {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("llama_infer_status failed (code=%d): %s", code, errStr))
	}
	var resp LlamaStatusResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer_status: unmarshal response: "+jsonStr, err)
	}
	return &resp, nil
}

func marshalLlamaRequest(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "llama_infer: marshal request", err)
	}
	return llamaGoStringToC(string(b)), nil
}
