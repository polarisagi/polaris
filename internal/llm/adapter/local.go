package adapter

import (
	"context"

	"github.com/polarisagi/polaris/internal/ffi"
	llmparent "github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// LocalAdapter 基于 rust/substrate llama_infer FFI（tier1 feature 门控，P3-1）
// 的进程内本地推理适配器。架构文档: docs/arch/M01-Inference-Runtime.md §8
//
// 与 OllamaAdapter（HTTP 调用独立 Ollama 服务进程）的区别: LocalAdapter 直接
// 在当前进程内通过 purego 调用 Rust 侧 llama.cpp 绑定，无需额外部署/管理一个
// Ollama 服务，适合单二进制部署场景；两条本地推理路径并不互斥，由启动时
// FeatureLocalInference 门控 + ffi.LlamaAvailable()（二进制是否以
// --features tier1 构建）共同决定注册哪一个/是否都注册，交给 Router 按健康度/
// 延迟自动择优（见 cmd/polaris/boot_substrate.go）。
type LocalAdapter struct {
	caps types.ProviderCapabilities
	// modelID 缓存最近一次 LoadModel 的路径，用作 ModelID() 展示；
	// 未加载时返回固定占位符，避免空字符串导致 Router 展示异常。
	modelID string
}

var (
	_ protocol.Provider      = (*LocalAdapter)(nil)
	_ protocol.LocalProvider = (*LocalAdapter)(nil)
)

// NewLocalAdapter 构造本地 llama.cpp FFI 推理适配器。构造时不加载任何模型
// （惰性——LoadModel 由调用方在热切换场景中显式触发，见 LocalProvider 接口）。
func NewLocalAdapter() *LocalAdapter {
	return &LocalAdapter{
		caps: types.ProviderCapabilities{
			SupportsStreaming: true,
			// llama.cpp 无原生 tool_call 协议；GBNF grammar 可强约束输出格式，
			// 但那是"结构化输出"而非"模型原生发起 tool_use"语义，故如实声明 false。
			SupportsTools:    false,
			SupportsThinking: false,
			MaxContextTokens: 4096, // LoadModel 成功后按模型实际 n_ctx 动态更新
		},
		modelID: "local:unloaded",
	}
}

func (a *LocalAdapter) ModelID() string                          { return a.modelID }
func (a *LocalAdapter) Capabilities() types.ProviderCapabilities { return a.caps }
func (a *LocalAdapter) Tokenizer() protocol.TokenizerAdapter     { return &llmparent.SimpleTokenizer{} }

// LoadModel 实现 protocol.LocalProvider。
func (a *LocalAdapter) LoadModel(ctx context.Context, modelPath string, opts protocol.LocalModelOptions) error {
	resp, err := ffi.LlamaLoad(ctx, ffi.LlamaLoadRequest{
		ModelPath:  modelPath,
		NCtx:       opts.NCtx,
		NGPULayers: opts.NGPULayers,
		NThreads:   opts.NThreads,
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "local adapter: load model", err)
	}
	a.modelID = "local:" + modelPath
	if resp.NCtx > 0 {
		a.caps.MaxContextTokens = int(resp.NCtx)
	}
	return nil
}

// UnloadModel 实现 protocol.LocalProvider。
func (a *LocalAdapter) UnloadModel(ctx context.Context) error {
	if err := ffi.LlamaUnload(ctx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "local adapter: unload model", err)
	}
	a.modelID = "local:unloaded"
	return nil
}

// EvictKVCache 实现 protocol.LocalProvider。
func (a *LocalAdapter) EvictKVCache(ctx context.Context) error {
	if err := ffi.LlamaEvictKVCache(ctx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "local adapter: evict kv cache", err)
	}
	return nil
}

// LocalStatus 实现 protocol.LocalProvider。
func (a *LocalAdapter) LocalStatus(ctx context.Context) (protocol.LocalModelStatus, error) {
	s, err := ffi.LlamaStatus(ctx)
	if err != nil {
		return protocol.LocalModelStatus{}, apperr.Wrap(apperr.CodeInternal, "local adapter: status", err)
	}
	return protocol.LocalModelStatus{
		Loaded:     s.Loaded,
		Path:       s.Path,
		NCtx:       s.NCtx,
		NCtxTrain:  s.NCtxTrain,
		NEmbd:      s.NEmbd,
		NGPULayers: s.NGPULayers,
	}, nil
}

