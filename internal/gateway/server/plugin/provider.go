package plugin

import (
	"context"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
)

// 本文件声明 plugin 包对外部模块的消费端接口（Consumer-side Interfaces）。
// 方法集以 plugin/catalog.go + plugin/manage.go 实际调用点为准。
//
// @consumer: plugin/handler.go（字段类型），plugin/catalog.go, plugin/manage.go
// @producer: 具体实现由 gateway/server.go 注入

// MCPManager plugin 包对 MCP 管理器的消费端接口（仅 plugin 实际调用的方法）。
// 实现：extension/mcp.MCPManager
type MCPManager interface {
	// ListServers 返回所有 MCP 服务器状态（plugin 管理页面展示用）。
	ListServers() []protocol.MCPServerInfo
	// Remove 注销一个 MCP 服务器（plugin 卸载时触发）。
	Remove(serverID string)
}

// ExtensionInstaller plugin 包对扩展安装管理器的消费端接口。
// 实现：extension/marketplace.Manager
type ExtensionInstaller interface {
	// Authorize 校验当前主体是否有权安装该扩展。
	Authorize(ctx context.Context, req marketplace.InstallRequest) error
	// AuthorizeAction 校验任意动作权限（如 "plugin:manage"）。
	AuthorizeAction(ctx context.Context, principal string, action string, target any) error
	// InstallExtension 执行扩展安装流程。
	InstallExtension(ctx context.Context, req marketplace.InstallRequest) error
	// UninstallExtension 卸载指定扩展（级联清理）。
	UninstallExtension(ctx context.Context, catalogID string) error
	// UpdateInstance 更新扩展实例元数据（状态/错误信息）。
	UpdateInstance(ctx context.Context, id string, upd marketplace.InstanceUpdate) error
}
