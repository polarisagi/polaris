// Package tool 实现 M7 ToolRegistry（protocol.ToolRegistry 接口）。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §3
//
// 执行路径: ExecuteTool → PolicyGate 五阶段校验 → Sandbox 分级 → ToolResult
// Rate Limiter: builtin 100 QPS / MCP 10 QPS / shell 2 QPS（对应 state.yaml §m7_tool）
package tool

import (
	"github.com/polarisagi/polaris/internal/security/token"

	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/protocol"
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
	mu         sync.RWMutex
	tools      map[string]types.Tool
	policy     protocol.PolicyGate
	limiters   map[string]*rateLimiter // source → limiter
	sandbox    SandboxExecutor         // 真实执行路径（如果为 nil 则用 stub）
	blackboard SideEffectChecker       // 可选：TOCTOU 前置校验（M8 Blackboard）
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
// policy 为 nil 时 deny-by-default 生效（不允许任何执行）。
func NewInMemoryToolRegistry(policy protocol.PolicyGate) *InMemoryToolRegistry {
	return &InMemoryToolRegistry{
		tools:  make(map[string]types.Tool),
		policy: policy,
		limiters: map[string]*rateLimiter{
			string(types.ToolBuiltin): newRateLimiter(100), // 100 QPS
			string(types.ToolMCP):     newRateLimiter(10),  // 10 QPS
			// shell 工具通过 SideEffects 包含 process_spawn 识别，限制 2 QPS
			"shell": newRateLimiter(2),
		},
	}
}

// SetSandbox 设置工具实际执行器（允许运行时替换，例如单元测试注入 mock）。
func (r *InMemoryToolRegistry) SetSandbox(sb SandboxExecutor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandbox = sb
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

// ExecuteTool 执行工具：PolicyGate 五阶段校验 → RateLimit → Sandbox → ToolResult。
func (r *InMemoryToolRegistry) ExecuteTool(ctx context.Context, name string, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	tool, err := r.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("InMemoryToolRegistry.ExecuteTool: %w", err)
	}

	// 预执行校验 (PolicyGate, RateLimit, JIT Token, DryRun)
	if res, err := r.checkPreExecution(ctx, tool, taintLevel); res != nil || err != nil {
		if err != nil {
			return res, fmt.Errorf("InMemoryToolRegistry.ExecuteTool: %w", err)
		}
		return res, nil
	}

	r.mu.RLock()
	sb := r.sandbox
	checker := r.blackboard
	r.mu.RUnlock()

	start := time.Now()

	if sb == nil {
		// 无注册 Sandbox 时返回原始输入（单元测试居安模式）
		return &types.ToolResult{
			Success:    true,
			Output:     input,
			LatencyMs:  time.Since(start).Milliseconds(),
			TaintLevel: taintLevel,
		}, nil
	}

	// 真实 Sandbox 执行路径
	out, execErr := sb.Execute(ctx, name, input, taintLevel)

	// PostCheck: 防止 TOCTOU 导致已取消任务的副作用不被感知（重用 SideEffectPreCheck 接口）
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
			LatencyMs:  time.Since(start).Milliseconds(),
			TaintLevel: taintLevel,
		}, nil
	}
	return &types.ToolResult{
		Success:    true,
		Output:     out,
		LatencyMs:  time.Since(start).Milliseconds(),
		TaintLevel: taintLevel,
	}, nil
}

// checkPreExecution 处理 PolicyGate、RateLimiter、Token 及 DryRun 等预检逻辑
func (r *InMemoryToolRegistry) checkPreExecution(ctx context.Context, tool types.Tool, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	if r.policy == nil {
		return &types.ToolResult{
			Success: false,
			Error:   "tool_registry: policy gate not initialized, refusing tool call (fail-closed)",
		}, apperr.New(apperr.CodeInternal, "tool_registry: policy gate not initialized")
	}

	tokenVal := ctx.Value(protocol.CtxCapabilityToken{})
	tok, _ := tokenVal.(*token.Token)
	tokenValid := validateToken(tok, tool.Name)

	allowed, pErr := r.policy.IsAuthorized(ctx, "agent", "tool_execute", tool.Name,
		map[string]any{
			"tool_source":            string(tool.Source),
			"risk_level":             int(tool.RiskLevel),
			"trust_level":            toolTrustLevel(tool.Source),
			"capability_token_valid": tokenValid,
		})
	if pErr != nil || !allowed {
		reason := "policy denied"
		if pErr != nil {
			reason = pErr.Error()
		}
		return &types.ToolResult{
			Success:    false,
			Error:      fmt.Sprintf("tool_registry: policy blocked: %s", reason),
			TaintLevel: taintLevel,
		}, nil
	}

	limiterKey := string(tool.Source)
	if isShellTool(tool) {
		limiterKey = "shell"
	}
	if lim, ok := r.limiters[limiterKey]; ok {
		if !lim.Allow() {
			return &types.ToolResult{
				Success:    false,
				Error:      fmt.Sprintf("tool_registry: rate limit exceeded for source %s", limiterKey),
				TaintLevel: taintLevel,
			}, nil
		}
	}

	if !tokenValid {
		return nil, apperr.New(apperr.CodeForbidden, "missing/invalid capability token for tool")
	}

	if dryRun, ok := ctx.Value(protocol.CtxDryRun{}).(bool); ok && dryRun {
		// DryRun 模式：拦截所有工具，返回模拟结果；不执行真实副作用
		sideEffectSummary := "none"
		if !isReversible(tool) {
			sideEffectSummary = fmt.Sprintf("would execute %q with side effects: %v", tool.Name, tool.SideEffects)
		}
		out, _ := json.Marshal(map[string]any{
			"status":              "dry_run_simulated",
			"reversible":          isReversible(tool),
			"side_effect_preview": sideEffectSummary,
		})
		return &types.ToolResult{
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
				return &types.ToolResult{
					Success:    false,
					Error:      fmt.Sprintf("tool_registry: side-effect pre-check failed: %s", err.Error()),
					TaintLevel: taintLevel,
				}, nil
			}
		}
	}

	return nil, nil
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

// validateToken 校验 Capability Token 的合法性。
func validateToken(tok *token.Token, toolName string) bool {
	if tok == nil {
		return false
	}
	return action.GetTokenManager().Verify(tok) == nil
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

// toolTrustLevel 根据工具来源推导信任等级。
// ToolBuiltin → 4（系统信任）; ToolMCP/ToolA2A → 2（社区信任）; 其余 → 1
func toolTrustLevel(source types.ToolSource) int {
	switch source {
	case types.ToolBuiltin:
		return 4
	case types.ToolMCP, types.ToolA2A:
		return 2
	}
	return 1
}

// ErrToolNotFound 工具未注册时返回的哨兵错误。
var ErrToolNotFound = apperr.New(apperr.CodeInternal, "tool not found")
