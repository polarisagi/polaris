package protocol

import "encoding/json"

// ============================================================================
// M7 Extensions — Plugin, Skill, MCP, Marketplace 模型
// ============================================================================

// RegistryEntry 插件目录条目（ADR-0016：Publisher/TrustTier/Type 字段）。
type RegistryEntry struct {
	// ID 全局唯一 slug，格式："{publisher}/{name}" 或 "mcp/{name}"
	ID        string `json:"id" yaml:"id"`
	Publisher string `json:"publisher" yaml:"publisher"`
	// Type "mcp" | "skill" | "plugin" | "app"
	Type      string `json:"type" yaml:"type"`
	TrustTier int    `json:"trust_tier" yaml:"trust_tier"`

	Name        string            `json:"name" yaml:"name"`
	Description string            `json:"description" yaml:"description"`
	Transport   string            `json:"transport,omitempty" yaml:"transport,omitempty"`
	Command     string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args        []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	URL         string            `json:"url,omitempty" yaml:"url,omitempty"`
	Tags        []string          `json:"tags" yaml:"tags"`
	Homepage    string            `json:"homepage,omitempty" yaml:"homepage,omitempty"`
	Timeout     int               `json:"timeout" yaml:"timeout"`
	// UI 展示元数据（来自 interface 块 或 agents/openai.yaml）
	DisplayName      string `json:"display_name,omitempty" yaml:"display_name,omitempty"`
	ShortDescription string `json:"short_description,omitempty" yaml:"short_description,omitempty"`
	Icon             string `json:"icon,omitempty" yaml:"icon,omitempty"`
	// 运行时叠加：是否已安装（extension_instances 表中存在同 catalog_id）
	Installed bool `json:"installed" yaml:"installed"`
	// 版本标识，若原数据无则自动填充为所在 repo 的 commit hash前缀
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	// 运行时叠加：本地已安装的版本标识
	InstalledVersion string `json:"installed_version,omitempty" yaml:"-"`
	// 运行时叠加：所属市场的排序权重（用于列表展示）
	MarketplaceSortOrder int `json:"marketplace_sort_order,omitempty" yaml:"-"`
}

// Marketplace 市场配置。
type Marketplace struct {
	ID          string `json:"id" yaml:"id"`
	Name        string `json:"name" yaml:"name"`
	Type        string `json:"type" yaml:"type"`
	Publisher   string `json:"publisher" yaml:"publisher"`
	RepoURL     string `json:"repo_url" yaml:"repo_url"`
	Description string `json:"description" yaml:"description"`
	IsBuiltin   int    `json:"is_builtin" yaml:"is_builtin"`
	TrustTier   int    `json:"trust_tier" yaml:"trust_tier"`
	Enabled     int    `json:"enabled" yaml:"enabled"`
	SortOrder   int    `json:"sort_order" yaml:"sort_order"` // 展示排序权重，值越小越靠前
	CreatedAt   string `json:"created_at" yaml:"created_at"`
}

// PluginInterface 对应 plugin.json 的 interface 块（UI 展示元数据）。
// 兼容 Polaris .codex-plugin/plugin.json 和 OpenAI agents/openai.yaml 的 interface 节。
type PluginInterface struct {
	DisplayName      string   `json:"displayName,omitempty" yaml:"display_name,omitempty"`
	ShortDescription string   `json:"shortDescription,omitempty" yaml:"short_description,omitempty"`
	LongDescription  string   `json:"longDescription,omitempty" yaml:"long_description,omitempty"`
	DeveloperName    string   `json:"developerName,omitempty" yaml:"developer_name,omitempty"`
	Category         string   `json:"category,omitempty" yaml:"category,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	IconSmall        string   `json:"icon_small,omitempty" yaml:"icon_small,omitempty"` // agents/openai.yaml 用下划线
	WebsiteURL       string   `json:"websiteURL,omitempty" yaml:"website_url,omitempty"`
	PrivacyPolicyURL string   `json:"privacyPolicyURL,omitempty" yaml:"privacy_policy_url,omitempty"`
	TermsURL         string   `json:"termsOfServiceURL,omitempty" yaml:"terms_url,omitempty"`
	DefaultPrompt    []string `json:"defaultPrompt,omitempty" yaml:"default_prompt,omitempty"`
}

// PluginJSON 表示 .codex-plugin/plugin.json 或 .claude-plugin/plugin.json 的完整清单格式。
// 字段集覆盖 OpenAI Codex / Anthropic Claude Code 两家标准 plugin.json。
type PluginJSON struct {
	Name        string           `json:"name"`
	Version     string           `json:"version"`
	Description string           `json:"description"`
	Author      any              `json:"author,omitempty"` // Change to any to support both string and object
	Homepage    string           `json:"homepage,omitempty"`
	Repository  string           `json:"repository,omitempty"`
	License     string           `json:"license,omitempty"`
	Keywords    []string         `json:"keywords,omitempty"`
	MCPServers  string           `json:"mcpServers,omitempty"` // 指向 .mcp.json 的相对路径
	Interface   *PluginInterface `json:"interface,omitempty"`  // UI 展示元数据
}

// MCPServerDef 定义单个 MCP Server 配置。
// 字段兼容：Claude Code .mcp.json / OpenAI Codex .mcp.json / Anthropic Messages API。
//   - "stdio"（默认）: 本地进程，使用 Command/Args/Env
//   - "http" / "streamable-http": 远端 HTTP，使用 URL/Headers
//   - "sse": 已废弃（Anthropic 2026-04-01 停止接受），仍保留兼容
type MCPServerDef struct {
	Type               string            `json:"type,omitempty"`    // "stdio"|"http"|"streamable-http"|"sse"
	Command            string            `json:"command,omitempty"` // stdio 专用
	Args               []string          `json:"args,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	URL                string            `json:"url,omitempty"`                 // http/sse 专用
	Headers            map[string]string `json:"headers,omitempty"`             // http/sse 专用，Bearer 等
	AuthorizationToken string            `json:"authorization_token,omitempty"` // Anthropic Messages API 远程 MCP 鉴权
}

