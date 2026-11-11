package builtin

import (
	"fmt"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	codepatchtest "github.com/polarisagi/polaris/internal/tool/builtin/code_patch_test"
	"github.com/polarisagi/polaris/internal/tool/builtin/file_edit"
	"github.com/polarisagi/polaris/internal/tool/builtin/project_diagnostics"
	"github.com/polarisagi/polaris/internal/tool/builtin/project_preview"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type DevelopmentToolDeps struct {
	AllowedPaths   []string
	Store          protocol.Store
	Registry       protocol.ToolRegistry
	SafeHTTPClient network.SafeHTTPClient
	JobManager     *bash.JobManager
	CmdRunner      sandbox.CmdRunner
	ScratchRoot    string
}

type developmentToolDef struct {
	name string
	fn   sandbox.InProcessFn
	rich sandbox.InProcessRichFn
}

// RegisterDevelopmentTools 注册内置开发工作流工具，执行仍经统一 ToolRegistry。
func RegisterDevelopmentTools(sbx *sandbox.InProcessSandbox, reg *tool.InMemoryToolRegistry, deps DevelopmentToolDeps) error {
	if sbx == nil || reg == nil {
		return apperr.New(apperr.CodeServiceUnavailable, "development_tools: sandbox and tool registry are required")
	}
	deps.AllowedPaths = append([]string(nil), deps.AllowedPaths...)
	registry := deps.Registry
	if registry == nil {
		registry = reg
	}
	defs := []developmentToolDef{
		{name: "file_edit", fn: file_edit.MakeFileEditFn(file_edit.Deps{AllowedPaths: deps.AllowedPaths, Store: deps.Store})},
		{name: "project_diagnostics", rich: project_diagnostics.MakeProjectDiagnosticsFn(project_diagnostics.Deps{AllowedPaths: deps.AllowedPaths, Registry: registry})},
		{name: "project_preview", rich: project_preview.MakeProjectPreviewFn(project_preview.Deps{AllowedPaths: deps.AllowedPaths, Registry: registry, Store: deps.Store, SafeHTTPClient: deps.SafeHTTPClient, JobManager: deps.JobManager})},
		{name: "code_patch_test", fn: codepatchtest.MakeCodePatchTestFn(codepatchtest.Deps{CmdRunner: deps.CmdRunner, ScratchRoot: deps.ScratchRoot, AllowedPaths: deps.AllowedPaths})},
	}
	metas := make([]types.Tool, len(defs))
	for i, def := range defs {
		meta, err := tool.GetBuiltinToolMeta(def.name)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("development_tools: load metadata for %q", def.name), err)
		}
		if def.name == "code_patch_test" {
			meta.Timeout = codepatchtest.TotalTimeout
		}
		metas[i] = meta
	}
	registered := make([]string, 0, len(defs))
	for i, def := range defs {
		meta := metas[i]
		if def.rich != nil {
			sbx.RegisterRich(def.name, def.rich, types.TaintHigh)
		} else {
			sbx.RegisterWithTaint(def.name, def.fn, types.TaintHigh)
		}
		if err := reg.Register(meta); err != nil {
			sbx.Unregister(def.name)
			for _, name := range registered {
				reg.Unregister(name)
				sbx.Unregister(name)
			}
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("development_tools: register %q", def.name), err)
		}
		registered = append(registered, def.name)
	}
	return nil
}
