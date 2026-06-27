// Package tool 实现 M7 ToolRegistry（protocol.ToolRegistry 接口）。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §3
//
// 执行路径: ExecuteTool → PolicyGate 五阶段校验 → Sandbox 分级 → ToolResult
// Rate Limiter: builtin 100 QPS / MCP 10 QPS / shell 2 QPS（对应 state.yaml §m7_tool）
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// InMemoryToolRegistry 实现 protocol.ToolRegistry。
// 特性:
//   - 并发安全的工具注册/查找/列举
//   - PolicyGate 前置校验（每次 ExecuteTool 前执行）
//   - 分源 Rate Limiter（builtin/mcp/shell 独立限速）
//   - Taint 传播：ExecuteTool 结果继承输入 TaintLevel（max 传播规则）
type InMemoryToolRegistry struct {
	mu               sync.RWMutex
	tools            map[string]types.Tool
	envelope         *sandbox.ExecEnvelope
	limiters         map[string]*rateLimiter
	blackboard       SideEffectChecker
	idempotencyCache map[string]*types.ToolResult
}

// SideEffectChecker 定义 TOCTOU 校验接口（consumer-side 定义，防包循环）。
type SideEffectChecker interface {
	SideEffectPreCheck(ctx context.Context, taskID, agentID string, claimedVersion int32) error
}

// WithSideEffectChecker 注入 Blackboard TOCTOU 校验器（可选）。
func (r *InMemoryToolRegistry) WithSideEffectChecker(c SideEffectChecker) *InMemoryToolRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blackboard = c
	return r
}

// SandboxExecutor 是工具注册表最小执行器接口（速率限前需要工具元数据）。
type SandboxExecutor interface {
	Execute(ctx context.Context, name string, input []byte, taintLevel types.TaintLevel) ([]byte, error)
}

var _ protocol.ToolRegistry = (*InMemoryToolRegistry)(nil)

// NewInMemoryToolRegistry 创建工具注册表。
func NewInMemoryToolRegistry(envelope *sandbox.ExecEnvelope) *InMemoryToolRegistry {
	return &InMemoryToolRegistry{
		tools:            make(map[string]types.Tool),
		envelope:         envelope,
		blackboard:       nil,
		idempotencyCache: make(map[string]*types.ToolResult),
		limiters: map[string]*rateLimiter{
			string(types.ToolBuiltin): newRateLimiter(100), // 100 QPS
			string(types.ToolMCP):     newRateLimiter(10),  // 10 QPS
			// shell 工具通过 SideEffects 包含 process_spawn 识别，限制 2 QPS
			"shell": newRateLimiter(2),
		},
	}
}

