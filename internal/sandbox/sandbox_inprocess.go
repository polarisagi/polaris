package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── Tier 1: InProcessSandbox ────────────────────────────────────────────────

// InProcessSandbox 在调用方 goroutine 内直接执行内置工具函数。
// 适用于: types.ToolBuiltin + types.CapReadOnly
// 安全约束: 无文件写、无网络——由 PolicyGate 在调用前验证，此处不再重复校验。
type InProcessSandbox struct {
	mu       sync.RWMutex
	registry map[string]InProcessFn
	// richRegistry 存储可返回 ToolResult（含 ImageParts）的富工具函数（MCP 等外部工具）。
	// Run() 优先查此表，未命中才走 registry，两表互斥（RegisterRich 不写 registry）。
	richRegistry map[string]InProcessRichFn
	// taintMap 存储每个工具的输出污点等级。
	// 内置工具保持 TaintNone（零值），MCP/外部工具通过 RegisterWithTaint/RegisterRich 写入。
	taintMap map[string]types.TaintLevel
}

// InProcessFn 内置工具执行函数签名（仅返回字节）。
type InProcessFn func(ctx context.Context, input []byte) ([]byte, error)

// InProcessRichFn 富工具执行函数签名，返回完整 ToolResult（含 ImageParts）。
// 适用于 MCP 工具等可能返回图片/多媒体内容的外部工具。
// 调用方（InProcessSandbox.Run）会将 ToolResult.TaintLevel 设为注册时指定的 taint（若未设置）。
type InProcessRichFn func(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error)

func NewInProcessSandbox() *InProcessSandbox {
	return &InProcessSandbox{
		registry:     make(map[string]InProcessFn),
		richRegistry: make(map[string]InProcessRichFn),
		taintMap:     make(map[string]types.TaintLevel),
	}
}

// Level 返回沙箱级别（实现 protocol.SandboxProvider）。
func (s *InProcessSandbox) Level() int { return 1 }

// Register 注册工具函数（并发安全，支持运行时动态注册 MCP 工具）。
// 内置工具使用此方法，输出污点为 TaintNone。
func (s *InProcessSandbox) Register(toolName string, fn InProcessFn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry[toolName] = fn
}

// RegisterWithTaint 注册工具函数并指定输出污点等级。
// MCP/外部工具调用此方法：白名单 → TaintMedium，其余 → TaintHigh。
func (s *InProcessSandbox) RegisterWithTaint(toolName string, fn InProcessFn, taint types.TaintLevel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry[toolName] = fn
	s.taintMap[toolName] = taint
}

// RegisterRich 注册富工具函数（返回完整 ToolResult，含 ImageParts）。
// 供 MCP/外部工具使用；taint 在 Run() 中回填（若 ToolResult.TaintLevel==0）。
// 不同于 Register/RegisterWithTaint：不写 registry，两路互斥。
func (s *InProcessSandbox) RegisterRich(toolName string, fn InProcessRichFn, taint types.TaintLevel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.richRegistry[toolName] = fn
	s.taintMap[toolName] = taint
}

// Unregister 取消注册工具（MCP Server 断开时调用，同时清理两个注册表）。
func (s *InProcessSandbox) Unregister(toolName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.registry, toolName)
	delete(s.richRegistry, toolName)
	delete(s.taintMap, toolName)
}

func (s *InProcessSandbox) Run(ctx context.Context, spec SandboxSpec) (result *types.ToolResult, runErr error) {
	start := time.Now()
	tierLabel := trace.SandboxTierLabel(int(spec.SandboxTier))
	defer func() {
		latencyMs := float64(time.Since(start).Milliseconds())
		status := "success"
		if runErr != nil {
			status = "error"
		}
		trace.RecordToolCall(ctx, spec.ToolName, status, tierLabel, latencyMs)
		trace.RecordSandboxExecution(ctx, tierLabel)
	}()

	s.mu.RLock()
	fn, ok := s.registry[spec.ToolName]
	richFn := s.richRegistry[spec.ToolName]
	taint := s.taintMap[spec.ToolName] // TaintNone(0) for builtins
	s.mu.RUnlock()

	quotaMs := spec.CPUQuotaMs
	if quotaMs == 0 {
		quotaMs = 300000 // 默认提升至 5 分钟 (300,000ms)，以应对极端长耗时的插件或宏操作
	}
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(quotaMs)*time.Millisecond)
	defer cancel()

	// 优先走富工具路径（MCP 等可返回 ImageParts 的工具）
	if richFn != nil {
		// InProcessRichFn 工具执行
		res, execErr := richFn(execCtx, spec)
		latency := time.Since(start).Milliseconds()
		if execErr != nil {
			return &types.ToolResult{
				Success:    false,
				Error:      execErr.Error(),
				LatencyMs:  latency,
				TaintLevel: taint,
			}, nil
		}
		if res == nil {
			res = &types.ToolResult{}
		}
		res.LatencyMs = latency
		// 回填注册时的污点等级（富工具函数通常不感知 taint，由注册层统一设置）
		if res.TaintLevel == 0 {
			res.TaintLevel = taint
		}
		return res, nil
	}

	if !ok {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("inprocess_sandbox: unknown tool %q", spec.ToolName))
	}

	out, execErr := fn(execCtx, spec.Input)
	if execErr != nil {
		return &types.ToolResult{
			Success:    false,
			Error:      execErr.Error(),
			LatencyMs:  time.Since(start).Milliseconds(),
			TaintLevel: taint,
		}, nil
	}
	return &types.ToolResult{
		Success:    true,
		Output:     out,
		LatencyMs:  time.Since(start).Milliseconds(),
		TaintLevel: taint,
	}, nil
}

// Execute 满足 tool.SandboxExecutor 接口（简化版，无 SandboxSpec 包装），
// 允许 InProcessSandbox 直接作为 InMemoryToolRegistry 的执行后端。
func (s *InProcessSandbox) Execute(ctx context.Context, toolName string, input []byte, taintLevel types.TaintLevel) ([]byte, error) {
	s.mu.RLock()
	fn, ok := s.registry[toolName]
	s.mu.RUnlock()
	if !ok {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("inprocess_sandbox: unknown tool %q", toolName))
	}
	return fn(ctx, input)
}
