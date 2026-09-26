package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// Claude channel 协议（code.claude.com/docs/en/channels-reference；ADR-0103 决策三）。
const (
	ChannelCapability           = "claude/channel"
	ChannelPermissionCapability = "claude/channel/permission"
	methodChannelEvent          = "notifications/claude/channel"
	methodPermissionRequest     = "notifications/claude/channel/permission_request"
	methodPermissionVerdict     = "notifications/claude/channel/permission"
)

// ChannelBinding 插件 channel 的对外视图：定义来自 manifest 快照，启用状态来自 plugin_channels，
// 能力来自已连接服务器的 initialize 声明。
type ChannelBinding struct {
	PluginID           string `json:"plugin_id"`
	PluginName         string `json:"plugin_name"`
	Server             string `json:"server"`
	ServerID           string `json:"server_id"`
	DisplayName        string `json:"display_name,omitempty"`
	Enabled            bool   `json:"enabled"`
	PermissionRelay    bool   `json:"permission_relay"`
	Connected          bool   `json:"connected"`
	DeclaresChannel    bool   `json:"declares_channel"`
	DeclaresPermission bool   `json:"declares_permission"`
}

// ChannelMCP MCP 管理器中 channel 所需的能力（实现为 *mcp.MCPManager）。
type ChannelMCP interface {
	ServerMeta(serverID string) (mcp.ServerMeta, bool)
	NotifyServer(ctx context.Context, serverID, method string, params any) error
}

// ChannelTurnRunner 把一条 channel 事件作为一轮 headless 会话执行（实现在装配层，基于
// session.Orchestator）。channel 回复由模型调用服务器自己的工具完成，宿主不自动回发。
type ChannelTurnRunner interface {
	RunChannelTurn(ctx context.Context, sessionID, input string) error
}

// ApprovalResponder 审批裁决入口（实现为 hitl.GatewayImpl）。
type ApprovalResponder interface {
	Respond(ctx context.Context, checkpointID string, response types.HITLResponse) error
}

// ChannelService Claude 插件 channels 的运行时：入站事件 → 会话轮次；审批请求 → 远程裁决。
type ChannelService struct {
	extRepo   protocol.ExtensionRepository
	mcp       ChannelMCP
	turns     atomic.Pointer[ChannelTurnRunner]
	approvals atomic.Pointer[ApprovalResponder]

	mu       sync.Mutex
	sessions map[string]*sync.Mutex // 同一 channel 的事件串行进入同一会话
	relays   *permissionRelays
}

func NewChannelService(extRepo protocol.ExtensionRepository, m ChannelMCP) *ChannelService {
	return &ChannelService{extRepo: extRepo, mcp: m, sessions: map[string]*sync.Mutex{}, relays: newPermissionRelays()}
}

// BindTurns / BindApprovals 会话编排与审批网关在扩展层之后构造，装配完成时绑定。
func (s *ChannelService) BindTurns(r ChannelTurnRunner)     { s.turns.Store(&r) }
func (s *ChannelService) BindApprovals(a ApprovalResponder) { s.approvals.Store(&a) }
func channelServerID(pluginID, server string) string        { return "plugin_" + pluginID + "_" + server }
func channelSessionID(pluginID, server string) string       { return "ch_plugin_" + pluginID + "_" + server }
func channelUserID(b ChannelBinding) string                 { return "channel:" + b.PluginName + "/" + b.Server }
func (b ChannelBinding) active() bool                       { return b.Enabled && b.Connected && b.DeclaresChannel }
func (b ChannelBinding) relays() bool                       { return b.active() && b.PermissionRelay && b.DeclaresPermission }

