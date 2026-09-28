package protocol

import (
	"context"
	"encoding/json"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// MCPTransport MCP 传输层枚举。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type MCPTransport string

const (
	MCPStdio          MCPTransport = "stdio"
	MCPStreamableHTTP MCPTransport = "streamable_http"
	MCPSSE            MCPTransport = "sse"
)

// MCPTool MCP Server 暴露的工具描述。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// Meta 原始 _meta 字段（MCP Apps io.modelcontextprotocol/ui 元数据的解析输入）。
	// 仅用于承载 tools/list 响应原文，解析结果落 UI 字段；toolEqual 据此判定
	// 刷新前后定义是否变化（含 UI 元数据变化）。
	Meta json.RawMessage `json:"_meta,omitempty"`
	// UI 解析后的 MCP Apps 工具级 UI 元数据；nil 表示该工具无 UI 关联
	// （既非声明 resourceUri，也未偏离默认可见性）。解析逻辑见
	// extension/mcp.ParseToolUI（标准/废弃别名/ChatGPT 别名归一化）。
	UI *ToolUI `json:"-"`
}

// ToolUI MCP Apps 工具级 UI 元数据（_meta.ui，apps_spec.mdx §Resource Discovery）。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type ToolUI struct {
	// ResourceURI 关联的 UI 资源 URI，必须以 "ui://" 开头（校验见 ParseToolUI）。
	ResourceURI string
	// Visibility 取值子集 {"model","app"}；nil/空表示未显式声明，按规范默认
	// 视为 ["model","app"]（两端均可见），判定逻辑见 mcp.ToolUIVisibleTo。
	Visibility []string
}

// MCPServerInfo MCP Server 运行时状态快照。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type MCPServerInfo struct {
	ID        string
	Name      string
	Transport string
	Connected bool
	Tools     []MCPTool
	Error     string
	// AuthRequired 该服务器需要用户完成（重新）授权：连接阶段 401、运行期 401 刷新失败、
	// 或运行期 403 insufficient_scope（basic_authorization.md §Error Handling）。
	AuthRequired bool
	// AuthScopes AuthRequired=true 时，挑战给出的所需 scope（用于 UI 提示 / 下一次
	// BeginAuthorization 的 step-up scope 并集）。
	AuthScopes []string
}

// MCPClientConfig MCP Server 连接配置。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type MCPClientConfig struct {
	Transport MCPTransport      // "stdio" | "sse" | "streamable_http"
	Command   string            // stdio: 可执行命令
	Args      []string          // stdio: 命令参数
	Env       map[string]string // stdio: 附加环境变量
	WorkDir   string            // stdio: 进程工作目录；空字符串则继承父进程
	URL       string            // sse / streamable_http: 端点 URL
	// Headers 远程传输的附加请求头（Claude / Codex .mcp.json 的 headers，常用于 Bearer 鉴权）。
	// 值已完成 ${user_config.*} 等变量展开；协议必需头（Content-Type / MCP-Protocol-Version）不可被覆盖。
	Headers    map[string]string
	Timeout    time.Duration // 单次请求超时，0 → 30s
	ServerName string        // 用于 TaintPreservingDecoder 溯源
	Trusted    bool          // true → TaintMedium（白名单）；false → TaintHigh
	// SandboxPolicy 控制 stdio 进程的 OS 级隔离策略。
	// ""（未设置）/ "auto"：按 TrustTier + OS 自动决策（默认安全路径，推荐所有调用方使用）；
	// "none"：唯一的显式退出路径，调用方主动声明不隔离（慎用）；
	// "bwrap"：强制 Linux Bubblewrap，忽略 TrustTier；
	// "seatbelt"：强制 macOS sandbox-exec（已废弃，当前为 no-op）。
	SandboxPolicy string
	// TrustTier 是沙箱策略和污点等级的统一驱动源（ADR-0016 §2.1）。
	// 值来自 mcp_servers.trust_tier：0=Unknown, 1=Local, 2=Community, 3=Official, 4=System/Builtin。
	// TrustTier<=2 → bwrap 断网 + TaintHigh；TrustTier>=3 → 保留网络 + TaintMedium。
	TrustTier int
	// RequiresNetwork 服务器声明需要网络访问（来自 mcp_servers.requires_network）。
	// TrustTier<=2 时默认断网；声明 true 后用户可通过审批接口放行，审批结果写入 preferences。
	RequiresNetwork bool
	// NetworkApproved 由 MCPManager 在 Add() 时按 preferences 表动态解析并写入。
	// true = 用户已批准该服务器的网络访问请求（仅在 RequiresNetwork=true 时有意义）。
	NetworkApproved bool
}

