package hitl

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
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
	waiters map[string]chan protocol.HITLResponse
}

var _ protocol.HITL = (*GatewayImpl)(nil)

func (g *GatewayImpl) SetNotifier(n Notifier) {
	g.notifier = n
}

func NewGateway(store protocol.Store) *GatewayImpl {
	return &GatewayImpl{
		store:   store,
		waiters: make(map[string]chan protocol.HITLResponse),
	}
}

// Prompt 挂起当前任务并请求人工审批。
func (g *GatewayImpl) Prompt(ctx context.Context, p protocol.HITLPrompt) (*protocol.HITLResponse, error) {
	// 1. 持久化 pending 状态
	key := []byte("hitl:pending:" + p.ID)
	data, err := json.Marshal(p)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "hitl_gateway: marshal failed", err)
	}
	if err := g.store.Put(ctx, key, data); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "hitl_gateway: put failed", err)
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
	ch := make(chan protocol.HITLResponse, 1)
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
		action := resolveTimeoutAction(p)
		switch action {
		case "auto_approve":
			resp := protocol.HITLResponse{Approved: true, Reason: "auto_approved_on_timeout"}
			_ = g.Respond(context.Background(), p.ID, resp)
			return &resp, nil
		case "auto_deny":
			resp := protocol.HITLResponse{Approved: false, Reason: "auto_denied_on_timeout"}
			_ = g.Respond(context.Background(), p.ID, resp)
			return &resp, nil
		default: // "kill_pause" 或未配置
			return nil, ctx.Err()
		}
	case resp := <-ch:
		return &resp, nil
	}
}

func resolveTimeoutAction(p protocol.HITLPrompt) string {
	if p.CheckpointType == "low_risk" {
		return "auto_approve"
	}
	if p.CheckpointType == "high_risk" || p.RiskLevel >= 3 {
		return "auto_deny"
	}
	return "kill_pause"
}

// Respond 提交人工审批决策。
func (g *GatewayImpl) Respond(ctx context.Context, checkpointID string, response protocol.HITLResponse) error {
	// 1. 清理 pending
	key := []byte("hitl:pending:" + checkpointID)
	// （可选：可以验证记录是否存在）
	if err := g.store.Delete(ctx, key); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "hitl_gateway: delete pending failed", err)
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
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("hitl_gateway: no active waiter for %s (possibly timed out)", checkpointID))
	}
	return nil
}

// Pending 返回当前所有待审批请求。
func (g *GatewayImpl) Pending(ctx context.Context) ([]protocol.HITLPrompt, error) {
	iter, err := g.store.Scan(ctx, []byte("hitl:pending:"))
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var prompts []protocol.HITLPrompt
	for iter.Next() {
		var p protocol.HITLPrompt
		if err := json.Unmarshal(iter.Value(), &p); err == nil {
			prompts = append(prompts, p)
		}
	}
	return prompts, nil
}

// ChannelNotifier 适配器实现（通过 Dispatch 发送）
type ChannelNotifier struct {
	// 实际应用中可能需要注入具体的 channel 实例或 client
}

func NewChannelNotifier() *ChannelNotifier {
	return &ChannelNotifier{}
}

func (c *ChannelNotifier) Notify(ctx context.Context, msg HITLNotification) error {
	// 在此处集成到现有的 pkg/gateway/channels 逻辑或 slog 告警
	// log.Printf("ChannelNotifier: HITL triggered for Checkpoint %s", msg.CheckpointID)
	return nil
}
