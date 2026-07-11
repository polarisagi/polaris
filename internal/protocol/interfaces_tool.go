package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

type

// ToolRegistry 是工具发现、注册、执行的统一入口。
// 工具来源: Built-in(~20) | MCP(inf) | types.Skill(inf) | A2A(inf) | LLM-generated(临时, [Sandbox-L3])。
// 执行路径: ExecuteTool → Policy Gate(五阶段) → Sandbox → ToolResult。
ToolRegistry interface {
	Register(tool types.Tool) error
	Lookup(name string) (types.Tool, error)
	List() []types.Tool
	ExecuteTool(ctx context.Context, name string, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error)
}

type

// SandboxProvider 是分级沙箱抽象（Sbx-L1/L2/L3）。
SandboxProvider interface {
	Level() int // 1=InProc, 2=Wasmtime, 3=gVisor/microVM
	Run(ctx context.Context, spec types.SandboxSpec) (*types.SandboxResult, error)
}

type

// ToolExecutor — 工具执行器，含 DryRun 保护。
ToolExecutor interface {
	Execute(ctx context.Context, call types.ToolCallRequest) (*types.ToolResult, error)
	ExecuteDryRun(ctx context.Context, call types.ToolCallRequest) (*types.ToolResult, error)
	Cancel(ctx context.Context, callID string) error
	// RecordAudit 写入工具调用的全链路审计记录。
	RecordAudit(ctx context.Context, toolName string, payload []byte) error
}

type

// AgentToolExecutor 是 Agent Kernel 依赖的工具执行入口（消费方定义，符合 R1.4）。
// 由 dispatch.Dispatcher 实现，确保 Agent 与 HTTP 网关走同一条拦截器链
// （SchemaValidateInterceptor + AuditInterceptor），不再存在"网关有审计、
// Agent 自主调用无审计"的双路径分裂问题。
AgentToolExecutor interface {
	ExecuteWithTaint(ctx context.Context, name string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error)
	Lookup(name string) (types.Tool, error)
}
