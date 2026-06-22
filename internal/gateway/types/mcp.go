package types

type MCPServerConfig struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Transport  string            `json:"transport"` // "stdio" | "sse" | "streamable_http"
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        string            `json:"url,omitempty"`
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
}
