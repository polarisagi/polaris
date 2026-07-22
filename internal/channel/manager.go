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
	wecomSends sync.Map // channelID → chan wecomMsg
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
	if a, ok := cadapter.Lookup(channelType); ok {
		a.StartPoller(m, channelID, cfg)
		return
	}

	switch channelType {
	case "telegram":
		token, _ := cfg["bot_token"].(string)
		if token != "" {
			m.startTelegramPoller(channelID, token, cfg)
		}
	case "discord":
		token, _ := cfg["bot_token"].(string)
		if token != "" {
			m.startDiscordPoller(channelID, token, cfg)
		}
	case "slack":
		botToken, _ := cfg["bot_token"].(string)
		appToken, _ := cfg["app_token"].(string)
		if botToken != "" && appToken != "" {
			m.startSlackPoller(channelID, botToken, appToken, cfg)
		}
	case "feishu":
		appID, _ := cfg["app_id"].(string)
		appSecret, _ := cfg["app_secret"].(string)
		mode, _ := cfg["connection_mode"].(string)
		if appID != "" && appSecret != "" && mode != "webhook" {
			m.startFeishuPoller(channelID, appID, appSecret, cfg)
		}
	case "qqbot":
		appID, _ := cfg["app_id"].(string)
		clientSecret, _ := cfg["client_secret"].(string)
		if appID != "" && clientSecret != "" {
			m.startQQBotPoller(channelID, appID, clientSecret, cfg)
		}
	case "dingtalk":
		clientID, _ := cfg["client_id"].(string)
		clientSecret, _ := cfg["client_secret"].(string)
		if clientID != "" && clientSecret != "" {
			m.startDingTalkPoller(channelID, clientID, clientSecret, cfg)
		}
	case "wecom":
		botID, _ := cfg["bot_id"].(string)
		secret, _ := cfg["secret"].(string)
		if botID != "" && secret != "" {
			m.startWeComPoller(channelID, botID, secret, cfg)
		}
	case "mattermost":
		mmURL, _ := cfg["url"].(string)
		token, _ := cfg["token"].(string)
		if mmURL != "" && token != "" {
			m.startMattermostPoller(channelID, mmURL, token, cfg)
		}
	case "email":
		imapHost, _ := cfg["imap_host"].(string)
		address, _ := cfg["address"].(string)
		password, _ := cfg["password"].(string)
		if imapHost != "" && address != "" && password != "" {
			m.startEmailPoller(channelID, cfg)
		}
	case "matrix":
		homeserver, _ := cfg["homeserver"].(string)
		accessToken, _ := cfg["access_token"].(string)
		if homeserver != "" {
			m.startMatrixPoller(channelID, homeserver, accessToken, cfg)
		}
	case "signal":
		apiURL, _ := cfg["api_url"].(string)
		account, _ := cfg["account"].(string)
		if apiURL != "" && account != "" {
			m.startSignalPoller(channelID, apiURL, account, cfg)
		}
	case "homeassistant":
		haURL, _ := cfg["url"].(string)
		haToken, _ := cfg["token"].(string)
		if haURL != "" && haToken != "" {
			m.startHomeAssistantPoller(channelID, haURL, haToken, cfg)
		}
		// line / whatsapp / sms / teams / webhook：纯 webhook 模式，无需 poller
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

// WecomEnqueue 将 wecom 回复投递到 Manager 持有的发送通道；非 wecom 适配器不调用。
func (m *Manager) WecomEnqueue(channelID string, msg cadapter.WecomSendMsg) bool {
	if v, ok := m.wecomSends.Load(channelID); ok {
		if ch, ok := v.(chan cadapter.WecomSendMsg); ok {
			select {
			case ch <- msg:
				return true
			default:
				slog.Warn("wecom: send channel full", "channel", channelID)
			}
		}
	}
	return false
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
