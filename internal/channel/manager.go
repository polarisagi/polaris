package channel

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/internal/protocol"
)

// Manager 持有所有聊天平台 poller 的生命周期，与 HTTP 层解耦。
type Manager struct {
	mu         sync.Mutex
	pollers    map[string]context.CancelFunc
	httpClient *http.Client
	safeDialer protocol.SafeDialer // IMAP/SMTP 等 raw-TCP 通道的 SSRF 防护拨号器
	onMessage  atomic.Pointer[cadapter.MessageHandler]
}

// NewManager 创建 Manager，httpClient 用于各平台 HTTP 调用，onMessage 是消息分发回调。
func NewManager(httpClient *http.Client, onMessage cadapter.MessageHandler, opts ...func(*Manager)) *Manager {
	m := &Manager{
		pollers:    make(map[string]context.CancelFunc),
		httpClient: httpClient,
	}
	m.onMessage.Store(&onMessage)
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// WithSafeDialer 注入 SafeDialer，用于需要 raw TCP 的 channel（email IMAP 等）。
// 未注入时 email poller 拒绝启动。
func WithSafeDialer(sd protocol.SafeDialer) func(*Manager) {
	return func(m *Manager) { m.safeDialer = sd }
}

// registerPoller 注册 cancel 函数，同名旧 poller 先停止。
func (m *Manager) registerPoller(channelID string, cancel context.CancelFunc) {
	m.mu.Lock()
	if old, ok := m.pollers[channelID]; ok {
		old()
	}
	m.pollers[channelID] = cancel
	m.mu.Unlock()
}

// Stop 停止指定 channel 的 poller。
func (m *Manager) Stop(channelID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, ok := m.pollers[channelID]; ok {
		cancel()
		delete(m.pollers, channelID)
	}
}

// StopAll 停止所有 poller（Server.Shutdown 调用）。
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cancel := range m.pollers {
		cancel()
		delete(m.pollers, id)
	}
}

// Start 按平台类型分发 poller 启动。
func (m *Manager) Start(channelID, channelType string, cfg map[string]any) { //nolint:gocyclo
	if a, ok := cadapter.GetAdapter(channelType); ok {
		a.StartPoller(m, channelID, cfg)
		return
	}

}

// RestoreChannelsFromDB 从数据库加载所有的频道配置（如 Discord/Telegram Token 等）并拉起 polling 协程。
func (m *Manager) RestoreChannelsFromDB(db protocol.SQLQuerier) {
	m.StopAll()
	if db == nil {
		return
	}
	rows, err := db.QueryContext(context.Background(),
		`SELECT id,type,config_json FROM channels WHERE enabled=1`)
	if err != nil {
		slog.Error("channels: load from db", "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, chType, cfgJSON string
		if err := rows.Scan(&id, &chType, &cfgJSON); err != nil {
			continue
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
			slog.Warn("channel manager: restore config unmarshal failed, skip channel", "id", id, "type", chType, "err", err)
			continue
		}
		m.Start(id, chType, cfg)
		slog.Info("channels: poller started", "id", id, "type", chType)
	}
}

func (m *Manager) HTTPClient() *http.Client { return m.httpClient }

// SetMessageHandler 晚绑定入站消息处理器（poller 回调最终落点）。
// boot 阶段 channelMgr 先于 ChannelsAdmin 构造，故用 setter 而非构造参数注入。
func (m *Manager) SetMessageHandler(h cadapter.MessageHandler) {
	m.onMessage.Store(&h)
}

// OnMessage 处理 poller 轮询到的消息。
func (m *Manager) OnMessage(channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage) {
	if h := m.onMessage.Load(); h != nil && *h != nil {
		(*h)(channelType, channelID, cfg, msg)
	}
}

// RegisterPoller ...
func (m *Manager) RegisterPoller(channelID string, cancel context.CancelFunc) {
	m.registerPoller(channelID, cancel)
}
func (m *Manager) SafeDialer() protocol.SafeDialer { return m.safeDialer }

// ExtractMessage delegates to the standalone ExtractMessage function.
func (m *Manager) ExtractMessage(channelType string, body []byte, r *http.Request) protocol.ChannelMessage {
	return ExtractMessage(channelType, body, r)
}
