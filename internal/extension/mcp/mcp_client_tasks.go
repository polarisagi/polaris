package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// tasks 扩展（io.modelcontextprotocol/tasks，SEP-2663）：服务器可以把一次
// methodToolsCall 请求转成异步任务（resultType:"task"），客户端通过 tasks/get 轮询、
// tasks/update 应答任务内的 inputRequests、tasks/cancel 提前终止。规范只对 methodToolsCall
// 定义了该扩展的语义（tasks §Supported Requests），resources/read、prompts/get 等其余
// MRTR 支持的请求类型不在本实现范围内。
const (
	methodTasksGet    = "tasks/get"
	methodTasksUpdate = "tasks/update"
	methodTasksCancel = "tasks/cancel"

	// extensionTasks clientCapabilities.extensions 里声明的扩展键名，只在新纪元请求
	// 的 _meta 中出现（见 withMeta）：旧纪元 initialize 的 capabilities 语义不同（旧纪元
	// core tasks 与本扩展不是同一套协议），本任务不实现，故不在 initializeLegacy 里声明。
	extensionTasks = "io.modelcontextprotocol/tasks"

	taskStatusWorking       = "working"
	taskStatusInputRequired = "input_required"
	taskStatusCompleted     = "completed"
	taskStatusFailed        = "failed"
	taskStatusCancelled     = "cancelled"

	// defaultPollIntervalMs / minPollInterval / maxPollInterval 轮询间隔缺省值与夹紧区间
	// （tasks §Task Polling 未规定上下限，全靠客户端自律；防止服务器把间隔设得过密打爆
	// 自己，或设得过疏导致任务观感上"卡死"）。
	defaultPollIntervalMs = int64(1000)
	minPollInterval       = 200 * time.Millisecond
	maxPollInterval       = 30 * time.Second

	// taskCancelTimeout ctx 取消时尽力发送 tasks/cancel 使用的独立超时（不能复用已取消的
	// ctx，否则请求根本发不出去）。
	taskCancelTimeout = 5 * time.Second
)

// mcpTaskState CreateTaskResult 与 tasks/get 响应共用的信封。DetailedTask 按 status
// 内联 inputRequests / result / error 三个互斥字段（tasks §Tasks、§Task Polling）。
type mcpTaskState struct {
	TaskID         string                      `json:"taskId"`
	Status         string                      `json:"status"`
	StatusMessage  string                      `json:"statusMessage,omitempty"`
	CreatedAt      string                      `json:"createdAt"`
	LastUpdatedAt  string                      `json:"lastUpdatedAt"`
	TTLMs          *int64                      `json:"ttlMs"`
	PollIntervalMs *int64                      `json:"pollIntervalMs,omitempty"`
	InputRequests  map[string]mrtrInputRequest `json:"inputRequests,omitempty"`
	Result         json.RawMessage             `json:"result,omitempty"`
	Error          *RPCError                   `json:"error,omitempty"`
}

// awaitTask 在新纪元 methodToolsCall 结果为 resultType:"task" 时接管：轮询 tasks/get
// 直到任务进入终态，期间对 input_required 状态里尚未答复过的 inputRequests 调用统一
// 输入处理器并发起 tasks/update。返回值即 completed 状态下的 result 字段，原样交还给
// request 的调用方——CallTool / CallToolTainted 不需要感知任务语义即可解析。
//
// 规范（tasks §Task Polling）建议客户端持久化 taskId 以便进程崩溃后恢复轮询。Polaris
// 的 MCP 工具调用是同步 goroutine——本函数的调用方正阻塞在这里等待返回值，进程重启后
// 调用方连同这个 goroutine 一起消失，恢复轮询也没有接收方，与内存态异步任务缓存
// （M13-bis §8.4）同一理由，因此本实现不落盘 taskId，只保留在内存里。
func (c *MCPClient) awaitTask(ctx context.Context, createResult json.RawMessage) (json.RawMessage, error) {
	var seed mcpTaskState
	if err := json.Unmarshal(createResult, &seed); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp: parse CreateTaskResult", err)
	}
	if seed.TaskID == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "mcp: CreateTaskResult missing taskId")
	}
	createdAt, err := time.Parse(time.RFC3339, seed.CreatedAt)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mcp: parse task createdAt", err)
	}
	slog.Info("mcp: task created", "server", c.cfg.ServerName, "taskId", seed.TaskID, "status", seed.Status)

	interval := clampPollInterval(seed.PollIntervalMs)
	var ttl *int64 // 每轮 tasks/get 后立即刷新为服务器最新值，见循环体
	answered := make(map[string]struct{})
	status := seed.Status

	for {
		select {
		case <-ctx.Done():
			c.cancelTaskBestEffort(seed.TaskID)
			return nil, apperr.Wrap(apperr.CodeCancelled, "mcp: task "+seed.TaskID+" wait cancelled", ctx.Err())
		case <-time.After(interval):
		}

		state, err := c.getTask(ctx, seed.TaskID)
		if err != nil {
			return nil, err
		}
		interval, ttl = clampPollInterval(state.PollIntervalMs), state.TTLMs
		if state.Status != status {
			status = state.Status
			slog.Info("mcp: task status changed", "server", c.cfg.ServerName, "taskId", seed.TaskID, "status", status)
		}

		result, done, err := c.handleTaskStatus(ctx, seed.TaskID, state, answered)
		if done {
			return result, err
		}
		if ttl != nil && time.Now().After(createdAt.Add(time.Duration(*ttl)*time.Millisecond)) {
			return nil, apperr.New(apperr.CodeTimeout,
				fmt.Sprintf("mcp: task %s exceeded ttlMs without reaching a terminal status", seed.TaskID))
		}
	}
}

