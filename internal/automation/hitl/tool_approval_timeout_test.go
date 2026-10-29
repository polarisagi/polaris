package hitl

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// 统一权限矩阵的审批：任何权限模式、任何风险等级下，超时都拒绝（无人应答不等于同意）。
func TestResolveTimeoutAction_ToolApprovalAlwaysDenies(t *testing.T) {
	g := &GatewayImpl{}
	for _, mode := range []types.PermissionMode{types.ModeDefault, types.ModeAutoReview, types.ModeFullAccess} {
		for _, risk := range []int{0, 1, 2, 3} {
			p := types.HITLPrompt{CheckpointType: types.CheckpointToolApproval, PermissionMode: mode, RiskLevel: risk}
			if got := g.resolveTimeoutAction(p); got != "auto_deny" {
				t.Errorf("mode=%s risk=%d: got %q want auto_deny", mode, risk, got)
			}
		}
	}
}
