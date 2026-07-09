package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

type

// Provider 是 LLM 厂商适配器的统一接口。
// 每个 Provider 实现负责: SSE 帧归一化（Anthropic SSE / OpenAI SSE / DeepSeek JSON 行流 → 统一 chan StreamEvent）、
// API Key JIT 从 CredentialVault 获取（使用后 subtle.ConstantTimeCopy + memclr 清零）、
// 结构化错误转换为 PolarisError（禁止暴露裸 error）。
Provider interface {
	Infer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error)
	StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error)
	Capabilities() types.ProviderCapabilities
	Tokenizer() TokenizerAdapter
	ModelID() string
}

type

// LocalProvider 扩展 Provider，暴露进程内本地推理特有的模型生命周期管理能力
// （加载/卸载/KV cache 回收/状态查询）。与走网络 API 的远程 Provider 不同，
// 本地模型需要显式的加载/卸载步骤（对应 llama.cpp GGUF 权重装载），供
// "/model local <id>" 热切换场景与硬件门控（FeatureLocalInference）联动使用。
// @consumer: 任何需要对本地模型做生命周期管理的调用方（sysadmin/CLI/未来命令面），
//
//	通过 ProviderRegistry.Get(name) 取出 Provider 后类型断言为 LocalProvider
//
// @producer: internal/llm/adapter/local.go (LocalAdapter，基于 rust/substrate
//
//	llama_infer_* FFI，tier1 feature 门控，P3-1)
//
// @arch: docs/arch/M01-Inference-Runtime.md §8
LocalProvider interface {
	Provider
	// LoadModel 加载/热切换本地 GGUF 模型（覆盖式替换，单槽位常驻）。
	LoadModel(ctx context.Context, modelPath string, opts LocalModelOptions) error
	// UnloadModel 卸载当前模型，释放所有资源（未加载时是幂等 no-op）。
	UnloadModel(ctx context.Context) error
	// EvictKVCache 清空当前常驻生成上下文的 KV cache（会话切换/内存回收场景）。
	EvictKVCache(ctx context.Context) error
	// LocalStatus 查询当前加载状态。
	LocalStatus(ctx context.Context) (LocalModelStatus, error)
	// Probe 验证本地模型当前处于可用（已加载）状态，并返回进程峰值 RSS 供调用方
	// 结合系统已用内存做预算判断。不负责加载模型（加载是 LoadModel 的职责，
	// Probe 只读校验），未加载时返回 ModelLoadable=false 而非 error，
	// 由调用方（如 M11 local_only 启动自检）决定如何处理。
	// @arch: docs/arch/M11-Policy-Safety.md §5.3 Tier3 本地模型守卫
	Probe(ctx context.Context) (LocalProbeResult, error)
}

type

// LocalModelOptions 加载本地模型的可选参数。
LocalModelOptions struct {
	NCtx       uint32 // 上下文窗口 token 数；0 = 使用模型训练时的默认值
	NGPULayers uint32 // GPU 卸载层数；0 = 纯 CPU，999 = 全部层（约定俗成的"全卸载"值）
	NThreads   int32  // CPU 推理线程数；0 = 由 llama.cpp 自动探测
}

type

// LocalModelStatus 本地模型当前加载状态。
LocalModelStatus struct {
	Loaded     bool
	Path       string
	NCtx       uint32
	NCtxTrain  uint32
	NEmbd      int32
	NGPULayers uint32
}

type

// LocalProbeResult Probe() 的探测结果。
LocalProbeResult struct {
	ModelLoadable   bool   // 当前是否有模型处于已加载可用状态
	PeakRSSBytes    uint64 // 本进程峰值常驻内存（ru_maxrss）
	UsedMemoryBytes uint64 // 系统当前已用内存（TotalRAM - AvailableRAM）
}

// @consumer: internal/gateway/server (字段类型), server/chat, server/plugin, server/sysadmin
// LLMRegistry server 包对 LLM Provider 注册表的消费端接口（超集）。
// 实现：llm.ProviderRegistry
type LLMRegistry interface {
	// PickProvider 按角色选取最优 Provider（返回 nil 表示无可用 Provider）。
	PickProvider(role string) Provider
	// PickProviderName 按角色返回最优 Provider 的注册名（用于日志/遥测）。
	PickProviderName(role string) string
	// PickProviderByRecordID 按 provider_models.id 精确选取（用户手动选模型时调用）。
	PickProviderByRecordID(mID string) Provider
	// UnregisterAll 清空所有已注册 Provider（DB 热重载前调用）。
	UnregisterAll()
	// RegisterWithRole 注册一个 Provider，绑定路由角色。
	RegisterWithRole(name, displayName, role string, p Provider)
}
