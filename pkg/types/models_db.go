package types

type

// ChatSessionRow 对应 chat_sessions 表一行。
ChatSessionRow struct {
	ID             string
	Title          string
	ThrashingIndex float64
	CreatedAt      string
	UpdatedAt      string
	MessageCount   int
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
	Enabled         bool
	Timeout         int
	TrustTier       int
	CatalogID       string
	PluginID        string
	WorkDir         string
	RequiresNetwork bool
	CreatedAt       string
	UpdatedAt       string
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

// AppRow 对应 apps 表一行（自定义 App）。
AppRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Publisher   string `json:"publisher"`
	Enabled     bool   `json:"enabled"`
	TrustTier   int    `json:"trust_tier"`
	CatalogID   string `json:"catalog_id"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
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
