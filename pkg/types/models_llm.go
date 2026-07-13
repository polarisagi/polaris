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
	MaxContextTokens  int
	CostPer1KInput    float64
	CostPer1KOutput   float64
	CostPer1KCacheHit float64
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
// 注意：不含任何方法，与 internal/protocol/go 中的同名类型语义相同。
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
	Model           string
	Messages        []Message
	Tools           []ToolSchema
	MaxTokens       int
	Temperature     float64
	Thinking        *ThinkingConfig
	ResponseFormat  *ResponseFormat // 支持强制 JSON Schema / GBNF 等结构化约束
	ReasoningEffort ReasoningEffort
	ThinkingMode    ThinkingMode // TTC 推理深度控制（None=不传，High=最大扩展思考）
	ThinkingBudget  int
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
	ThinkingMode    ThinkingMode // 默认 ThinkingDisabled
	MaxTokens       int          // 0 = 使用模型默认值
	Model           string
	Tools           []ToolSchema
	ResponseFormat  *ResponseFormat
	Temperature     float64
	TopP            float64
	ReasoningEffort ReasoningEffort
	ThinkingBudget  int
	CacheHints      *SemanticCacheHints
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
}

// WithResponseFormat 设置响应格式
func WithResponseFormat(fmt *ResponseFormat) InferOption {
	return func(o *InferOptions) { o.ResponseFormat = fmt }
}

// WithSemanticCacheHints 设置语义缓存提示
func WithSemanticCacheHints(hints *SemanticCacheHints) InferOption {
	return func(o *InferOptions) { o.CacheHints = hints }
}