// Probe 实现 protocol.LocalProvider。只读校验，不触发加载。
// 架构文档: docs/arch/M11-Policy-Safety.md §5.3——Tier3 local_only 启动自检使用此
// 结果判断"峰值 RSS + 已用内存 < 64GB (1GB 预留)"预算是否超限。
func (a *LocalAdapter) Probe(ctx context.Context) (protocol.LocalProbeResult, error) {
	status, err := a.LocalStatus(ctx)
	if err != nil {
		return protocol.LocalProbeResult{}, apperr.Wrap(apperr.CodeInternal, "local adapter: probe", err)
	}
	totalRAM, availableRAM := probe.MemoryProbe()
	usedMemory := uint64(0)
	if totalRAM > availableRAM {
		usedMemory = totalRAM - availableRAM
	}
	return protocol.LocalProbeResult{
		ModelLoadable:   status.Loaded,
		PeakRSSBytes:    probe.ProcessPeakRSSBytes(),
		UsedMemoryBytes: usedMemory,
	}, nil
}

func toLocalMessages(msgs []types.Message) []ffi.LlamaChatMessage {
	out := make([]ffi.LlamaChatMessage, 0, len(msgs))
	for _, m := range msgs {
		// Parts 非空时暂退回 Content 字符串表示：本地推理路径尚不支持多模态
		// Parts（与 SupportsVision=false 声明一致），未来接入 llama.cpp mtmd
		// 视觉扩展时在此处改造。
		out = append(out, ffi.LlamaChatMessage{Role: m.Role, Content: m.Content})
	}
	return out
}

func (a *LocalAdapter) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	options := &types.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := ffi.LlamaGenerateRequest{
		Messages:    toLocalMessages(msgs),
		MaxTokens:   int32(options.MaxTokens),
		Temperature: float32(options.Temperature),
		TopP:        float32(options.TopP),
	}
	if options.ResponseFormat != nil && options.ResponseFormat.Type == "gbnf" {
		req.Grammar = options.ResponseFormat.Grammar
	}
	resp, err := ffi.LlamaGenerate(ctx, req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "local adapter: infer", err)
	}
	return &types.ProviderResponse{
		Content:      resp.Text,
		Model:        a.modelID,
		FinishReason: resp.FinishReason,
		Usage: types.Usage{
			// PromptTokens 来自 Rust 侧真实 tokenizer 计数（str_to_token 结果长度），
			// 非估算值——精度优于其它 Provider 依赖 SimpleTokenizer 粗估的路径。
			InputTokens:  int(resp.PromptTokens),
			OutputTokens: int(resp.TokensGenerated),
		},
	}, nil
}

// StreamInfer 当前实现为"整体生成后单帧下发"（非逐 token 流式）——
// llama_infer_generate 的 FFI 协议是 run-to-completion（Rust 侧一次性返回
// 完整文本），真正的逐 token SSE 需要 Rust 侧改为回调/迭代器式 FFI（每采样
// 一个 token 就跨界回调一次 Go），属于更大的 FFI 协议改造，记录为已知后续
// 优化点（不在本次 P3-1 范围内）。此处仍返回 <-chan types.StreamEvent 以
// 满足 protocol.Provider 接口，调用方感知到的是"一次性到达的单个 delta"。
func (a *LocalAdapter) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultStreamInferTimeout)
		defer cancel()
	}

	ch := make(chan types.StreamEvent, 1)
	// [SafeGo] a.Infer 经 purego 跨界调用 Rust llama.cpp FFI，边界异常此前会直接崩进程。
	concurrent.SafeGo(ctx, "llm.adapter.local_stream_infer", func(ctx context.Context) {
		defer close(ch)
		resp, err := a.Infer(ctx, msgs, opts...)
		if err != nil {
			ch <- types.StreamEvent{Type: types.StreamError, Content: err.Error()}
			return
		}
		ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: resp.Content, Usage: resp.Usage}
	})
	return ch, nil
}
