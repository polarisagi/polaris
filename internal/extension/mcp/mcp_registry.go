package mcp

// MCP 工具注册与发现。
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §1

// MCPTransport MCP 传输层（统一错误映射）。
type MCPTransport string

const (
	MCPStdio          MCPTransport = "stdio"
	MCPStreamableHTTP MCPTransport = "streamable_http"
	MCPSSE            MCPTransport = "sse"
)

// MCPErrorCode 传输层无关的统一错误码。
type MCPErrorCode string

const (
	MCPConnectionLost    MCPErrorCode = "CONNECTION_LOST"
	MCPConnectionTimeout MCPErrorCode = "CONNECTION_TIMEOUT"
	MCPConnectionFailed  MCPErrorCode = "CONNECTION_FAILED"
	MCPRemoteError       MCPErrorCode = "REMOTE_ERROR"
	MCPRemoteUnavailable MCPErrorCode = "REMOTE_UNAVAILABLE"
	MCPClientError       MCPErrorCode = "CLIENT_ERROR"
)

// MCPRetryPolicy MCP 重试策略。
// CONNECTION_LOST/FAILED/TIMEOUT → 2次指数退避
// CLIENT_ERROR → 0
// REMOTE_ERROR/UNAVAILABLE → 1次
func MCPRetryPolicy(code MCPErrorCode) int {
	switch code {
	case MCPConnectionLost, MCPConnectionFailed, MCPConnectionTimeout:
		return 2
	case MCPRemoteError, MCPRemoteUnavailable:
		return 1
	default:
		return 0
	}
}

// MCPServerConfig MCP Server 连接配置。
type MCPServerConfig struct {
	Name        string
	Command     string
	Args        []string
	Env         map[string]string
	AutoConnect bool
	Timeout     int  // 30s
	Trusted     bool // true → 白名单（TaintMedium）；false → TaintHigh（默认保守）
}

// AgentCard A2A v0.3 Agent 能力声明。
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §2
type A2AAgentCard struct {
	Name               string          `json:"name"`
	Version            string          `json:"version"`
	URL                string          `json:"url"`
	Capabilities       map[string]bool `json:"capabilities"`
	Authentication     map[string]any  `json:"authentication"`
	DefaultInputModes  []string        `json:"defaultInputModes"`
	DefaultOutputModes []string        `json:"defaultOutputModes"`
	Skills             []A2ASkillRef   `json:"skills"`
}

// A2ASkillRef A2A 技能引用。
type A2ASkillRef struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}
