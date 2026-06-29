package chat

import (
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/protocol"
)

// 本文件声明 chat 包对外部模块的消费端接口（Consumer-side Interfaces）。
// 方法集以 chat/handler.go + chat/sse.go 实际调用点为准。
//
// @consumer: chat/handler.go（字段类型），chat/sse.go（调用点）
// @producer: 具体实现由 gateway/server.go（Server.SetMCPManager 等）注入

// MCPManager chat 包对 MCP 管理器的消费端接口（仅 chat 实际调用的方法）。
// 实现：extension/mcp.MCPManager（满足此接口的方法子集）
type MCPManager interface {
	// ListServers 返回所有 MCP 服务器运行时状态（供 SSE 推送连接状态）。
	ListServers() []mcp.MCPServerInfo
	// IsPluginConnected 检查指定 Plugin 的 MCP 进程是否在线。
	IsPluginConnected(pluginID string) bool
}

// LLMRegistry chat 包对 LLM Provider 注册表的消费端接口。
// 实现：llm.ProviderRegistry
type LLMRegistry interface {
	PickProvider(role string) protocol.Provider
	PickProviderName(role string) string
	// PickProviderByRecordID 按 provider_models.id 精确选取（用户手动指定模型时调用）。
	PickProviderByRecordID(mID string) protocol.Provider
}

// PromptManager chat 包对提示词管理器的消费端接口（仅 chat/sse.go 实际调用的方法）。
// 实现：prompt.Manager
type PromptManager interface {
	// ReadPrompt 读取指定提示词（优先用户覆盖，回退内置）。
	ReadPrompt(name, fallback string) string
	// ModelSpecificGuidance 返回 modelID 对应的模型专属引导文本。
	ModelSpecificGuidance(modelID string) string
	// PlatformHintFor 返回指定接入平台的提示词片段。
	PlatformHintFor(platform string) string
}
