package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"html"
	"log/slog"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// channelMetaKey Claude：meta 键只允许字母、数字、下划线，其余键静默丢弃。
var channelMetaKey = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// FormatChannelEvent 按 Claude 约定渲染 channel 事件：<channel source="server" k="v">content</channel>。
// 属性值做 XML 转义，防止事件内容伪造标签边界；instructions 作为同源上下文前置。
func FormatChannelEvent(server, instructions, content string, meta map[string]string) string {
	keys := make([]string, 0, len(meta))
	for k := range meta {
		if channelMetaKey.MatchString(k) && k != "source" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var sb strings.Builder
	if strings.TrimSpace(instructions) != "" {
		sb.WriteString("<channel-instructions source=\"" + html.EscapeString(server) + "\">\n" + instructions + "\n</channel-instructions>\n")
	}
	sb.WriteString("<channel source=\"" + html.EscapeString(server) + "\"")
	for _, k := range keys {
		sb.WriteString(" " + k + "=\"" + html.EscapeString(meta[k]) + "\"")
	}
	sb.WriteString(">" + strings.ReplaceAll(content, "</channel>", "&lt;/channel&gt;") + "</channel>")
	return sb.String()
}

// requestIDAlphabet Claude：五个小写字母，去掉 l（手机上不与 1/I 混淆）。
const requestIDAlphabet = "abcdefghijkmnopqrstuvwxyz"

// defaultRelayTTL 审批请求未带截止时间时的转发有效期（与 HITL 审批窗口同量级）。
const defaultRelayTTL = 10 * time.Minute

type pendingRelay struct {
	checkpointID string
	serverID     string
	expires      time.Time
}

// permissionRelays 已发出的审批转发：只接受宿主签发、且由同一服务器回传的 request_id（Claude 规则）。
type permissionRelays struct {
	mu      sync.Mutex
	pending map[string]pendingRelay
}

func newPermissionRelays() *permissionRelays {
	return &permissionRelays{pending: map[string]pendingRelay{}}
}

func (r *permissionRelays) issue(checkpointID, serverID string, expires time.Time) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for id, p := range r.pending {
		if now.After(p.expires) {
			delete(r.pending, id)
		}
	}
	for {
		id, err := randomRequestID()
		if err != nil {
			return "", err
		}
		if _, taken := r.pending[id]; !taken {
			r.pending[id] = pendingRelay{checkpointID: checkpointID, serverID: serverID, expires: expires}
			return id, nil
		}
	}
}

// take 取出并作废该 checkpoint 的全部转发（一个裁决即终结，其他通道的同一请求随之失效）。
func (r *permissionRelays) take(requestID, serverID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pending[requestID]
	if !ok || p.serverID != serverID || time.Now().After(p.expires) {
		return "", false
	}
	for id, other := range r.pending {
		if other.checkpointID == p.checkpointID {
			delete(r.pending, id)
		}
	}
	return p.checkpointID, true
}

// randomRequestID 系统熵源失效时不转发（可预测的 ID 可被远程猜中并冒充裁决）。
func randomRequestID() (string, error) {
	b := make([]byte, 5)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(requestIDAlphabet))))
		if err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "channel: crypto/rand unavailable", err)
		}
		b[i] = requestIDAlphabet[n.Int64()]
	}
	return string(b), nil
}

// RelayPrompt 把审批请求转发到开启了审批转发的 channel（实现 hitl.PromptRelay）。特权级请求
// 不外发：远程一句 "yes" 不应足以批准特权操作，仍须在宿主审批界面完成。
func (s *ChannelService) RelayPrompt(ctx context.Context, p types.HITLPrompt) {
	if p.RiskLevel >= int(types.RiskPrivileged) {
		return
	}
	bindings, err := s.ListBindings(ctx)
	if err != nil {
		slog.Warn("channel: relay list bindings failed", "err", err)
		return
	}
	expires := time.Now().Add(defaultRelayTTL)
	if p.DeadlineNs > 0 {
		expires = time.Unix(0, p.DeadlineNs)
	}
	for _, b := range bindings {
		if !b.relays() {
			continue
		}
		id, err := s.relays.issue(p.ID, b.ServerID, expires)
		if err != nil {
			slog.Error("channel: permission relay skipped", "server", b.ServerID, "err", err)
			return
		}
		params := map[string]string{"request_id": id, "tool_name": p.CheckpointType, "description": p.PromptText, "input_preview": ""}
		if err := s.mcp.NotifyServer(ctx, b.ServerID, methodPermissionRequest, params); err != nil {
			slog.Warn("channel: permission relay failed", "server", b.ServerID, "err", err)
		}
	}
}

// applyVerdict 远程裁决：behavior=allow 批准，其余一律拒绝；未知/过期/非本服务器签发的 ID 丢弃。
func (s *ChannelService) applyVerdict(ctx context.Context, b ChannelBinding, params json.RawMessage) {
	var v struct {
		RequestID string `json:"request_id"`
		Behavior  string `json:"behavior"`
	}
	if err := json.Unmarshal(params, &v); err != nil || !b.relays() {
		return
	}
	checkpointID, ok := s.relays.take(strings.ToLower(v.RequestID), b.ServerID)
	approvals := s.approvals.Load()
	if !ok || approvals == nil {
		slog.Info("channel: permission verdict ignored", "server", b.ServerID, "request_id", v.RequestID)
		return
	}
	resp := types.HITLResponse{Approved: v.Behavior == "allow", UserID: channelUserID(b), Reason: "verdict via channel " + b.Server}
	if err := (*approvals).Respond(ctx, checkpointID, resp); err != nil {
		slog.Warn("channel: apply verdict failed", "checkpoint", checkpointID, "err", err)
	}
}
