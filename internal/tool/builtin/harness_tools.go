package builtin

import (
	"fmt"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin/harness_history"
	"github.com/polarisagi/polaris/internal/tool/builtin/harness_rollback"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// RegisterHarnessTools 注册 harness_history 与 harness_rollback 工具（ADR-0115 WP6）。
func RegisterHarnessTools(
	sbx *sandbox.InProcessSandbox,
	toolReg *tool.InMemoryToolRegistry,
	overlayMgr protocol.HarnessOverlayManager,
	auditTrail harness_rollback.AuditTrail,
) error {
	if overlayMgr == nil {
		return nil
	}

	defs := []builtinDef{
		{"harness_history", harness_history.MakeHarnessHistoryFn(overlayMgr)},
		{"harness_rollback", harness_rollback.MakeHarnessRollbackFn(overlayMgr, auditTrail)},
	}

	for _, d := range defs {
		meta, err := tool.GetBuiltinToolMeta(d.name)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("harness_tools: load meta for %q", d.name), err)
		}
		sbx.Register(meta.Name, d.fn)
		if err := toolReg.Register(meta); err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("harness_tools: register %q", d.name), err)
		}
	}
	return nil
}
