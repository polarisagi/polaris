package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/observability/metrics"
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

	conflictCheck atomic.Pointer[func(ctx context.Context, pluginID, pluginName, server string) error]

	sessionLocks [64]sync.Mutex // 固定分片避免每个用户会话永久占用一把锁
	relays       *permissionRelays
	dedup        *channelDedup
	state        *channelRuntimeState
}

func NewChannelService(extRepo protocol.ExtensionRepository, m ChannelMCP) *ChannelService {
	return &ChannelService{
		extRepo: extRepo,
		mcp:     m,
		relays:  newPermissionRelays(),
		dedup:   newChannelDedup(maxChannelDedupCapacity),
		state:   newChannelRuntimeState(),
	}
}

// BindTurns / BindApprovals 会话编排与审批网关在扩展层之后构造，装配完成时绑定。
func (s *ChannelService) BindTurns(r ChannelTurnRunner)     { s.turns.Store(&r) }
func (s *ChannelService) BindApprovals(a ApprovalResponder) { s.approvals.Store(&a) }
func (s *ChannelService) SetConflictChecker(fn func(ctx context.Context, pluginID, pluginName, server string) error) {
	s.conflictCheck.Store(&fn)
}

func (s *ChannelService) CancelBinding(pluginID, server string) {
	s.state.cancelBinding(pluginID, server)
}

func channelServerID(pluginID, server string) string  { return "plugin_" + pluginID + "_" + server }
func channelSessionID(pluginID, server string) string { return "ch_plugin_" + pluginID + "_" + server }
func channelUserID(b ChannelBinding) string           { return "channel:" + b.PluginName + "/" + b.Server }
func (b ChannelBinding) active() bool                 { return b.Enabled && b.Connected && b.DeclaresChannel }
func (b ChannelBinding) relays() bool                 { return b.active() && b.PermissionRelay && b.DeclaresPermission }

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

func (s *ChannelService) checkConflict(ctx context.Context, b ChannelBinding) error {
	if fn := s.conflictCheck.Load(); fn != nil && *fn != nil {
		return (*fn)(ctx, b.PluginID, b.PluginName, b.Server)
	}
	return nil
}

// SetBindingState 用户显式启用/停用 channel 与审批转发（安装 ≠ 启用）。
func (s *ChannelService) SetBindingState(ctx context.Context, st types.PluginChannelState) error {
	bindings, err := s.ListBindings(ctx)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if b.PluginID != st.PluginID || b.Server != st.Server {
			continue
		}
		if st.Enabled {
			if err := s.checkConflict(ctx, b); err != nil {
				return err
			}
		}
		if err := s.extRepo.SavePluginChannelState(ctx, st); err != nil {
			return apperr.Wrap(apperr.CodeOf(err), "ChannelService.SetBindingState", err)
		}
		if !st.Enabled {
			s.CancelBinding(st.PluginID, st.Server)
		}
		return nil
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
	if method == methodChannelEvent {
		// 在启用状态检查之前计数，确保随后被拒绝的已知 channel 事件也属于 received。
		metrics.RecordChannelEventReceived(context.Background())
	}
	concurrent.SafeGo(context.Background(), "extension.channel.notification", func(ctx context.Context) {
		b, ok := s.bindingFor(ctx, serverID)
		// 未声明为插件 channel、未启用或服务器未声明 claude/channel 能力：丢弃（不能让任意 MCP
		// 服务器把消息推入会话）。
		if !ok || !b.active() {
			if method == methodChannelEvent {
				metrics.RecordChannelEventRejected(ctx, "binding_inactive")
			}
			slog.Info("channel: notification dropped (channel not enabled)", "server", serverID, "method", method)
			return
		}
		if method == methodChannelEvent {
			gen := s.state.currentGen(b.PluginID, b.Server)
			s.deliverEvent(ctx, b, meta, params, gen)
			return
		}
		s.applyVerdict(ctx, b, params)
	})
}

type channelEventPayload struct {
	content   string
	meta      map[string]string
	route     *channelRoute
	messageID string
}

func parseChannelEventPayload(params json.RawMessage) (*channelEventPayload, string, error) {
	var ev struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(params, &ev); err != nil {
		return nil, "malformed", apperr.Wrap(apperr.CodeInvalidInput, "unmarshal channel event", err)
	}
	if strings.TrimSpace(ev.Content) == "" {
		return nil, "malformed", apperr.New(apperr.CodeInvalidInput, "channel event content must be non-empty")
	}
	route, err := channelRouteFromMeta(ev.Meta)
	if err != nil {
		return nil, "route_invalid", err
	}
	messageID, err := validateChannelMessageID(ev.Meta)
	if err != nil {
		return nil, "route_invalid", err
	}
	return &channelEventPayload{
		content:   ev.Content,
		meta:      ev.Meta,
		route:     route,
		messageID: messageID,
	}, "", nil
}

