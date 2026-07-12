package mcp

import (
	"context"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"

	"github.com/google/uuid"
)

// AsyncTaskStatus 异步 MCP 任务生命周期状态（M13-bis §8.4）。
type AsyncTaskStatus string

const (
	AsyncTaskPending AsyncTaskStatus = "pending"
	AsyncTaskDone    AsyncTaskStatus = "done"
	AsyncTaskFailed  AsyncTaskStatus = "failed"
)

// asyncTaskTTL tasks_cache 条目存活时间，M13-bis §8.4 SSoT：TTL=300s。
const asyncTaskTTL = 300 * time.Second

// AsyncTaskResult 异步任务当前状态快照；Status=pending 时 Text/Images/Error 均为零值。
//
// TaintLevel 由 CallToolTainted（TaintPreservingDecoder 对响应 JSON 逐叶打标后取最高值）
// 计算得出，Status=done 时有效；此前曾被 runAsyncCall 用 `_` 丢弃，导致异步 MCP 工具结果
// 永远无法参与 agent_execute_dag.go 的 GlobalTaintLevel 抬升（GR-4-002 同款漏洞的异步变体）。
type AsyncTaskResult struct {
	TaskID     string
	Status     AsyncTaskStatus
	Text       string
	Images     []types.ImagePart
	Error      string
	TaintLevel types.TaintLevel
	ExpiresAt  time.Time
}

// asyncTaskCache tasks_cache 的具体实现：内存 map（task_id → result），TTL=300s，
// M13-bis §8.4 明确要求"内存 map"而非持久化表——异步任务本就是单进程生命周期内的
// 临时中间态，重启后不需要也不应该恢复（重启后 goroutine 已不存在，恢复一个不会再
// 完成的 pending 状态没有意义）。
type asyncTaskCache struct {
	mu    sync.RWMutex
	tasks map[string]*AsyncTaskResult
}

func newAsyncTaskCache() *asyncTaskCache {
	return &asyncTaskCache{tasks: make(map[string]*AsyncTaskResult)}
}

func (c *asyncTaskCache) put(r *AsyncTaskResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tasks[r.TaskID] = r
}

// get 查询任务；已过期的条目视为不存在（惰性淘汰），同时顺带物理删除。
func (c *asyncTaskCache) get(taskID string) (*AsyncTaskResult, bool) {
	c.mu.RLock()
	r, ok := c.tasks[taskID]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(r.ExpiresAt) {
		c.mu.Lock()
		delete(c.tasks, taskID)
		c.mu.Unlock()
		return nil, false
	}
	return r, true
}

// sweep 清理所有已过期任务，防止长期运行下 pending→done 后无人轮询的任务导致
// map 无界增长（惰性淘汰只在被 get() 命中时触发，孤儿任务永远不会被 get 命中）。
func (c *asyncTaskCache) sweep() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, r := range c.tasks {
		if now.After(r.ExpiresAt) {
			delete(c.tasks, id)
		}
	}
}

// asyncTaskSweepInterval 后台清理周期，远小于 TTL 以保证及时释放内存。
const asyncTaskSweepInterval = 30 * time.Second

// startAsyncTaskSweeper 启动后台清理 goroutine，随进程生命周期运行。
// 由 NewMCPManager 调用一次；ctx 使用 context.Background() 是有意的——tasks_cache
// 的清理不应绑定任何单次请求的生命周期，与 MCPManager 实例本身同寿命。
func (m *MCPManager) startAsyncTaskSweeper() {
	concurrent.SafeGo(context.Background(), "mcp.async_task_sweeper", func(ctx context.Context) {
		ticker := time.NewTicker(asyncTaskSweepInterval)
		defer ticker.Stop()
		for range ticker.C {
			m.asyncTasks.sweep()
		}
	})
}

// CallToolAsync 立即返回 task_id（status=pending），后台 goroutine 执行实际 MCP
// 调用，完成后将结果写入 tasks_cache（TTL=300s）。LLM 通过 GetAsyncTaskResult
// （对应 get_task_result 工具）轮询结果。
//
// 适用场景（M13-bis §8.4）：预估执行时间 > 5s 的耗时 MCP 工具，避免同步阻塞
// Agent 主执行链路（S_EXECUTE 单节点长时间挂起会连带阻塞 DAG 中其余可并行节点）。
//
// serverID 为 Add()/entries map 的键（DB 实例 ID），与 LLM 侧工具名前缀使用的
// serverName 是两个不同的标识符，此处按 serverID 精确查找，语义与既有 CallTool
// 一致。工具注册路径（registerTools）已直接持有 *MCPClient，走 runAsyncCall
// 跳过本次查找，避免重复加锁。
func (m *MCPManager) CallToolAsync(ctx context.Context, serverID, toolName string, args map[string]any) (string, error) {
	m.mu.RLock()
	e, ok := m.entries[serverID]
	m.mu.RUnlock()
	if !ok {
		return "", apperr.New(apperr.CodeInternal, "mcp_manager: server not found: "+serverID)
	}
	return m.runAsyncCall(ctx, e.client, toolName, args), nil
}

// runAsyncCall 是 CallToolAsync 与工具注册路径（makeMCPToolAsyncFn）共享的核心
// 实现：立即分配 task_id 并写入 pending 状态，随后在后台 goroutine 中执行真正
// 的 MCP 调用，完成/失败后回写 tasks_cache。
func (m *MCPManager) runAsyncCall(ctx context.Context, client *MCPClient, mcpName string, args map[string]any) string {
	taskID := "mcptask_" + uuid.NewString()
	m.asyncTasks.put(&AsyncTaskResult{
		TaskID:    taskID,
		Status:    AsyncTaskPending,
		ExpiresAt: time.Now().Add(asyncTaskTTL),
	})

	// 后台执行需要脱离调用方 ctx 的取消/超时（调用方在拿到 task_id 后可能立即返回，
	// 结束当前请求的 ctx），但仍需继承 TraceID 等身份信息，用 protocol.Detach。
	bgCtx := protocol.Detach(ctx)
	concurrent.SafeGo(bgCtx, "mcp.async_task_run", func(ctx context.Context) {
		text, imgs, taintLevel, err := client.CallToolTainted(ctx, mcpName, args)
		if err != nil {
			m.asyncTasks.put(&AsyncTaskResult{
				TaskID:    taskID,
				Status:    AsyncTaskFailed,
				Error:     err.Error(),
				ExpiresAt: time.Now().Add(asyncTaskTTL),
			})
			return
		}
		m.asyncTasks.put(&AsyncTaskResult{
			TaskID:     taskID,
			Status:     AsyncTaskDone,
			Text:       text,
			Images:     imgs,
			TaintLevel: taintLevel,
			ExpiresAt:  time.Now().Add(asyncTaskTTL),
		})
	})

	return taskID
}

// GetAsyncTaskResult 查询异步任务当前状态与（若已完成）结果，供 get_task_result
// 工具轮询使用。任务不存在或已过期（TTL=300s）时返回 ok=false。
func (m *MCPManager) GetAsyncTaskResult(taskID string) (*AsyncTaskResult, bool) {
	return m.asyncTasks.get(taskID)
}
