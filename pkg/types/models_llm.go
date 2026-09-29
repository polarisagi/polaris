package types

type

// Usage LLM 调用的 Token 用量统计。
// 跨 M1（Provider）、M3（Observability）、M8（Blackboard token 记账）共用。
Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheHitTokens      int // Anthropic: cache_read_input_tokens
	CacheCreationTokens int // Anthropic: cache_creation_input_tokens（写入缓存消耗）
	ReasoningTokens     int // 扩展思考消耗的 token 数（不计入 OutputTokens）
}

type

// ProviderCapabilities LLM Provider 的能力声明（供 Router 路由决策）。
ProviderCapabilities struct {
	SupportsStreaming bool
	SupportsTools     bool
	SupportsThinking  bool
	SupportsVision    bool
	SupportsVideo     bool
	SupportsTTS       bool
	// SupportsPromptCacheKey 端点认识 OpenAI 的 prompt_cache_key / prompt_cache_retention
	// 请求字段（ADR-0105 决策三）。仅 OpenAI 官方端点默认 true；DeepSeek/Ollama/local 及其他
	// 兼容端点为 false，避免向不认识该字段的端点发送未知参数。
	SupportsPromptCacheKey bool
	MaxContextTokens       int
	CostPer1KInput         float64
	CostPer1KOutput        float64
	CostPer1KCacheHit      float64
}

type

// StreamEvent LLM 流式输出的单个事件帧。
StreamEvent struct {
	Type    StreamEventType
	Content string
	Usage   Usage
}

type

// ImagePart 多模态图片内容块（工具结果、LLM 消息均可携带）。
// 注意：不含任何方法，与 internal/protocol 中的同名类型语义相同。
ImagePart struct {
	Type      string // "image"
	MediaType string // "image/jpeg" | "image/png" | "image/webp" | "image/gif"
	Data      []byte // base64 decoded raw bytes
	URL       string // 互斥于 Data，远程 URL 路径
	Width     int    // 可选，0=未知；token 计算用
	Height    int    // 可选，0=未知；token 计算用
	Detail    string // "low" | "high" | "auto"，空串等同 "auto"
}
type InferRequest struct {
	Model          string
	Messages       []Message
	Tools          []ToolSchema
	MaxTokens      int
	Temperature    float64
	Thinking       *ThinkingConfig
	ResponseFormat *ResponseFormat // 支持强制 JSON Schema / GBNF 等结构化约束
	ThinkingMode   ThinkingMode    // TTC 推理深度控制（None=不传，High=最大扩展思考）
	ThinkingBudget int
	ModelPool      string // 目标 Model Pool（Provider 角色/role），空串表示不限定。由 WithModelPool 选项填充（GD-13-005）
	// ToolChoice 工具选择约束（ADR-0105 决策三）："" = 不下发（Provider 默认 auto）；
	// "none" = 带 tools 定义但禁止调用（使各阶段 tools 前缀一致而不触发工具）；"auto"|"required"。
	// 仅在 Tools 非空时有意义；不支持的适配器忽略。
	ToolChoice string
}

func (req *InferRequest) HasImageParts() bool {
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if _, ok := p.(ImagePart); ok {
				return true
			}
		}
	}
	return false
}

func (req *InferRequest) HasVideoParts() bool {
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if _, ok := p.(VideoPart); ok {
				return true
			}
		}
	}
	return false
}

