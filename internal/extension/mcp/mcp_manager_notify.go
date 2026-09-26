package mcp

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// NotificationSink 服务端通知的消费方（Claude channel 绑定服务，ADR-0103 决策三）。在 MCP
// 读循环 goroutine 中同步调用：实现方不得阻塞（耗时处理须自行异步）。
type NotificationSink func(serverID string, meta ServerMeta, method string, params json.RawMessage)

// SetNotificationSink 注册服务端通知出口（对之后连接的服务器生效；启动装配时先于 StartFromDB 调用）。
func (m *MCPManager) SetNotificationSink(sink NotificationSink) { m.notificationSink.Store(&sink) }

func (m *MCPManager) attachNotificationSink(serverID string, client *MCPClient) {
	client.SetNotificationHandler(func(method string, params json.RawMessage) {
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
