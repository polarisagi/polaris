package protocol

import (
	"encoding/json"
	"time"
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
}

// MCPClientConfig MCP Server 连接配置。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type MCPClientConfig struct {
	Transport  MCPTransport      // "stdio" | "sse" | "streamable_http"
	Command    string            // stdio: 可执行命令
	Args       []string          // stdio: 命令参数
	Env        map[string]string // stdio: 附加环境变量
	WorkDir    string            // stdio: 进程工作目录；空字符串则继承父进程
	URL        string            // sse / streamable_http: 端点 URL
	Timeout    time.Duration     // 单次请求超时，0 → 30s
	ServerName string            // 用于 TaintPreservingDecoder 溯源
	Trusted    bool              // true → TaintMedium（白名单）；false → TaintHigh
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
}

// MCPUpdateConfig MCP Server 可更新字段。
// @canonical: 此处为唯一定义，extension/mcp 包以 type alias 引用。
type MCPUpdateConfig struct {
	Name      string
	Transport string
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Enabled   bool
	Timeout   int
	TrustTier int
}
