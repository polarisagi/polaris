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
type GatewayImpl struct {
	store protocol.Store

	// waiters 保存等待审批结果的 channel
	mu      sync.Mutex
	waiters map[string]chan protocol.HITLResponse
}

var _ protocol.HITL = (*GatewayImpl)(nil)

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
		return nil, ctx.Err()
	case resp := <-ch:
		return &resp, nil
	}
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
