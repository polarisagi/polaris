package server

import (
	"context"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
)

// 本文件声明 gateway/server 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// 所有接口方法均以实际调用点为准（grep 溯源，非臆测）。
// Phase 3 完成：server.go 字段已全部替换为接口类型，具体 struct 仅在 cmd/polaris 层持有。
//
// @consumer: server.go（字段类型），server/chat, server/plugin, server/sysadmin（共享超集）
// @producer: 各具体模块由 cmd/polaris/boot_server.go 注入

// LLMRegistry server 包对 LLM Provider 注册表的消费端接口（超集）。
// 实现：llm.ProviderRegistry
// 注：server.go 将同一 registry 同时传给 provider.ProviderHandler（需 Register*）
//
//	和 chat.ChatHandler（需 PickProviderByRecordID），因此 server 层超集包含所有方法。
type LLMRegistry interface {
	// PickProvider 按角色选取最优 Provider（返回 nil 表示无可用 Provider）。
	PickProvider(role string) protocol.Provider
	// PickProviderName 按角色返回最优 Provider 的注册名（用于日志/遥测）。
	PickProviderName(role string) string
	// PickProviderByRecordID 按 provider_models.id 精确选取（用户手动选模型时调用）。
	PickProviderByRecordID(mID string) protocol.Provider
	// UnregisterAll 清空所有已注册 Provider（DB 热重载前调用）。
	UnregisterAll()
	// RegisterWithRole 注册一个 Provider，绑定路由角色。
	RegisterWithRole(name, displayName, role string, p protocol.Provider)
}

// ChannelStarter server 包对聊天平台管理器的消费端接口（超集）。
// 实现：channel.Manager
// server.go 将同一实例分别用于启动/停止（LoadFromDB/StopAll）
// 和 sysadmin.ChannelMgr（SendReply/Start/Stop），因此 server 层接口为超集。
type ChannelStarter interface {
	// LoadFromDB 从数据库加载并启动所有已配置平台的 poller（Server.Start 时调用）。
	LoadFromDB(db protocol.SQLQuerier)
	// StopAll 停止所有平台 poller（优雅退出时调用）。
	StopAll()
	// Start 启动指定平台 poller（运行时新增频道时调用）。
	Start(channelType, channelID string, cfg map[string]any)
	// Stop 停止指定平台 poller（频道禁用/删除时调用）。
	Stop(channelID string)
	// SendReply 向指定频道回复消息（sysadmin 工作流响应时调用）。
	SendReply(ctx context.Context, channelType, channelID string, cfg map[string]any, msg cadapter.Message, text string)
}

// MCPManager server 包对 MCP 服务器管理器的消费端接口（超集，覆盖所有子 handler 调用点）。
// 实现：extension/mcp.MCPManager
// 子 handler 各自声明自己需要的方法子集（见 chat/provider.go 等）。
type MCPManager interface {
	// ListServers 返回所有 MCP 服务器运行时状态快照。
	ListServers() []protocol.MCPServerInfo
	// Add 连接并注册一个 MCP 服务器（热插拔，工具自动注入 Catalog）。
	Add(ctx context.Context, serverID, name string, cfg protocol.MCPClientConfig) error
	// Remove 断开并注销一个 MCP 服务器（级联清理工具注册）。
	Remove(serverID string)
	// Update 更新 MCP 服务器配置（断开旧连接 → 应用新配置 → 重连）。
	Update(ctx context.Context, extRepo protocol.ExtensionRepository, id string, cfg protocol.MCPUpdateConfig, dataDir string) error
	// ApproveNetworkAccess 设置服务器网络访问审批并重启连接（approved=true 放行网络）。
	ApproveNetworkAccess(ctx context.Context, serverID string, extRepo protocol.ExtensionRepository, dataDir string, approved bool) error
	// IsPluginConnected 检查指定 Plugin 的 MCP 进程是否在线。
	IsPluginConnected(pluginID string) bool
	// SetOnToolsChanged 注册工具集变更回调（插件 MCP 连接完成时触发缓存失效）。
	SetOnToolsChanged(fn func())
}

// ExtensionInstaller server 包对扩展安装管理器的消费端接口（超集，覆盖所有子 handler 调用点）。
// 实现：extension/marketplace.Manager
type ExtensionInstaller interface {
	// Authorize 校验当前主体是否有权安装该扩展（Cedar 三层防线）。
	Authorize(ctx context.Context, req marketplace.InstallRequest) error
	// AuthorizeAction 校验任意动作权限（如 "plugin:manage"）。
	AuthorizeAction(ctx context.Context, principal string, action string, target any) error
	// InstallExtension 执行扩展安装流程（lifecycle FSM → 下载 → DB 记录）。
	InstallExtension(ctx context.Context, req marketplace.InstallRequest) error
	// UninstallExtension 卸载指定扩展（级联删除：DB + 沙箱资源）。
	UninstallExtension(ctx context.Context, catalogID string) error
	// UpdateInstance 更新扩展实例元数据（状态/错误信息/安装路径）。
	UpdateInstance(ctx context.Context, id string, upd marketplace.InstanceUpdate) error
}

// PromptManager server 包对提示词管理器的消费端接口（超集，覆盖所有子 handler 调用点）。
// 实现：prompt.Manager
type PromptManager interface {
	// LoadSoulMD 加载 SOUL.md（用户定制人格文件，server.go Start 时调用）。
	LoadSoulMD() string
	// ReadPrompt 读取指定提示词（优先用户覆盖，回退内置）。
	ReadPrompt(name, fallback string) string
	// ReadPromptDefault 只读取内置默认值，忽略用户文件。
	ReadPromptDefault(name string) string
	// WriteUserPrompt 写入用户自定义提示词（持久化到 configDir）。
	WriteUserPrompt(name, content string) error
	// DeleteUserPrompt 删除用户自定义提示词（回退内置）。
	DeleteUserPrompt(name string) error
	// ModelSpecificGuidance 返回 modelID 对应的模型专属引导文本。
	ModelSpecificGuidance(modelID string) string
	// PlatformHintFor 返回指定接入平台的提示词片段。
	PlatformHintFor(platform string) string
}

// OTAUpdater server 包对 OTA 自更新管理器的消费端接口。
// 实现：sysmgr/updater.Manager（nil 时禁用自动更新）
type OTAUpdater interface {
	// CheckUpdate 检查是否有新版本可用。
	CheckUpdate(ctx context.Context) (hasUpdate bool, version string, err error)
	// Apply 下载并应用更新（需系统重启生效）。
	Apply(ctx context.Context) error
}

// STTProvider server 包对语音识别的消费端接口。
// 实现：llm/stt.SherpaSTT（nil 时功能不可用）
type STTProvider interface {
	Transcribe(ctx context.Context, audioWAV []byte) (string, error)
	IsAvailable() bool
}

// TTSProvider server 包对语音合成的消费端接口。
// 实现：llm/tts.TTSEngine（nil 时功能不可用）
type TTSProvider interface {
	Synthesize(ctx context.Context, text, voice string) ([]byte, error)
	IsAvailable() bool
}
