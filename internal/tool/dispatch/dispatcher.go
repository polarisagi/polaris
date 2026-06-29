package dispatch

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCPCaller abstract MCP manager
type MCPCaller interface {
	CallTool(ctx context.Context, serverID, toolName string, args []byte) (*types.ToolResult, error)
}

// Dispatcher 统一工具调用入口。
// 替代分散的 ExecuteTool / CallTool 路径。
type Dispatcher struct {
	catalog   catalog.Catalog
	envelope  *sandbox.ExecEnvelope
	mcpMgr    MCPCaller
	skillExec protocol.SkillExecutor
	chain     []Interceptor
}

func New(catalog catalog.Catalog, envelope *sandbox.ExecEnvelope, mcpMgr MCPCaller, skillExec protocol.SkillExecutor) *Dispatcher {
	return &Dispatcher{
		catalog:   catalog,
		envelope:  envelope,
		mcpMgr:    mcpMgr,
		skillExec: skillExec,
	}
}

func (d *Dispatcher) Use(interceptors ...Interceptor) {
	d.chain = append(d.chain, interceptors...)
}

type ExecFn func(ctx context.Context, entry catalog.CatalogEntry, args []byte) (*types.ToolResult, error)
type Interceptor func(ctx context.Context, entry catalog.CatalogEntry, args []byte, next ExecFn) (*types.ToolResult, error)

// Execute 单一执行入口。
func (d *Dispatcher) Execute(ctx context.Context, name string, args []byte) (*types.ToolResult, error) {
	entry, ok := d.catalog.Lookup(name)
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "dispatch: tool not found: "+name)
	}
	return d.runChain(ctx, entry, args)
}

func (d *Dispatcher) runChain(ctx context.Context, entry catalog.CatalogEntry, args []byte) (*types.ToolResult, error) {
	idx := 0
	var next ExecFn
	next = func(ctx context.Context, e catalog.CatalogEntry, a []byte) (*types.ToolResult, error) {
		if idx < len(d.chain) {
			i := idx
			idx++
			return d.chain[i](ctx, e, a, next)
		}
		return d.route(ctx, e, a)
	}
	return next(ctx, entry, args)
}

func (d *Dispatcher) route(ctx context.Context, entry catalog.CatalogEntry, args []byte) (*types.ToolResult, error) {
	switch entry.Source {
	case types.ToolMCP:
		if d.mcpMgr != nil {
			return d.mcpMgr.CallTool(ctx, entry.MCPServerID, entry.MCPToolName, args)
		}
		// Fallback to sandbox if mcpMgr not provided but tool is registered
	case types.ToolSkill:
		if d.skillExec != nil {
			output, err := d.skillExec.ExecuteSkill(ctx, entry.SkillName, args)
			if err != nil {
				return nil, err
			}
			return &types.ToolResult{Success: true, Output: output}, nil
		}
	}

	res, err := d.envelope.Execute(ctx, sandbox.ExecRequest{
		Principal:  sandbox.PrincipalAgent,
		Kind:       sandbox.KindToolExecute,
		Resource:   entry.Name,
		TrustTier:  entry.TrustTier,
		TaintLevel: entry.TaintLevel,
		Input:      args,
		CPUQuotaMs: int(entry.Timeout.Milliseconds()),
		Tool: types.Tool{
			Name:       entry.Name,
			Source:     entry.Source,
			Capability: entry.Capability,
			TrustTier:  entry.TrustTier,
			Timeout:    entry.Timeout,
		},
	})
	if err != nil {
		return nil, err
	}
	return &types.ToolResult{
		Success:    res.Success,
		Output:     res.Output,
		Error:      res.Error,
		TaintLevel: res.TaintLevel,
		ImageParts: res.ImageParts,
	}, nil
}
