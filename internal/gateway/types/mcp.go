package types

type MCPServerConfig struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Transport  string            `json:"transport"` // "stdio" | "sse" | "streamable_http"
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        string            `json:"url,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"` // 远程传输请求头（如 Authorization）
	Enabled    bool              `json:"enabled"`
	Timeout    int               `json:"timeout"` // 秒
	TrustTier  int               `json:"trust_tier"`
	CatalogID  string            `json:"catalog_id,omitempty"`
	PluginID   string            `json:"plugin_id,omitempty"`
	PluginName string            `json:"plugin_name,omitempty"`
	WorkDir    string            `json:"work_dir,omitempty"`
	CreatedAt  string            `json:"created_at,omitempty"`
	UpdatedAt  string            `json:"updated_at,omitempty"`
	Connected  bool              `json:"connected"`
	ToolCount  int               `json:"tool_count"`
	Error      string            `json:"error,omitempty"`
	// RequiresNetwork 服务器声明需要网络访问（TrustTier<=2 时默认断网，用户可审批放行）。
	RequiresNetwork bool `json:"requires_network"`
	// NetworkApprovalStatus 当前审批状态："pending" | "approved" | "denied"。
	// 仅在 RequiresNetwork=true && TrustTier<=2 时有意义。
	NetworkApprovalStatus string `json:"network_approval_status,omitempty"`
	// AuthRequired 该服务器当前需要（重新）完成 OAuth 授权（8e-2，MCPServerInfo 透传）。
	AuthRequired bool `json:"auth_required"`
	// AuthScopes AuthRequired=true 时挑战给出的所需 scope。
	AuthScopes []string `json:"auth_scopes,omitempty"`
	// OAuthAuthorized 已持有 OAuth 令牌（mcp_oauth_tokens 有行）。与注册方式无关——DCR / CIMD
	// 授权的服务器没有预注册配置，不能靠 oauth 摘要推断是否已授权。
	OAuthAuthorized bool `json:"oauth_authorized"`
	// OAuth 预注册 OAuth 客户端配置摘要（不含明文/密文 secret，只回 has_client_secret）；
	// 未配置预注册时为 nil。
	OAuth *MCPServerOAuthSummary `json:"oauth,omitempty"`
}

// MCPServerOAuthSummary mcp_servers.oauth 列的对外只读摘要（GET /v1/mcp-servers 用）。
type MCPServerOAuthSummary struct {
	ClientID              string   `json:"client_id,omitempty"`
	AuthServerMetadataURL string   `json:"auth_server_metadata_url,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	HasClientSecret       bool     `json:"has_client_secret"`
}
