package mcp

import "github.com/polarisagi/polaris/internal/protocol"

// MCP 工具注册与发现。
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §1

// MCPTransport protocol.MCPTransport 本地别名，使包内调用无需显式引用 protocol 包。
type MCPTransport = protocol.MCPTransport

const (
	MCPStdio          = protocol.MCPStdio
	MCPStreamableHTTP = protocol.MCPStreamableHTTP
	MCPSSE            = protocol.MCPSSE
)

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