// MCPConfig 表示 .mcp.json 的结构（三家标准统一使用 mcpServers camelCase）。
type MCPConfig struct {
	MCPServers      map[string]MCPServerDef `json:"mcpServers"`
	MCPServersSnake map[string]MCPServerDef `json:"mcp_servers"` // 兼容历史格式
}

// PluginBundleManifest 是多组件 Bundle 的扩展 plugin.json（M13-bis §2.1）。
//
// JSON "skills" 字段兼容两种格式：
//   - 字符串路径（Codex 标准）："skills": "./skills/"  → SkillsDir
//   - 对象数组（Polaris 扩展）："skills": [{"path":"..."}]  → Skills
//
// JSON "hooks" 字段兼容两种格式：
//   - 字符串路径（Codex 标准）："hooks": "./hooks/hooks.json"  → HooksFile
//   - 映射（Polaris 扩展，install/uninstall）："hooks": {"install":"..."}  → Hooks
type PluginBundleManifest struct {
	Name           string                  `json:"name"`
	Version        string                  `json:"version"`
	Description    string                  `json:"description"`
	Entrypoint     string                  `json:"entrypoint,omitempty"`
	MCPFile        string                  `json:"mcpServers,omitempty"` // 指向 .mcp.json 的相对路径
	MCPFileSnake   string                  `json:"mcp_servers,omitempty"`
	MCPInline      map[string]MCPServerDef `json:"mcpInline,omitempty"` // 内联 MCP 服务器映射
	MCPInlineSnake map[string]MCPServerDef `json:"mcp_inline,omitempty"`
	Interface      *PluginInterface        `json:"interface,omitempty"` // UI 展示元数据

	// Skills: 由 UnmarshalJSON 从 "skills" 字段解析，见下方注释。
	Skills    []BundleSkillRef // array form: [{"path":"...","name":"..."}]
	SkillsDir string           // string form: "./skills/"
	// Hooks: 由 UnmarshalJSON 从 "hooks" 字段解析。
	Hooks     map[string]string // map form: {"install":"path","uninstall":"path"}
	HooksFile string            // string form: "./hooks/hooks.json"
}

// bundleManifestWire 是 PluginBundleManifest 的 JSON 解码中间结构，
// 用于处理 "skills" 和 "hooks" 字段的多态性。
type bundleManifestWire struct {
	Name           string                  `json:"name"`
	Version        string                  `json:"version"`
	Description    string                  `json:"description"`
	Entrypoint     string                  `json:"entrypoint,omitempty"`
	MCPFile        string                  `json:"mcpServers,omitempty"`
	MCPFileSnake   string                  `json:"mcp_servers,omitempty"`
	MCPInline      map[string]MCPServerDef `json:"mcpInline,omitempty"`
	MCPInlineSnake map[string]MCPServerDef `json:"mcp_inline,omitempty"`
	Interface      *PluginInterface        `json:"interface,omitempty"`
	SkillsRaw      json.RawMessage         `json:"skills,omitempty"`
	HooksRaw       json.RawMessage         `json:"hooks,omitempty"`
}

