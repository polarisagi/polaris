// Package tool 实现 M7 ToolRegistry（protocol.ToolRegistry 接口）。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §3
//
// 执行路径: ExecuteTool → PolicyGate 五阶段校验 → Sandbox 分级 → ToolResult
// Rate Limiter: builtin 100 QPS / MCP 10 QPS / shell 2 QPS（对应 state.yaml §m7_tool）
package tool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// InMemoryToolRegistry 实现 protocol.ToolRegistry。
// 特性:
//   - 并发安全的工具注册/查找/列举
//   - PolicyGate 前置校验（每次 ExecuteTool 前执行）
//   - 分源 Rate Limiter（builtin/mcp/shell 独立限速）
//   - Taint 传播：ExecuteTool 结果继承输入 TaintLevel（max 传播规则）
type InMemoryToolRegistry struct {
	mu       sync.RWMutex
	tools    map[string]protocol.Tool
	policy   protocol.PolicyGate
	limiters map[string]*rateLimiter // source → limiter
	sandbox  SandboxExecutor         // 真实执行路径（如果为 nil 则用 stub）
}

// SandboxExecutor 是工具注册表最小执行器接口（速率限前需要工具元数据）。
type SandboxExecutor interface {
	Execute(ctx context.Context, name string, input []byte, taintLevel protocol.TaintLevel) ([]byte, error)
}

var _ protocol.ToolRegistry = (*InMemoryToolRegistry)(nil)

// NewInMemoryToolRegistry 创建工具注册表。
// policy 为 nil 时 deny-by-default 生效（不允许任何执行）。
func NewInMemoryToolRegistry(policy protocol.PolicyGate) *InMemoryToolRegistry {
	return &InMemoryToolRegistry{
		tools:  make(map[string]protocol.Tool),
		policy: policy,
		limiters: map[string]*rateLimiter{
			string(protocol.ToolBuiltin): newRateLimiter(100), // 100 QPS
			string(protocol.ToolMCP):     newRateLimiter(10),  // 10 QPS
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
func (r *InMemoryToolRegistry) Register(tool protocol.Tool) error {
	if tool.Name == "" {
		return perrors.New(perrors.CodeInternal, "tool_registry: tool name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name] = tool
	return nil
}

// Lookup 按名称查找工具。未找到返回 ErrToolNotFound。
func (r *InMemoryToolRegistry) Lookup(name string) (protocol.Tool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return protocol.Tool{}, perrors.Wrap(perrors.CodeNotFound, fmt.Sprintf("tool_registry: tool %q not found", name), ErrToolNotFound)
	}
	return t, nil
}

// List 返回所有已注册工具的快照。
func (r *InMemoryToolRegistry) List() []protocol.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]protocol.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

// ExecuteTool 执行工具：PolicyGate 五阶段校验 → RateLimit → Sandbox → ToolResult。
func (r *InMemoryToolRegistry) ExecuteTool(ctx context.Context, name string, input []byte, taintLevel protocol.TaintLevel) (*protocol.ToolResult, error) {
	tool, err := r.Lookup(name)
	if err != nil {
		return nil, err
	}

	// Phase 0: PolicyGate 校验（Capability Token 出口 + Cedar Forbid/Permit）
	if r.policy == nil {
		return &protocol.ToolResult{
			Success: false,
			Error:   "tool_registry: policy gate not initialized, refusing tool call (fail-closed)",
		}, perrors.New(perrors.CodeInternal, "tool_registry: policy gate not initialized")
	}
	allowed, pErr := r.policy.IsAuthorized(ctx, "agent", "tool_execute", name,
		map[string]any{
			"tool_source":            string(tool.Source),
			"risk_level":             int(tool.RiskLevel),
			"trust_level":            toolTrustLevel(tool.Source),
			"capability_token_valid": tool.Capability <= protocol.CapReadOnly,
		})
	if pErr != nil || !allowed {
		reason := "policy denied"
		if pErr != nil {
			reason = pErr.Error()
		}
		return &protocol.ToolResult{
			Success:    false,
			Error:      fmt.Sprintf("tool_registry: policy blocked: %s", reason),
			TaintLevel: taintLevel,
		}, nil
	}

	// Rate Limiter：按工具来源分组
	limiterKey := string(tool.Source)
	if isShellTool(tool) {
		limiterKey = "shell"
	}
	if lim, ok := r.limiters[limiterKey]; ok {
		if !lim.Allow() {
			return &protocol.ToolResult{
				Success:    false,
				Error:      fmt.Sprintf("tool_registry: rate limit exceeded for source %s", limiterKey),
				TaintLevel: taintLevel,
			}, nil
		}
	}

	r.mu.RLock()
	sb := r.sandbox
	r.mu.RUnlock()

	start := time.Now()

	if sb == nil {
		// 无注册 Sandbox 时返回原始输入（单元测试居安模式）
		return &protocol.ToolResult{
			Success:    true,
			Output:     input,
			LatencyMs:  time.Since(start).Milliseconds(),
			TaintLevel: taintLevel,
		}, nil
	}

	// 真实 Sandbox 执行路径
	out, execErr := sb.Execute(ctx, name, input, taintLevel)
	if execErr != nil {
		return &protocol.ToolResult{ //nolint:nilerr
			Success:    false,
			Error:      execErr.Error(),
			LatencyMs:  time.Since(start).Milliseconds(),
			TaintLevel: taintLevel,
		}, nil
	}
	return &protocol.ToolResult{
		Success:    true,
		Output:     out,
		LatencyMs:  time.Since(start).Milliseconds(),
		TaintLevel: taintLevel,
	}, nil
}

// isShellTool 判断工具是否包含 shell/进程副作用（限速 2 QPS）。
func isShellTool(t protocol.Tool) bool {
	for _, se := range t.SideEffects {
		if se == protocol.SideProcessSpawn {
			return true
		}
	}
	return false
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
func toolTrustLevel(source protocol.ToolSource) int {
	switch source {
	case protocol.ToolBuiltin:
		return 4
	case protocol.ToolMCP, protocol.ToolA2A:
		return 2
	}
	return 1
}

// ErrToolNotFound 工具未注册时返回的哨兵错误。
var ErrToolNotFound = perrors.New(perrors.CodeInternal, "tool not found")