// handleTaskStatus 处理一次 tasks/get 响应。终态（completed/failed/cancelled/未知状态）
// 返回 (result, true, err)；working / input_required 返回 (nil, false, nil) 继续轮询——
// input_required 会先尝试应答尚未答复过的 inputRequests。
func (c *MCPClient) handleTaskStatus(ctx context.Context, taskID string, state *mcpTaskState,
	answered map[string]struct{}) (json.RawMessage, bool, error) {
	switch state.Status {
	case taskStatusWorking:
		return nil, false, nil
	case taskStatusInputRequired:
		if err := c.answerTaskInput(ctx, taskID, state.InputRequests, answered); err != nil {
			return nil, true, err
		}
		return nil, false, nil
	case taskStatusCompleted:
		slog.Info("mcp: task completed", "server", c.cfg.ServerName, "taskId", taskID)
		return state.Result, true, nil
	case taskStatusFailed:
		slog.Info("mcp: task failed", "server", c.cfg.ServerName, "taskId", taskID)
		if state.Error == nil {
			return nil, true, apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: task %s failed without error detail", taskID))
		}
		return nil, true, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: task %s failed", taskID), state.Error)
	case taskStatusCancelled:
		slog.Info("mcp: task cancelled", "server", c.cfg.ServerName, "taskId", taskID)
		return nil, true, apperr.New(apperr.CodeCancelled, fmt.Sprintf("mcp: task %s was cancelled", taskID))
	default:
		return nil, true, apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: task %s has unknown status %q", taskID, state.Status))
	}
}

// getTask 发起一次 tasks/get 请求并解析为 mcpTaskState。不经 request()/MRTR 处理——
// tasks/get 不在 MRTR §Supported Requests 之列，规范未定义它自己也返回 input_required
// 的情形。
func (c *MCPClient) getTask(ctx context.Context, taskID string) (*mcpTaskState, error) {
	result, err := c.call(ctx, methodTasksGet, map[string]any{"taskId": taskID})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp: tasks/get "+taskID, err)
	}
	var state mcpTaskState
	if err := json.Unmarshal(result, &state); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp: parse tasks/get result", err)
	}
	return &state, nil
}

// answerTaskInput 对 input_required 状态里尚未答复过的 key 调用 fulfillInputRequests
// （与 MRTR 共用同一统一输入处理器），随后以 tasks/update 提交 inputResponses；已经
// 答复过的 key 直接跳过——规范 SHOULD 要求客户端对跨轮询重复出现的 inputRequests
// 去重，避免同一请求被重复呈现给用户或模型（tasks §Task Update Requests）。
func (c *MCPClient) answerTaskInput(ctx context.Context, taskID string, reqs map[string]mrtrInputRequest,
	answered map[string]struct{}) error {
	pending := make(map[string]mrtrInputRequest, len(reqs))
	for key, req := range reqs {
		if _, done := answered[key]; !done {
			pending[key] = req
		}
	}
	if len(pending) == 0 {
		return nil
	}
	responses, err := c.fulfillInputRequests(ctx, pending)
	if err != nil {
		return err
	}
	if _, err := c.call(ctx, methodTasksUpdate, map[string]any{
		"taskId":         taskID,
		"inputResponses": responses,
	}); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp: tasks/update "+taskID, err)
	}
	for key := range pending {
		answered[key] = struct{}{}
	}
	return nil
}

// cancelTaskBestEffort ctx 取消时尽力通知服务器释放任务：用独立的短超时 context 而非
// 已经取消的 ctx（否则请求根本发不出去），失败只记日志，不影响返回给调用方的取消错误
// （规范未要求 tasks/cancel 必达，仅是尽力而为的资源释放提示）。
func (c *MCPClient) cancelTaskBestEffort(taskID string) {
	cancelCtx, cancel := context.WithTimeout(context.Background(), taskCancelTimeout)
	defer cancel()
	if _, err := c.call(cancelCtx, methodTasksCancel, map[string]any{"taskId": taskID}); err != nil {
		slog.Warn("mcp: best-effort tasks/cancel failed", "server", c.cfg.ServerName, "taskId", taskID, "err", err)
	}
}

// clampPollInterval 计算下一次轮询前的等待时长：取最近一次响应的 pollIntervalMs，
// 缺省 defaultPollIntervalMs，并夹到 [minPollInterval, maxPollInterval] 区间。
func clampPollInterval(ms *int64) time.Duration {
	v := defaultPollIntervalMs
	if ms != nil {
		v = *ms
	}
	d := time.Duration(v) * time.Millisecond
	switch {
	case d < minPollInterval:
		return minPollInterval
	case d > maxPollInterval:
		return maxPollInterval
	default:
		return d
	}
}
