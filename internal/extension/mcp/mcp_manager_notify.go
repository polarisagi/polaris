package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// NotificationSink 服务端通知的消费方（Claude channel 绑定服务，ADR-0103 决策三）。在 MCP
// 读循环 goroutine 中同步调用：实现方不得阻塞（耗时处理须自行异步）。
type NotificationSink func(serverID string, meta ServerMeta, method string, params json.RawMessage)

// SetNotificationSink 注册服务端通知出口（对之后连接的服务器生效；启动装配时先于 StartFromDB 调用）。
func (m *MCPManager) SetNotificationSink(sink NotificationSink) { m.notificationSink.Store(&sink) }

// attachNotificationSink 装配 MCPClient 唯一的服务端通知回调。工具列表变更通知
// （notificationToolsListChanged）由 manager 自己消费触发 refreshTools 后直接返回，不转交
// sink——旧纪元没有订阅机制，收到即处理；新纪元 stdio 传输的订阅通知也走这条 dispatch
// 管道到达，须先经 shouldConsumeToolsListChanged 核对 _meta.subscriptionId（新纪元
// Streamable HTTP 传输的订阅走独立读取路径，见 mcp_client_subscribe.go，根本不经过
// 这里）。其余通知类型原样转交 sink，链路不受影响。
func (m *MCPManager) attachNotificationSink(serverID string, client *MCPClient) {
	client.SetNotificationHandler(func(method string, params json.RawMessage) {
		if method == notificationToolsListChanged && client.shouldConsumeToolsListChanged(params) {
			// refreshTools 内部会同步做 ListTools 网络调用，独立 goroutine 触发避免
			// 阻塞本次 dispatch（读循环 goroutine，见 mcp_client_serverreq.go dispatch）。
			concurrent.SafeGo(context.Background(), "mcp_manager.tools_list_changed", func(context.Context) {
				if err := m.refreshTools(context.Background(), serverID); err != nil {
					// 失败已在 refreshTools 内部记录 Warn；这里用 Debug 标注触发源
					// （HE-1：禁止 `_ = ` 丢弃错误）。
					slog.Debug("mcp: notification-triggered refresh failed", "server", serverID, "err", err)
				}
			})
			return
		}
		if sink := m.notificationSink.Load(); sink != nil && *sink != nil {
			(*sink)(serverID, client.ServerMeta(), method, params)
		}
	})
}

// ServerMeta 返回已连接服务器在 initialize 中声明的元数据。
func (m *MCPManager) ServerMeta(serverID string) (ServerMeta, bool) {
	m.mu.RLock()
	e, ok := m.entries[serverID]
	m.mu.RUnlock()
	if !ok || e.client == nil {
		return ServerMeta{}, false
	}
	return e.client.ServerMeta(), true
}

// NotifyServer 向已连接服务器发送通知。
func (m *MCPManager) NotifyServer(ctx context.Context, serverID, method string, params any) error {
	m.mu.RLock()
	e, ok := m.entries[serverID]
	m.mu.RUnlock()
	if !ok || e.client == nil {
		return apperr.New(apperr.CodeNotFound, "mcp: server not connected: "+serverID)
	}
	if err := e.client.Notify(ctx, method, params); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "mcp: notify "+serverID, err)
	}
	return nil
}
