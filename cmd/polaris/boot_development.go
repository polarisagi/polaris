package main

import (
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/network"
	polartool "github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin"
	builtinbash "github.com/polarisagi/polaris/internal/tool/builtin/bash"
	toolsandbox "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func registerDevelopmentTools(sb *SubstrateBundle, inProc *sandbox.InProcessSandbox, reg *polartool.InMemoryToolRegistry, allowedPaths []string) error {
	if sb == nil || sb.Cfg == nil || sb.Store == nil || sb.Layout.Workspace == "" || len(allowedPaths) == 0 || inProc == nil || reg == nil {
		return apperr.New(apperr.CodeServiceUnavailable, "boot_development: required development tool dependency is unavailable")
	}
	if err := builtin.RegisterDevelopmentTools(inProc, reg, builtin.DevelopmentToolDeps{
		AllowedPaths:   allowedPaths,
		Store:          sb.Store,
		Registry:       reg,
		SafeHTTPClient: network.NewLoopbackSafeHTTPClient(sb.Cfg.Thresholds.M11Policy),
		JobManager:     builtinbash.GetDefaultJobManager(),
		CmdRunner:      toolsandbox.NewBoundedCmdRunner(),
		ScratchRoot:    sb.Layout.Workspace,
	}); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "boot_development: register development tools", err)
	}
	return nil
}
