package sysadmin

import (
	"context"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/protocol"
)

// 本文件声明 sysadmin 包对外部模块的消费端接口（Consumer-side Interfaces）。
// 方法集以各 sysadmin/*.go 文件的实际调用点为准。
//
// @consumer: sysadmin/handler.go（字段类型），mcp_servers.go / prompts.go 等调用点
// @producer: 具体实现由 gateway/server.go 注入

// MCPManager sysadmin 包对 MCP 管理器的消费端接口。
// 实现：extension/mcp.MCPManager
type MCPManager interface {
	// ListServers 返回所有 MCP 服务器运行时状态。
	ListServers() []mcp.MCPServerInfo
	// Add 注册并启动一个新 MCP 服务器连接。
	Add(ctx context.Context, serverID, name string, cfg mcp.MCPClientConfig) error
	// Remove 注销指定 MCP 服务器连接。
	Remove(serverID string)
	// Update 更新 MCP 服务器配置（替换原有连接）。
	Update(ctx context.Context, extRepo protocol.ExtensionRepository, id string, cfg mcp.MCPUpdateConfig, dataDir string) error
}

// ExtensionInstaller sysadmin 包对扩展安装管理器的消费端接口。
// 实现：extension/marketplace.Manager
type ExtensionInstaller interface {
	// Authorize 校验当前主体是否有权安装该扩展。
	Authorize(ctx context.Context, req marketplace.InstallRequest) error
	// InstallExtension 执行扩展安装流程（含 MCP 注册等后续步骤）。
	InstallExtension(ctx context.Context, req marketplace.InstallRequest) error
}

// LLMRegistry sysadmin 包对 LLM Provider 注册表的消费端接口。
// 实现：llm.ProviderRegistry
type LLMRegistry interface {
	// PickProvider 按角色名选取最优 Provider（default / general 等）。
	PickProvider(role string) protocol.Provider
}

// PromptManager sysadmin 包对提示词管理器的消费端接口。
// 实现：prompt.Manager
type PromptManager interface {
	// LoadSoulMD 加载 soul.md（系统人格提示词，三层优先级：用户覆盖 > 嵌入默认）。
	LoadSoulMD() string
	// ReadPrompt 读取指定提示词（优先用户覆盖，fallback 为内置默认）。
	ReadPrompt(name, fallback string) string
	// ReadPromptDefault 读取内置默认提示词（忽略用户覆盖）。
	ReadPromptDefault(name string) string
	// WriteUserPrompt 持久化用户自定义提示词（覆盖同名内置）。
	WriteUserPrompt(name, content string) error
	// DeleteUserPrompt 删除用户自定义提示词（回落内置）。
	DeleteUserPrompt(name string) error
}
