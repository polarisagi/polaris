package dispatch

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// Dispatcher 统一工具调用入口——全系统唯一的工具执行路由。
//
// 路由规则（route 方法）:
//   - Source==ToolSkill: 委托 protocol.SkillExecutor.ExecuteSkill（instructions 渲染 / 脚本执行，
//     内部按需走 PolicyGate + 沙箱分级，见 internal/extension/skill.ScriptSkillExecutor）。
//   - 其余全部来源（builtin/mcp/native/...）: 委托 protocol.ToolRegistry.ExecuteTool。
//     MCP 工具的真实调用函数已在连接时通过 InProcessSandbox.RegisterRich 注册
//     （见 internal/extension/mcp/mcp_manager.go registerTools），ExecuteTool 内部的
//     PolicyGate→沙箱分级→执行 链路会原生找到并调用它——不需要在本包重复实现一条
//     "直连 MCPManager" 的旁路（历史上存在过 mcpMgr 短路分支，已删除：该分支永远拿不到
//     注入实例，属于死代码，且一旦被误接上会绕过 PolicyGate，是潜在的安全回归)。
//
// 拦截器链（Interceptor）只做横切关注点（审计等），不重复实现 PolicyGate/RateLimit/Idempotency——
// 这些已经是 ToolRegistry.ExecuteTool 的职责，重复实现即为本文件试图消除的"两条线"问题本身。
type Dispatcher struct {
	catalog   catalog.Catalog
	toolReg   protocol.ToolRegistry
	skillExec protocol.SkillExecutor
	chain     []Interceptor
}

func New(catalog catalog.Catalog, toolReg protocol.ToolRegistry, skillExec protocol.SkillExecutor) *Dispatcher {
	return &Dispatcher{
		catalog:   catalog,
		toolReg:   toolReg,
		skillExec: skillExec,
	}
}

func (d *Dispatcher) Use(interceptors ...Interceptor) {
	d.chain = append(d.chain, interceptors...)
}

type ExecFn func(ctx context.Context, entry protocol.CatalogEntry, args []byte) (*types.ToolResult, error)
type Interceptor func(ctx context.Context, entry protocol.CatalogEntry, args []byte, next ExecFn) (*types.ToolResult, error)

// Execute 单一执行入口。
func (d *Dispatcher) Execute(ctx context.Context, name string, args []byte) (*types.ToolResult, error) {
	entry, ok := d.catalog.Lookup(name)
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "dispatch: tool not found: "+name)
	}
	return d.runChain(ctx, entry, args)
}

// ExecuteWithTaint 与 Execute 走同一条拦截器链（SchemaValidate → Audit → route），
// 区别在于允许调用方（Agent Kernel）传入运行时动态计算出的污点级别，
// 与 catalog 静态声明的 TaintLevel 取 only-up（只升不降，遵循 HE-7 污点传播不变量）。
func (d *Dispatcher) ExecuteWithTaint(ctx context.Context, name string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	entry, ok := d.catalog.Lookup(name)
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "dispatch: tool not found: "+name)
	}
	if taintLevel > entry.TaintLevel {
		entry.TaintLevel = taintLevel
	}
	return d.runChain(ctx, entry, args)
}

// Lookup 供 Agent Kernel 等调用方查询工具元数据。
func (d *Dispatcher) Lookup(name string) (types.Tool, error) {
	if d.toolReg == nil {
		return types.Tool{}, apperr.New(apperr.CodeInternal, "dispatch: tool registry not configured")
	}
	tool, err := d.toolReg.Lookup(name)
	if err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal, "Dispatcher.Lookup", err)
	}
	return tool, nil
}

func (d *Dispatcher) runChain(ctx context.Context, entry protocol.CatalogEntry, args []byte) (*types.ToolResult, error) {
	idx := 0
	var next ExecFn
	next = func(ctx context.Context, e protocol.CatalogEntry, a []byte) (*types.ToolResult, error) {
		if idx < len(d.chain) {
			i := idx
			idx++
			return d.chain[i](ctx, e, a, next)
		}
		return d.route(ctx, e, a)
	}
	return next(ctx, entry, args)
}

func (d *Dispatcher) route(ctx context.Context, entry protocol.CatalogEntry, args []byte) (*types.ToolResult, error) {
	// Skill 的 LLM 调用名（entry.Name，如 skill_catalog.go 剥离 "skill:" 前缀后的裸名）
	// 与 InProcessSandbox/InMemoryToolRegistry 实际注册名（"skill__{slug}"）不是同一字符串，
	// 无法通过 ToolRegistry.Lookup 按名定位；ExecuteSkill 直接用 entry.SkillName（"skill:xxx"
	// DB 主键）查询 SkillRegistry，是唯一正确的路由方式，因此单独分支。
	if entry.Source == types.ToolSkill && d.skillExec != nil {
		output, err := d.skillExec.ExecuteSkill(ctx, entry.SkillName, args)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "dispatch: execute skill "+entry.SkillName, err)
		}
		return &types.ToolResult{Success: true, Output: output, TaintLevel: entry.TaintLevel}, nil
	}

	if d.toolReg == nil {
		return nil, apperr.New(apperr.CodeInternal, "dispatch: tool registry not configured (deny-by-default)")
	}
	// builtin/mcp/native 等全部来源统一走 ToolRegistry.ExecuteTool：
	// PolicyGate → Capability Token → 沙箱分级 → 执行 → Taint only-up 传播 → RateLimit/Idempotency，
	// 与 Agent Kernel（internal/agent/agent_execute.go）完全同一条路径，无第二套实现。
	return d.toolReg.ExecuteTool(ctx, entry.Name, args, entry.TaintLevel)
}
