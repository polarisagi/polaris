package hitl

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// GatewayImpl 实现了 protocol.HITL，管理人机交互网关 [ESCALATE]。
// 架构文档: docs/arch/M13-Interface-Scheduler.md §2.4
type HITLNotification struct {
	CheckpointID string
	TaskID       string
	Description  string
	Risk         string
	Timeout      int64
	ReviewURL    string
}

type Notifier interface {
	Notify(ctx context.Context, msg HITLNotification) error
}

type GatewayImpl struct {
	store    protocol.Store
	notifier Notifier

	// waiters 保存等待审批结果的 channel
	mu      sync.Mutex
	waiters map[string]chan types.HITLResponse

	allowedAutoApproveTypes []string
	currentPolicyEtag       string // Cedar policy 当前 etag，由外部热更新时注入
}

var _ protocol.HITL = (*GatewayImpl)(nil)

func (g *GatewayImpl) SetNotifier(n Notifier) {
	g.notifier = n
}

func NewGateway(store protocol.Store) *GatewayImpl {
	return &GatewayImpl{
		store:                   store,
		waiters:                 make(map[string]chan types.HITLResponse),
		allowedAutoApproveTypes: []string{},
	}
}

func (g *GatewayImpl) SetAllowedAutoApproveTypes(types []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allowedAutoApproveTypes = types
}

// Prompt 挂起当前任务并请求人工审批。
func (g *GatewayImpl) Prompt(ctx context.Context, p types.HITLPrompt) (*types.HITLResponse, error) {
	// 若调用方未设置截止时间但 p.DeadlineNs > 0，用 DeadlineNs 建立截止上下文，
	// 防止因上层 context 无超时而无限阻塞。
	if p.DeadlineNs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Now().Add(time.Duration(p.DeadlineNs)))
		defer cancel()
	}

	// 1. 持久化 pending 状态
	key := []byte("hitl:pending:" + p.ID)
	data, err := json.Marshal(p)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "hitl_gateway: marshal failed", err)
	}
	if err := g.store.Put(ctx, key, data); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "hitl_gateway: put failed", err)
	}
	if g.notifier != nil {
		go func() {
			_ = g.notifier.Notify(context.Background(), HITLNotification{
				CheckpointID: p.ID,
				TaskID:       "",
				Description:  p.PromptText,
				Risk:         p.CheckpointType,
				Timeout:      p.DeadlineNs,
				ReviewURL:    "/v1/hitl/review?id=" + p.ID,
			})
		}()
	}

	// 2. 注册 waiter
	ch := make(chan types.HITLResponse, 1)
	g.mu.Lock()
	g.waiters[p.ID] = ch
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.waiters, p.ID)
		g.mu.Unlock()
	}()

	// 3. 阻塞等待或超时 (上下文控制)
	select {
	case <-ctx.Done():
		// 超时/取消
		action := g.resolveTimeoutAction(p)
		switch action {
		case "auto_approve":
			resp := types.HITLResponse{Approved: true, Reason: "auto_approved_on_timeout"}
			_ = g.Respond(context.Background(), p.ID, resp)
			return &resp, nil
		case "auto_deny":
			resp := types.HITLResponse{Approved: false, Reason: "auto_denied_on_timeout"}
			_ = g.Respond(context.Background(), p.ID, resp)
			return &resp, nil
		default: // "kill_pause" 或未配置
			return nil, ctx.Err()
		}
	case resp := <-ch:
		return &resp, nil
	}
}

func (g *GatewayImpl) resolveTimeoutAction(p types.HITLPrompt) string {
	// TaintLevel >= TaintMedium(2) 时超时一律拒绝，禁止 auto_approve（M13 §2.4）
	if p.TaintLevel >= 2 {
		return "auto_deny"
	}

	if p.RiskLevel == 0 {
		g.mu.Lock()
		allowed := false
		for _, t := range g.allowedAutoApproveTypes {
			if p.CheckpointType == t {
				allowed = true
				break
			}
		}
		g.mu.Unlock()
		if allowed {
			// auto_approve 前校验 Cedar policy etag 原子性（M13 §2.4）
			if !g.validateDecisionEtag(p.DecisionEtag) {
				// etag 不一致：策略已热更新，升级为 HITL 审批（不自动放行）
				return "kill_pause"
			}
			return "auto_approve"
		}
	}
	if p.CheckpointType == "high_risk" || p.RiskLevel >= 3 {
		return "auto_deny"
	}
	return "kill_pause"
}

// validateDecisionEtag 原子校验当前 Cedar policy etag 与决策时刻记录的 etag 是否一致。
// etag 为空（旧路径兼容）时放行；不一致时拒绝 auto_approve。
func (g *GatewayImpl) validateDecisionEtag(decisionEtag string) bool {
	if decisionEtag == "" {
		return true // 旧路径兼容：无 etag 字段，跳过校验
	}
	g.mu.Lock()
	currentEtag := g.currentPolicyEtag
	g.mu.Unlock()
	return currentEtag == "" || currentEtag == decisionEtag
}