// UnmarshalJSON 处理 "skills" 和 "hooks" 字段的多态解码：
//   - "skills" 可以是字符串路径（Codex 标准）或 []BundleSkillRef（Polaris 扩展）
//   - "hooks" 可以是字符串路径（Codex 标准）或 map[string]string（Polaris 扩展）
func (m *PluginBundleManifest) UnmarshalJSON(data []byte) error {
	var w bundleManifestWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	m.Name = w.Name
	m.Version = w.Version
	m.Description = w.Description
	m.Entrypoint = w.Entrypoint
	m.MCPFile = w.MCPFile
	if m.MCPFile == "" && w.MCPFileSnake != "" {
		m.MCPFile = w.MCPFileSnake
	}
	m.MCPInline = w.MCPInline
	if m.MCPInline == nil && w.MCPInlineSnake != nil {
		m.MCPInline = w.MCPInlineSnake
	}
	m.Interface = w.Interface

	// 解析 "skills"：先尝试字符串（Codex 路径形式），再尝试数组（Polaris 形式）
	if len(w.SkillsRaw) > 0 {
		var path string
		if err := json.Unmarshal(w.SkillsRaw, &path); err == nil {
			m.SkillsDir = path
		} else {
			var refs []BundleSkillRef
			if err := json.Unmarshal(w.SkillsRaw, &refs); err == nil {
				m.Skills = refs
			}
		}
	}

	// 解析 "hooks"：先尝试字符串（Codex 路径形式），再尝试 map（Polaris 形式）
	if len(w.HooksRaw) > 0 {
		var hookPath string
		if err := json.Unmarshal(w.HooksRaw, &hookPath); err == nil {
			m.HooksFile = hookPath
		} else {
			var hookMap map[string]string
			if err := json.Unmarshal(w.HooksRaw, &hookMap); err == nil {
				m.Hooks = hookMap
			}
		}
	}

	return nil
}

// BundleSkillRef 引用 Bundle 内的单个技能。
type BundleSkillRef struct {
	Path string `json:"path"` // 相对于 Bundle 根目录的 SKILL.md 路径
	Name string `json:"name,omitempty"`
}

// AppJSON 是 OpenAI Codex .app.json connector/app 映射格式。
// 用于声明插件所包含的 App 或第三方 Connector。
type AppJSON struct {
	Apps []AppDef `json:"apps,omitempty"`
}

// AppDef 单个 App/Connector 定义。
type AppDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`     // HTTP connector 端点
	Command     string `json:"command,omitempty"` // 本地进程命令
}

// AIPluginJSON 是 OpenAI ai-plugin.json 清单格式（ChatGPT Plugins 时代，已逐步被 MCP 取代）。
// https://platform.openai.com/docs/plugins/getting-started/plugin-manifest
type AIPluginJSON struct {
	SchemaVersion       string `json:"schema_version"`
	NameForModel        string `json:"name_for_model"`
	NameForHuman        string `json:"name_for_human"`
	DescriptionForModel string `json:"description_for_model"`
	DescriptionForHuman string `json:"description_for_human"`
	Auth                struct {
		Type string `json:"type"` // "none" | "service_http" | "user_http" | "oauth"
	} `json:"auth"`
	API struct {
		Type string `json:"type"` // "openapi" | "mcp"
		URL  string `json:"url"`
	} `json:"api"`
	LogoURL      string `json:"logo_url"`
	ContactEmail string `json:"contact_email"`
	LegalInfoURL string `json:"legal_info_url"`
}

// AnthropicPluginTOML 是 Anthropic .claude-plugin/plugin.toml 格式。
// struct tag `toml:` 由 go-toml/v2 反射读取，无需在 protocol 包导入 toml 库。
type AnthropicPluginTOML struct {
	Plugin struct {
		Name        string `toml:"name"`
		Description string `toml:"description"`
		Version     string `toml:"version"`
	} `toml:"plugin"`
	MCP struct {
		Command string            `toml:"command"`
		Args    []string          `toml:"args"`
		Env     map[string]string `toml:"env"`
	} `toml:"mcp"`
}

// GoogleSkillsYAML 是 Google Agent Skills manifest 格式（skills.yaml / agent-manifest.yaml）。
type GoogleSkillsYAML struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Version     string   `yaml:"version"`
	Command     string   `yaml:"command,omitempty"`
	Args        []string `yaml:"args,omitempty"`
	Skills      []struct {
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Command     string   `yaml:"command,omitempty"`
		Args        []string `yaml:"args,omitempty"`
	} `yaml:"skills,omitempty"`
}

// PluginInstallRequest 一键安装请求体。
type PluginInstallRequest struct {
	CatalogID string `json:"catalog_id"`
	// 可选覆盖：不传则使用 catalog 默认值
	Name    string            `json:"name,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}