type ResponseFormat struct {
	Type       string // "json_object" | "json_schema" | "gbnf"
	JSONSchema any    // 当 Type="json_schema" 时传递的 Schema
	Grammar    string // 当 Type="gbnf" 时传递的规则串
}
type Message struct {
	Role    string
	Content string
	// Parts 非空时，adapter 应使用 Parts 作为 content（用于 tool_use/tool_result 多块消息）。
	// 向后兼容：nil 时退回到 Content 字符串。
	Parts []any
	// ReasoningContent 保存 DeepSeek 思考模式下的 reasoning_content，
	// 多轮 tool_call 时必须原样回传，否则 API 返回 400。
	ReasoningContent string
	// CacheBreakpoint 由 prompt 组装方在缓存层末尾的消息上置位（ADR-0105 决策一：L2 历史层最后一条）。
	// 语义：该消息（含）之前的前缀应被 Provider 显式缓存。当前仅 Anthropic 适配器消费它来放置
	// cache_control 断点；其余适配器忽略，不参与任何序列化。无任何消息置位时适配器回退到
	// 内置启发式（首块 system + 末块 system + 最近 2 条消息）。
	CacheBreakpoint bool
}
type VideoPart struct {
	Type      string // "video"
	MediaType string // "video/mp4" | "video/webm"
	Data      []byte // 文件内容 (≤20MB inline)
	URI       string // Provider File API 上传后的 URI
}
type ToolSchema struct {
	Name        string
	Description string
	Parameters  any // JSON Schema
}
type ThinkingConfig struct {
	BudgetTokens int
	Mode         string // "auto" | "enabled" | "disabled"
}

type

// InferToolCall LLM 返回的工具调用请求（finish_reason=tool_calls / stop_reason=tool_use 时）。
InferToolCall struct {
	ID    string
	Name  string
	Input []byte // JSON 编码的工具输入参数
}
type InferResponse struct {
	Content      string
	ToolCalls    []InferToolCall // LLM 请求调用的工具列表；为空表示纯文本回复
	Usage        Usage
	Model        string
	FinishReason string
}

type

// InferOptions Provider 调用的可选参数集合。
InferOptions struct {
	ThinkingMode   ThinkingMode // 默认 ThinkingDisabled
	MaxTokens      int          // 0 = 使用模型默认值
	Model          string
	Tools          []ToolSchema
	ResponseFormat *ResponseFormat
	Temperature    float64
	TopP           float64
	ThinkingBudget int
	CacheHints     *SemanticCacheHints
	ModelPool      string // 目标 Model Pool（Provider 角色/role），空串表示不限定（GD-13-005）
	// Purpose 调用用途（perceive/plan/consolidate_summary/...），只用于 llm_calls 记账归因，
	// 不参与路由与请求构造。空串记为 "unspecified"。
	Purpose string
	// ToolChoice 见 InferRequest.ToolChoice。
	ToolChoice string
}

type

// SemanticCacheHints 语义缓存键值元数据。
SemanticCacheHints struct {
	Namespace              string
	SystemPromptHash       string
	ContextHintFingerprint string
	ActiveControlLabels    []string
	TaskType               string
}

type

// InferOption 函数选项模式，用于构造 InferOptions。
InferOption func(*InferOptions)

type

// ProviderResponse Provider 完整响应，包含思考内容和最终答案。
ProviderResponse struct {
	Content          string          // 最终回答
	ReasoningContent string          // CoT 思考内容（thinking mode 时有值）
	ToolCalls        []InferToolCall // 工具调用（若模型发起）；用现有 ToolCall 类型
	Usage            Usage           // Token 用量；用现有 Usage 类型（若存在）
	Model            string          // 添加以兼容现有使用
	FinishReason     string          // 添加以兼容现有使用
	DegradedFromPool string          `json:"degraded_from_pool,omitempty"` // 若非空，表示发生了跨 Pool 降级，值为原始请求目标 Pool
}

// WithResponseFormat 设置响应格式
func WithResponseFormat(fmt *ResponseFormat) InferOption {
	return func(o *InferOptions) { o.ResponseFormat = fmt }
}

// WithToolChoice 设置工具选择约束（"none"|"auto"|"required"），仅在同时下发 tools 时生效（ADR-0105 决策三）。
func WithToolChoice(choice string) InferOption {
	return func(o *InferOptions) { o.ToolChoice = choice }
}