// MCPUpdateConfig MCP Server 可更新字段。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type MCPUpdateConfig struct {
	Name            string
	Transport       string
	Command         string
	Args            []string
	Env             map[string]string
	URL             string
	Headers         map[string]string
	Enabled         bool
	Timeout         int
	TrustTier       int
	RequiresNetwork bool
}

// MCPResource 表示 MCP resources/list 返回的一条资源引用。
type MCPResource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// MCPResourceContent 表示 resources/read 返回的单条内容块。
type MCPResourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
	// Meta 内容级 _meta 原文，MCP Apps UI 资源的 csp/permissions/domain/prefersBorder
	// 均由此解析（apps_spec.mdx §UI Resource Format），见 extension/mcp.ReadUIResource。
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// UICSP MCP Apps UI 资源声明的 CSP 域名白名单（apps_spec.mdx §UI Resource Format
// McpUiResourceCsp）。仅接受 https 源（含 "*." 前缀通配子域），解析时过滤非法项，
// 见 extension/mcp 包内校验逻辑。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type UICSP struct {
	ConnectDomains  []string // fetch/XHR/WebSocket，映射 connect-src
	ResourceDomains []string // 静态资源（图片/脚本/样式/字体/媒体），映射 img-src/script-src/style-src/font-src/media-src
	FrameDomains    []string // 嵌套 iframe，映射 frame-src
	BaseURIDomains  []string // 映射 base-uri
}

// UIPermissions MCP Apps UI 资源请求的沙箱权限（Permission Policy 特性子集）。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type UIPermissions struct {
	Camera         bool
	Microphone     bool
	Geolocation    bool
	ClipboardWrite bool
}

// UIResource 资源读取返回的 MCP Apps UI 内容（HTML 正文 + 安全配置）。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type UIResource struct {
	HTML        string
	MimeType    string
	CSP         UICSP
	Permissions UIPermissions
	// Domain 宿主相关的专属沙箱子域（host-dependent，Polaris 当前不解释此字段，
	// 原样透传给前端宿主）。
	Domain string
	// PrefersBorder nil=宿主自行决定；非 nil 时为服务器显式偏好（apps_spec.mdx
	// §UIResourceMeta prefersBorder）。
	PrefersBorder *bool
}

// MCPClient 表示 MCP 客户端的通用接口。
type MCPClient interface {
	// Initialize 执行 MCP 初始化握手，校验服务器返回的协议版本。
	Initialize(ctx context.Context) error
	// ListTools 查询服务端工具列表。
	ListTools(ctx context.Context) ([]MCPTool, error)
	// ResourcesList 查询服务端资源列表。
	ResourcesList(ctx context.Context) ([]MCPResource, error)
	// ResourcesRead 读取指定 URI 的资源内容。
	ResourcesRead(ctx context.Context, uri string) ([]MCPResourceContent, error)
	// CallTool 调用指定工具并返回文本和图片结果。
	CallTool(ctx context.Context, name string, arguments map[string]any) (string, []types.ImagePart, error)
	// CallToolTainted 调用指定工具并返回文本、图片结果及污点标签。
	CallToolTainted(ctx context.Context, name string, arguments map[string]any) (string, []types.ImagePart, types.TaintLevel, error)
	// Close 关闭客户端连接。
	Close()
}