// Register 注册工具；同名覆盖（热更新 MCP schema 时使用）。
func (r *InMemoryToolRegistry) Register(tool types.Tool) error {
	if tool.Name == "" {
		return apperr.New(apperr.CodeInternal, "tool_registry: tool name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name] = tool
	return nil
}

// Unregister 从注册表移除指定工具（MCP Server 断开连接时调用）。
// 工具不存在时静默忽略。
func (r *InMemoryToolRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// Lookup 按名称查找工具。未找到返回 ErrToolNotFound。
func (r *InMemoryToolRegistry) Lookup(name string) (types.Tool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return types.Tool{}, apperr.Wrap(apperr.CodeNotFound, fmt.Sprintf("tool_registry: tool %q not found", name), ErrToolNotFound)
	}
	return t, nil
}

// List 返回所有已注册工具的快照。
func (r *InMemoryToolRegistry) List() []types.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]types.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

func (r *InMemoryToolRegistry) ExecuteTool(ctx context.Context, name string, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	cached, ok, idempotencyKey := r.checkIdempotency(ctx)
	if ok {
		return cached, nil
	}

	tool, err := r.Lookup(name)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "InMemoryToolRegistry.ExecuteTool", err)
	}

	// 预执行校验 (RateLimit, DryRun)
	modifiedInput, res, err := r.checkPreExecution(ctx, tool, taintLevel, input)
	if res != nil || err != nil {
		if err != nil {
			return res, apperr.Wrap(apperr.CodeInternal, "InMemoryToolRegistry.ExecuteTool", err)
		}
		return res, nil
	}
	if modifiedInput != nil {
		input = modifiedInput
	}

	if r.envelope == nil {
		return nil, apperr.New(apperr.CodeInternal, "tool_registry: envelope is nil")
	}

	// 统一由 Envelope 接管（包含权限验证、污点传播、日志记录）
	execRes, execErr := r.envelope.Execute(ctx, sandbox.ExecRequest{
		Principal:  sandbox.PrincipalAgent,
		Kind:       sandbox.KindToolExecute,
		Resource:   name,
		TrustTier:  tool.TrustTier,
		Tool:       tool,
		Input:      input,
		TaintLevel: taintLevel, // Envelope 将在执行后计算新的 TaintLevel
		CPUQuotaMs: int(tool.Timeout.Milliseconds()),
	})

	// PostCheck: 防止 TOCTOU 导致已取消任务的副作用不被感知（重用 SideEffectPreCheck 接口）
	r.mu.RLock()
	checker := r.blackboard
	r.mu.RUnlock()
	if checker != nil {
		type taskCtxKey struct{}
		type agentCtxKey struct{}
		type versionCtxKey struct{}
		taskID, _ := ctx.Value(taskCtxKey{}).(string)
		agentID, _ := ctx.Value(agentCtxKey{}).(string)
		claimedVersion, _ := ctx.Value(versionCtxKey{}).(int32)
		if taskID != "" {
			if postErr := checker.SideEffectPreCheck(ctx, taskID, agentID, claimedVersion); postErr != nil {
				slog.Warn("tool_registry: post-check failed (TOCTOU race detected after execution)", "task", taskID, "err", postErr)
			}
		}
	}

	if execErr != nil {
		return &types.ToolResult{ //nolint:nilerr
			Success:    false,
			Error:      execErr.Error(),
			TaintLevel: taintLevel,
		}, nil
	}

	finalResult := &types.ToolResult{
		Success:    execRes.Success,
		Output:     execRes.Output,
		Error:      execRes.Error,
		LatencyMs:  execRes.LatencyMs,
		TaintLevel: execRes.TaintLevel,
		ImageParts: execRes.ImageParts,
	}

	r.cacheIdempotencyResult(idempotencyKey, finalResult)

	return finalResult, nil
}

func (r *InMemoryToolRegistry) checkIdempotency(ctx context.Context) (*types.ToolResult, bool, string) {
	if key, ok := ctx.Value(protocol.CtxIdempotencyKey{}).(types.IdempotencyKey); ok && key != "" {
		idempotencyKey := string(key)
		r.mu.RLock()
		defer r.mu.RUnlock()
		if cachedResult, exists := r.idempotencyCache[idempotencyKey]; exists {
			slog.Debug("tool_registry: returning cached result for idempotency key", "key", idempotencyKey)
			return cachedResult, true, idempotencyKey
		}
		return nil, false, idempotencyKey
	}
	return nil, false, ""
}

func (r *InMemoryToolRegistry) cacheIdempotencyResult(key string, result *types.ToolResult) {
	if key != "" && result.Success {
		r.mu.Lock()
		r.idempotencyCache[key] = result
		r.mu.Unlock()
	}
}

