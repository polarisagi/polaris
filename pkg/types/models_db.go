package types

type

// ChatSessionRow 对应 chat_sessions 表一行。
ChatSessionRow struct {
	ID    string
	Title string
	// ProjectID 所属项目（ADR-0097）。空串在写入侧按 "default" 处理。
	ProjectID      string
	ThrashingIndex float64
	CreatedAt      string
	UpdatedAt      string
	MessageCount   int
}

type

// ProjectRow 对应 projects 表一行（ADR-0097）。
ProjectRow struct {
	ID           string
	Name         string
	RootPath     string // 规范路径；空串 = 无目录项目
	Instructions string
	Trusted      bool
	Archived     bool
	CreatedAt    string
	UpdatedAt    string
	SessionCount int // 仅列表查询填充
}

type

// ChatMessageRow 对应 chat_messages 表一行。
ChatMessageRow struct {
	ID               int64
	SessionID        string
	Role             string
	Content          string
	ReasoningContent string
	ToolCalls        string
	FileOffset       int64
	FileLength       int64
	CreatedAt        string
	UpdatedAt        string
	// DedupeKey 幂等键（GD-13-004 复核修复）：SaveMessage 每次调用生成，
	// AppendMessageIdempotent 据此做 INSERT OR IGNORE，供 outbox 重试兜底路径
	// 使用；直接同步写入路径可留空（历史行为不变）。
	DedupeKey string
}

type

// ProviderRow 对应 providers 表一行。
ProviderRow struct {
	ID        string
	Name      string
	Type      string
	BaseURL   string
	APIKey    string
	ProjectID string
	Location  string
	SAKeyJSON string
	Enabled   bool
	CatalogID string
	CreatedAt string
	UpdatedAt string
}

type

// ProviderModelRow 对应 provider_models 表一行。
ProviderModelRow struct {
	ID         string
	ProviderID string
	ModelID    string
	Name       string
	Role       string
	Enabled    bool
	CreatedAt  string
	UpdatedAt  string
}

type

// CronJobRow 对应 cron_jobs 表一行。
CronJobRow struct {
	ID              string
	Name            string
	Prompt          string
	Schedule        string
	SessionID       string
	Enabled         bool
	LastRunAt       string
	NextRunAt       string
	FailureCount    int
	CircuitOpen     bool
	LastError       string
	CircuitOpenedAt string
	CreatedAt       string
}

type

// ExtInstanceRow 对应 extension_instances 表一行。
ExtInstanceRow struct {
	ID               string
	ExtType          string
	Origin           string
	CatalogID        string
	Name             string
	InstalledVersion string
	Publisher        string
	TrustTier        int
	RuntimeID        string
	InstallPath      string
	Config           string
	Status           string
	ErrorMsg         string
	CreatedAt        string
	UpdatedAt        string
}

type

// ExtCatalogRow 对应 extension_catalog 表一行。
ExtCatalogRow struct {
	ID            string
	MarketplaceID string
	Type          string
	Name          string
	Description   string
	Publisher     string
	TrustTier     int
	URL           string
	Version       string
	Payload       string
	UpdatedAt     string
}

type

// MCPServerRow 对应 mcp_servers 表一行。
MCPServerRow struct {
	ID              string
	Name            string
	Transport       string
	Command         string
	Args            string
	Env             string
	URL             string
	Headers         string // JSON object；远程传输请求头，可含 ${user_config.*} 占位
	Enabled         bool
	Timeout         int
	TrustTier       int
	CatalogID       string
	PluginID        string
	WorkDir         string
	RequiresNetwork bool
	// OAuth 预注册 OAuth 客户端配置（JSON object）：{"client_id","client_secret_enc",
	// "auth_server_metadata_url","scopes":[...]}。client_secret 经 credential.Vault 加密后
	// 存 client_secret_enc，不落明文（basic_authorization_client-registration.md §Pre-registration）。
	OAuth     string
	CreatedAt string
	UpdatedAt string
}

type

// MCPOAuthClientRow 对应 mcp_oauth_clients 表一行：Dynamic Client Registration（DCR）
// 得到的客户端，主键 (Issuer, RedirectURI)——授权服务器变更时天然不会复用旧客户端
// （basic_authorization_client-registration.md §Authorization Server Binding）。
MCPOAuthClientRow struct {
	Issuer             string
	RedirectURI        string
	ClientID           string
	ClientSecretEnc    string // credential.Vault 密文；公开客户端（token_endpoint_auth_method=none）为空
	RegistrationMethod string // 目前恒为 "dcr"
	CreatedAt          string
}

type

// MCPOAuthTokenRow 对应 mcp_oauth_tokens 表一行：每个 MCP Server 当前持有的令牌（一对一）。
MCPOAuthTokenRow struct {
	ServerID        string
	Issuer          string
	Resource        string
	ClientID        string // 签发该令牌的客户端；刷新请求必须携带
	RedirectURI     string // 与 Issuer 一起定位 DCR 客户端密钥
	AccessTokenEnc  string // credential.Vault 密文
	RefreshTokenEnc string // credential.Vault 密文；可空（AS 未签发 refresh_token 时）
	TokenType       string
	Scopes          string // 空格分隔，已请求 scope 集合的并集（用于 step-up 取并集）
	ExpiresAt       string // RFC3339；空串=未知/不过期
	UpdatedAt       string
}

type

// PluginUserConfigRow 对应 plugin_user_config 表一行（021）。Value 为 JSON 编码值；
// Sensitive 时 Value 是 credential.Vault 密文。
PluginUserConfigRow struct {
	PluginID  string
	Scope     string // '' 插件级；非空为 channel 绑定的 MCP 服务器名
	Key       string
	Value     string
	Sensitive bool
}

type

// PluginRow 对应 plugins 表一行（021）。Manifest 为 pluginspec 归一化模型快照（含诊断）。
PluginRow struct {
	ID          string
	Name        string
	Version     string
	DisplayName string
	Description string
	Publisher   string
	Homepage    string
	InstallPath string
	Enabled     bool
	TrustTier   int
	CatalogID   string
	MCPPolicy   string
	Manifest    string
	CreatedAt   string
	UpdatedAt   string
}

type

// AuditEventRow 审计日志单条记录。
AuditEventRow struct {
	ID        string
	Action    string
	Actor     string
	Resource  string
	Meta      string // JSON
	CreatedAt string
}

type

// TokenCostAgg 按任务聚合的 Token 费用统计。
TokenCostAgg struct {
	Pool         string
	TotalInput   int64
	TotalOutput  int64
	TotalCacheRd int64
	TotalCostUSD float64
}

// PluginChannelState 对应 plugin_channels 表一行（021）：插件 channel 的用户启用状态。
type PluginChannelState struct {
	PluginID        string
	Server          string
	Enabled         bool
	PermissionRelay bool
}

// PluginAppBinding 对应 plugin_app_bindings 表一行（021）：Codex 应用绑定。
type PluginAppBinding struct {
	PluginID      string `json:"plugin_id"`
	Alias         string `json:"alias"`
	ConnectorRef  string `json:"connector_ref"`
	BoundServerID string `json:"bound_server_id"`
	Status        string `json:"status"` // bound / unbound
}