// SetPolicyEtag 更新当前 Cedar policy etag（热更新时由 PolicyWatcher 调用）。
func (g *GatewayImpl) SetPolicyEtag(etag string) {
	g.mu.Lock()
	g.currentPolicyEtag = etag
	g.mu.Unlock()
}

// Respond 提交人工审批决策。
func (g *GatewayImpl) Respond(ctx context.Context, checkpointID string, response types.HITLResponse) error {
	// 1. 清理 pending
	key := []byte("hitl:pending:" + checkpointID)
	// （可选：可以验证记录是否存在）
	if err := g.store.Delete(ctx, key); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "hitl_gateway: delete pending failed", err)
	}

	// 2. 持久化归档记录 (audit)
	archiveKey := []byte(fmt.Sprintf("hitl:archive:%s:%d", checkpointID, time.Now().UnixNano()))
	archiveData, _ := json.Marshal(response)
	_ = g.store.Put(ctx, archiveKey, archiveData)

	// 3. 通知等待中的任务
	g.mu.Lock()
	ch, ok := g.waiters[checkpointID]
	if ok {
		ch <- response
		delete(g.waiters, checkpointID)
	}
	g.mu.Unlock()

	if !ok {
		// 任务可能已经因为超时被取消（或跨节点等原因，当前只做本地分发）
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("hitl_gateway: no active waiter for %s (possibly timed out)", checkpointID))
	}
	return nil
}

// Pending 返回当前所有待审批请求。
func (g *GatewayImpl) Pending(ctx context.Context) ([]types.HITLPrompt, error) {
	iter, err := g.store.Scan(ctx, []byte("hitl:pending:"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "GatewayImpl.Pending", err)
	}
	defer iter.Close()

	var prompts []types.HITLPrompt
	for iter.Next() {
		var p types.HITLPrompt
		if err := json.Unmarshal(iter.Value(), &p); err == nil {
			prompts = append(prompts, p)
		}
	}
	return prompts, nil
}

// ChannelDispatcher 真实通知发送接口（Slack/Email/Desktop 实现此接口）
type ChannelDispatcher interface {
	Dispatch(ctx context.Context, msg HITLNotification) error
}

// ChannelNotifier 适配器实现
type ChannelNotifier struct {
	dispatcher ChannelDispatcher // nil 表示未配置
}

func NewChannelNotifier() *ChannelNotifier {
	return &ChannelNotifier{}
}

// SetDispatcher 注入具体通知 dispatcher（Slack/Email/Desktop）
func (c *ChannelNotifier) SetDispatcher(d ChannelDispatcher) {
	c.dispatcher = d
}

func (c *ChannelNotifier) Notify(ctx context.Context, msg HITLNotification) error {
	slog.Warn("hitl: approval required",
		"checkpoint_id", msg.CheckpointID,
		"task_id", msg.TaskID,
		"description", msg.Description,
		"risk", msg.Risk,
		"review_url", msg.ReviewURL,
		"timeout_ns", msg.Timeout,
	)
	if c.dispatcher == nil {
		slog.Info("hitl: you can approve this request locally using the following command:",
			"cmd", fmt.Sprintf("curl -s -X POST http://localhost:28889/v1/approvals/%s/resolve -H 'Content-Type: application/json' -d '{\"action\": \"approve\", \"comment\": \"CLI Auto-approved\"}'", msg.CheckpointID))
		// 未配置 dispatcher：仅输出日志提示，不返回错误以保持 prompt 挂起等待外部调用
		return nil
	}
	return c.dispatcher.Dispatch(ctx, msg)
}

// BroadcastTainted 广播污染事件。
func (g *GatewayImpl) BroadcastTainted(ctx context.Context, event string, taintLevel types.TaintLevel) error {
	if taintLevel == types.TaintHigh {
		// High 级别由 HITL 主流程处理，不在此广播
		slog.Warn("hitl: high_taint event suppressed in BroadcastTainted (handled by main HITL flow)",
			"event", event, "taint_level", taintLevel)
		return nil
	}

	slog.Warn("hitl: taint event broadcast",
		"event", event, "taint_level", taintLevel)

	if g.notifier == nil {
		slog.Warn("hitl: notifier not configured, taint broadcast logged only",
			"event", event, "taint_level", taintLevel)
		return nil
	}

	return g.notifier.Notify(ctx, HITLNotification{
		CheckpointID: fmt.Sprintf("taint_%s_%d", event, time.Now().UnixNano()),
		TaskID:       event,
		Description:  fmt.Sprintf("Taint level %d broadcast: %s", int(taintLevel), event),
		Risk:         fmt.Sprintf("taint_level_%d", int(taintLevel)),
		Timeout:      int64(30 * time.Second),
	})
}