// ListBindings 全部已安装插件声明的 channel。
func (s *ChannelService) ListBindings(ctx context.Context) ([]ChannelBinding, error) {
	plugins, err := s.extRepo.ListPlugins(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "ChannelService.plugins", err)
	}
	states, err := s.extRepo.ListPluginChannelStates(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "ChannelService.states", err)
	}
	stateOf := map[string]types.PluginChannelState{}
	for _, st := range states {
		stateOf[st.PluginID+"\x00"+st.Server] = st
	}
	var out []ChannelBinding
	for _, row := range plugins {
		var spec pluginspec.Plugin
		if err := json.Unmarshal([]byte(row.Manifest), &spec); err != nil {
			slog.Warn("channel: corrupt plugin manifest, channels skipped", "plugin", row.ID, "err", err)
			continue
		}
		for _, ch := range spec.Channels {
			st := stateOf[row.ID+"\x00"+ch.Server]
			b := ChannelBinding{PluginID: row.ID, PluginName: row.Name, Server: ch.Server, ServerID: channelServerID(row.ID, ch.Server),
				DisplayName: ch.DisplayName, Enabled: st.Enabled && row.Enabled, PermissionRelay: st.PermissionRelay}
			if meta, ok := s.mcp.ServerMeta(b.ServerID); ok {
				b.Connected = true
				b.DeclaresChannel = meta.DeclaresExperimental(ChannelCapability)
				b.DeclaresPermission = meta.DeclaresExperimental(ChannelPermissionCapability)
			}
			out = append(out, b)
		}
	}
	return out, nil
}

// SetBindingState 用户显式启用/停用 channel 与审批转发（安装 ≠ 启用）。
func (s *ChannelService) SetBindingState(ctx context.Context, st types.PluginChannelState) error {
	bindings, err := s.ListBindings(ctx)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if b.PluginID == st.PluginID && b.Server == st.Server {
			if err := s.extRepo.SavePluginChannelState(ctx, st); err != nil {
				return apperr.Wrap(apperr.CodeOf(err), "ChannelService.SetBindingState", err)
			}
			return nil
		}
	}
	return apperr.New(apperr.CodeNotFound, fmt.Sprintf("plugin %s declares no channel %q", st.PluginID, st.Server))
}

func (s *ChannelService) bindingFor(ctx context.Context, serverID string) (ChannelBinding, bool) {
	bindings, err := s.ListBindings(ctx)
	if err != nil {
		slog.Warn("channel: list bindings failed", "err", err)
		return ChannelBinding{}, false
	}
	for _, b := range bindings {
		if b.ServerID == serverID {
			return b, true
		}
	}
	return ChannelBinding{}, false
}

// HandleNotification 实现 mcp.NotificationSink（在 MCP 读循环中调用，处理一律异步）。
func (s *ChannelService) HandleNotification(serverID string, meta mcp.ServerMeta, method string, params json.RawMessage) {
	if method != methodChannelEvent && method != methodPermissionVerdict {
		return
	}
	concurrent.SafeGo(context.Background(), "extension.channel.notification", func(ctx context.Context) {
		b, ok := s.bindingFor(ctx, serverID)
		// 未声明为插件 channel、未启用或服务器未声明 claude/channel 能力：丢弃（不能让任意 MCP
		// 服务器把消息推入会话）。
		if !ok || !b.active() {
			slog.Info("channel: notification dropped (channel not enabled)", "server", serverID, "method", method)
			return
		}
		if method == methodChannelEvent {
			s.deliverEvent(ctx, b, meta, params)
			return
		}
		s.applyVerdict(ctx, b, params)
	})
}

// deliverEvent 事件以 <channel source="..." k="v">content</channel> 进入该 channel 的专属会话；
// 服务器 instructions 随事件下发（Claude 在连接时作为上下文交给模型）。
func (s *ChannelService) deliverEvent(ctx context.Context, b ChannelBinding, meta mcp.ServerMeta, params json.RawMessage) {
	var ev struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(params, &ev); err != nil || strings.TrimSpace(ev.Content) == "" {
		slog.Warn("channel: malformed event dropped", "server", b.ServerID, "err", err)
		return
	}
	runner := s.turns.Load()
	if runner == nil {
		slog.Warn("channel: session runner not bound, event dropped", "server", b.ServerID)
		return
	}
	sessionID := channelSessionID(b.PluginID, b.Server)
	lock := s.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	if err := (*runner).RunChannelTurn(ctx, sessionID, FormatChannelEvent(b.Server, meta.Instructions, ev.Content, ev.Meta)); err != nil {
		slog.Warn("channel: turn failed", "server", b.ServerID, "err", err)
	}
}

func (s *ChannelService) sessionLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.sessions[id]
	if !ok {
		l = &sync.Mutex{}
		s.sessions[id] = l
	}
	return l
}