func (r *InMemoryToolRegistry) checkPreExecution(ctx context.Context, tool types.Tool, taintLevel types.TaintLevel, input []byte) ([]byte, *types.ToolResult, error) {
	limiterKey := string(tool.Source)
	if isShellTool(tool) {
		limiterKey = "shell"
	}
	if lim, ok := r.limiters[limiterKey]; ok {
		if !lim.Allow() {
			return input, &types.ToolResult{
				Success:    false,
				Error:      fmt.Sprintf("tool_registry: rate limit exceeded for source %s", limiterKey),
				TaintLevel: taintLevel,
			}, nil
		}
	}

	type taskCtxKey struct{}
	taskID, _ := ctx.Value(taskCtxKey{}).(string)

	if dryRun, ok := ctx.Value(protocol.CtxDryRun{}).(bool); ok && dryRun {
		// DryRun 模式下，对于具备 FS 写入副作用的工具，将其工作目录重定向到 COW 后缀目录，允许其真实执行
		// 通过简单的 JSON 字符串替换实现（假设参数包含路径）
		if taskID != "" && isFileWriteTool(tool) {
			cowTaskID := taskID + ".cow"
			// 简单的字符串替换（注意: 强依赖路径结构中包含 /taskID/）
			modifiedInput := bytes.ReplaceAll(input, []byte("/"+taskID+"/"), []byte("/"+cowTaskID+"/"))
			slog.Debug("tool_registry: dry_run COW enabled, rewriting workspace path", "tool", tool.Name, "original_task", taskID)
			return modifiedInput, nil, nil
		}

		// 其他工具：拦截所有，返回模拟结果；不执行真实副作用
		sideEffectSummary := "none"
		if !isReversible(tool) {
			sideEffectSummary = fmt.Sprintf("would execute %q with side effects: %v", tool.Name, tool.SideEffects)
		}
		out, _ := json.Marshal(map[string]any{
			"status":              "dry_run_simulated",
			"reversible":          isReversible(tool),
			"side_effect_preview": sideEffectSummary,
		})
		return input, &types.ToolResult{
			Success:    true,
			Output:     out,
			TaintLevel: taintLevel,
		}, nil
	}

	// TOCTOU 校验：防止任务被 Reaper 回收后孤儿副作用继续执行（M8 §3.4）
	r.mu.RLock()
	checker := r.blackboard
	r.mu.RUnlock()
	if checker != nil {
		type taskCtxKey struct{}
		type agentCtxKey struct{}
		type versionCtxKey struct{}
		taskID, _ := ctx.Value(taskCtxKey{}).(string)
		agentID, _ := ctx.Value(agentCtxKey{}).(string)
		claimedVersion, _ := ctx.Value(versionCtxKey{}).(int32)
		if taskID != "" {
			if err := checker.SideEffectPreCheck(ctx, taskID, agentID, claimedVersion); err != nil {
				return input, &types.ToolResult{
					Success:    false,
					Error:      fmt.Sprintf("tool_registry: side-effect pre-check failed: %s", err.Error()),
					TaintLevel: taintLevel,
				}, nil
			}
		}
	}

	return input, nil, nil
}

// isFileWriteTool 判断是否是文件写操作工具
func isFileWriteTool(t types.Tool) bool {
	if t.Name == "write_file" || t.Name == "str_replace_editor" || t.Name == "multi_edit_file" || t.Name == "notebook_edit" {
		return true
	}
	for _, se := range t.SideEffects {
		if se == types.SideFileWrite {
			return true
		}
	}
	return false
}

// isShellTool 判断工具是否包含 shell/进程副作用（限速 2 QPS）。
func isShellTool(t types.Tool) bool {
	for _, se := range t.SideEffects {
		if se == types.SideProcessSpawn {
			return true
		}
	}
	return false
}

// isReversible 判断工具副作用是否可逆。
func isReversible(t types.Tool) bool {
	if t.Capability >= types.CapWriteNetwork {
		return false
	}
	for _, se := range t.SideEffects {
		if se == types.SideProcessSpawn || se == types.SideStateMutate {
			return false
		}
	}
	return true
}

// ─── 简单令牌桶限速器 ───────────────────────────────────────────────────────

type rateLimiter struct {
	tokens   atomic.Int64
	maxQPS   int64
	refillAt atomic.Int64 // unix nano
}

func newRateLimiter(qps int64) *rateLimiter {
	rl := &rateLimiter{maxQPS: qps}
	rl.tokens.Store(qps)
	rl.refillAt.Store(time.Now().Add(time.Second).UnixNano())
	return rl
}

func (rl *rateLimiter) Allow() bool {
	now := time.Now().UnixNano()
	if now >= rl.refillAt.Load() {
		// CAS 保证只有一个 goroutine 执行刷新，避免多次 Store 竞态。
		old := rl.refillAt.Load()
		if rl.refillAt.CompareAndSwap(old, now+int64(time.Second)) {
			rl.tokens.Store(rl.maxQPS)
		}
	}
	// Add(-1) 是原子操作；负值代表本窗口超限，不还回——避免多 goroutine 同时还回导致误放行。
	return rl.tokens.Add(-1) >= 0
}

// ErrToolNotFound 工具未注册时返回的哨兵错误。
var ErrToolNotFound = apperr.New(apperr.CodeInternal, "tool not found")