func (s *ChannelService) isBindingActive(ctx context.Context, b ChannelBinding, gen uint64) bool {
	current, ok := s.bindingFor(ctx, b.ServerID)
	return ok && current.active() && current.PluginID == b.PluginID && current.Server == b.Server && s.state.currentGen(b.PluginID, b.Server) == gen
}

// deliverEvent 事件以 <channel source="..." k="v">content</channel> 进入该 channel 的专属会话；
// 服务器 instructions 随事件下发（Claude 在连接时作为上下文交给模型）。
func (s *ChannelService) deliverEvent(ctx context.Context, b ChannelBinding, meta mcp.ServerMeta, params json.RawMessage, gen uint64) {
	ctx, span := otel.Tracer("polaris/extension/channel").Start(ctx, "channel.deliver_event")
	defer span.End()

	payload, rejectReason, err := parseChannelEventPayload(params)
	if err != nil || payload == nil {
		outcome := "malformed"
		if rejectReason == "route_invalid" {
			outcome = "route_rejected"
		}
		span.SetAttributes(attribute.String("channel.event.outcome", outcome))
		metrics.RecordChannelEventRejected(ctx, rejectReason)
		if err != nil {
			span.RecordError(err)
		}
		slog.Warn("channel: event rejected", "server", b.ServerID, "reason", rejectReason, "err", err)
		return
	}

	runner := s.turns.Load()
	if runner == nil {
		metrics.RecordChannelEventRejected(ctx, "runner_unavailable")
		slog.Warn("channel: session runner not bound, event dropped", "server", b.ServerID)
		return
	}

	sessionID := channelSessionID(b.PluginID, b.Server)
	routeType := "legacy"
	if payload.route != nil {
		sessionID = routedChannelSessionID(b.PluginID, b.Server, *payload.route)
		routeType = "routed"
	}
	span.SetAttributes(attribute.String("channel.event.route", routeType))

	lock := s.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	if !s.isBindingActive(ctx, b, gen) {
		span.SetAttributes(attribute.String("channel.event.outcome", "binding_inactive"))
		metrics.RecordChannelEventRejected(ctx, "binding_inactive")
		span.AddEvent("channel.event_dropped", trace.WithAttributes(attribute.String("reason", "binding_inactive")))
		slog.Info("channel: queued event dropped (binding inactive)", "server", b.ServerID)
		return
	}

	if payload.messageID != "" && !s.dedup.checkOrRecord(b.PluginID, b.Server, payload.messageID) {
		span.SetAttributes(attribute.String("channel.event.outcome", "duplicate"))
		metrics.RecordChannelEventRejected(ctx, "duplicate")
		span.AddEvent("channel.event_dropped", trace.WithAttributes(attribute.String("reason", "duplicate")))
		slog.Info("channel: duplicate event dropped", "server", b.ServerID, "message_id", payload.messageID)
		return
	}

	turnCtx, unregister, ok := s.state.startTurn(ctx, b.PluginID, b.Server, gen)
	if !ok {
		span.SetAttributes(attribute.String("channel.event.outcome", "binding_inactive"))
		metrics.RecordChannelEventRejected(ctx, "binding_inactive")
		span.AddEvent("channel.event_dropped", trace.WithAttributes(attribute.String("reason", "binding_inactive")))
		slog.Info("channel: event dropped before turn (generation changed)", "server", b.ServerID)
		return
	}
	defer unregister()

	eventInput := FormatChannelEvent(b.Server, meta.Instructions, payload.content, payload.meta)
	if err := (*runner).RunChannelTurn(turnCtx, sessionID, eventInput); err != nil {
		metrics.RecordChannelTurn(ctx, "failure")
		span.SetAttributes(attribute.String("channel.event.outcome", "turn_failed"))
		span.RecordError(err)
		slog.Warn("channel: turn failed", "server", b.ServerID, "err", err)
		return
	}
	metrics.RecordChannelTurn(ctx, "success")
	span.SetAttributes(attribute.String("channel.event.outcome", "dispatched"))
}

func (s *ChannelService) sessionLock(id string) *sync.Mutex {
	index := 0
	for _, b := range []byte(id) {
		index = (index*31 + int(b)) % len(s.sessionLocks)
	}
	return &s.sessionLocks[index]
}
