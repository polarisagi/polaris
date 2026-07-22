package types

import "time"

type

// StoreCapabilities 存储引擎的能力声明（供 StorageRouter 路由决策）。
StoreCapabilities struct {
	SupportsSQL       bool
	SupportsVector    bool
	SupportsGraph     bool
	SupportsFullText  bool
	SupportsStreaming bool
	Engine            string
}

type

// Op 单条批量写操作（Put / Delete）。
Op struct {
	Key   []byte
	Value []byte
	Type  OpType
}

type

// RetryPolicy 工具调用重试策略。
RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
}

type

// SafetyRule 安全规则条目（ImmutableCore 的一条记录）。
SafetyRule struct {
	RuleText string
	Severity string // info / warn / block
	Scope    string
}

type

// DocumentRef 外部文档引用（Connector 返回的文档列表条目）。
DocumentRef struct {
	URI         string
	Title       string
	SourceType  string // markdown | pdf | code | web | notion_page | gdoc
	ContentHash string
	ModifiedAt  int64
	Metadata    map[string]any
	Size        int64
}

type

// SyncDocument 拉取到的文档内容。
SyncDocument struct {
	URI      string
	Title    string
	Content  []byte
	Metadata map[string]string
}

type

// SyncConfig 数据源同步配置声明。
SyncConfig struct {
	DefaultInterval int  // seconds
	SupportsWatch   bool // 是否支持基于事件的 Watch 模式
	MaxBatchSize    int
}

type

// PolicyReviewRequest 策略评审请求（Cedar 策略引擎入参）。
PolicyReviewRequest struct {
	Principal string
	Action    string
	Resource  string
	Context   map[string]any
}

type

// PolicyReviewResult 策略评审结果（Cedar 策略引擎出参）。
PolicyReviewResult struct {
	Allowed bool
	Reason  string
	Etag    string
}

type

// HITLPrompt 人工审批请求（HITL 网关发出）。
//
// 2026-07-07 补齐 json tag（此前无 tag 时按 Go 字段名原样序列化，如 "ID"/
// "CheckpointType"，与 Web UI `/v1/approvals/pending` 消费方期望的 snake_case
// 完全对不上，审批页面的风险徽章/倒计时长期显示异常，见 web/src/js/store/
// approvals.js、web/src/pages/automation.html）。
HITLPrompt struct {
	ID string `json:"id"`
	// AgentID 发起审批的 Agent 标识，仅在真实 Agent 执行流程中可得
	// （如 interceptComputerUse/provider_exhausted）；扩展安装审查、
	// automation 预执行等场景无对应概念时留空，前端应据此隐藏该行而非
	// 显示 "undefined"。
	AgentID        string       `json:"agent_id,omitempty"`
	CheckpointType string       `json:"checkpoint_type"`
	PromptText     string       `json:"prompt_text"`
	Options        []HITLOption `json:"options,omitempty"`
	// DeadlineNs 绝对 Unix 纳秒时间戳（非相对 Duration），构造方式统一为
	// time.Now().Add(N).UnixNano()。前端可直接 `deadline_ns/1e6 - Date.now()`
	// 算剩余毫秒数，无需额外的 created_at/timeout_ms 字段。
	DeadlineNs int64      `json:"deadline_ns,omitempty"`
	RiskLevel  int        `json:"risk_level"`
	TaintLevel TaintLevel `json:"taint_level,omitempty"`
	// DecisionEtag 决策时刻的 Cedar policy etag，auto_approve 前校验原子性。
	DecisionEtag string `json:"decision_etag,omitempty"`
	// EligibleApproveTime 最早允许审批的时间，用于 L3 强制冷却期（Task 21）
	EligibleApproveTime int64 `json:"eligible_approve_time,omitempty"`
	// PermissionMode 仅电脑操控类 checkpoint（CheckpointDeviceControlReview）填充，
	// 记录发起 HITL 时用户在"设置 → 设备操控"选择的权限模式，供超时兜底
	// （resolveTimeoutAction）区分处理。零值表示"不适用"——非电脑操控类
	// checkpoint（扩展安装审查/L3-L4 自改进晋升/自动化预执行等）必须忽略此字段，
	// 不得让设备操控偏好影响与之无关的信任/合规判断（M13 §2.4 权限模式联动）。
	PermissionMode PermissionMode `json:"permission_mode,omitempty"`
	// ExemptionFieldContent 是 M04 §3 TaintBlocked→HITL 审批→颁发豁免令牌 转义
	// 路径专用字段：CheckpointType=="data_exfiltration" 时携带被拦截的原始字节
	// （由 policy.TaintEgressBlockedError.Data 提取），供审批通过后
	// automation/hitl.GatewayImpl.Respond 铸造 TaintExemptionToken——豁免令牌的
	// 哈希必须精确匹配被拦截的内容，不能用 PromptText 的人类可读摘要代替。
	// 非该转义场景的 HITLPrompt 一律留空。
	ExemptionFieldContent []byte `json:"exemption_field_content,omitempty"`
}

type

// HITLOption HITL 审批选项。
HITLOption struct {
	Key   string
	Label string
}

// HITLResponse 用户对 HITL 审批的响应。
type HITLResponse struct {
	OptionKey string
	UserID    string
	Approved  bool
	Reason    string
}

// WithThinkingMode 设置思考模式。
func WithThinkingMode(mode ThinkingMode) InferOption {
	return func(o *InferOptions) { o.ThinkingMode = mode }
}

// WithMaxTokens 设置最大输出 token 数。
func WithMaxTokens(n int) InferOption {
	return func(o *InferOptions) { o.MaxTokens = n }
}

// WithModel 设置覆盖使用的 Model。
func WithModel(model string) InferOption {
	return func(o *InferOptions) { o.Model = model }
}

type

// DecisionLogEntry 对应 006_decision_log.sql 表的数据结构
DecisionLogEntry struct {
	SessionID    string
	AgentID      string
	DecisionType string
	Context      []byte // JSON
	Choice       string
	Alternatives []byte // JSON
	Reason       string
	Outcome      []byte // JSON
}

// WithTemperature 设置温度
func WithTemperature(temp float64) InferOption {
	return func(o *InferOptions) { o.Temperature = temp }
}

// WithTopP 设置 TopP
func WithTopP(topP float64) InferOption {
	return func(o *InferOptions) { o.TopP = topP }
}
