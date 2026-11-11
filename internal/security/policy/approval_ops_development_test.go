package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestDevelopmentStatusActionsAreReadOnlyButStillCheckTargets(t *testing.T) {
	ctx, root := approvalCtx(t)
	ctx.Home = filepath.Join(root, "home")
	if err := ensureTestDir(ctx.Home); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mode types.PermissionMode
		tool types.Tool
		args map[string]any
		want Verdict
	}{
		{"file edit status", types.ModeAutoReview, types.Tool{Name: "file_edit", Capability: types.CapWriteLocal, RiskLevel: types.RiskMedium}, map[string]any{"action": "status", "path": filepath.Join(root, "project")}, VerdictAllow},
		{"preview status", types.ModeAutoReview, types.Tool{Name: "project_preview", Capability: types.CapWriteLocal, RiskLevel: types.RiskHigh}, map[string]any{"action": "status", "path": filepath.Join(root, "project"), "job_id": "job-1"}, VerdictAllow},
		{"file edit status sensitive path", types.ModeAutoReview, types.Tool{Name: "file_edit", Capability: types.CapWriteLocal, RiskLevel: types.RiskMedium}, map[string]any{"action": "status", "path": filepath.Join(ctx.Home, ".ssh", "id_ed25519")}, VerdictAsk},
		{"preview status sensitive path", types.ModeAutoReview, types.Tool{Name: "project_preview", Capability: types.CapWriteLocal, RiskLevel: types.RiskHigh}, map[string]any{"action": "status", "path": filepath.Join(ctx.Home, ".ssh", "id_ed25519"), "job_id": "job-1"}, VerdictAsk},
		{"file edit unknown action", types.ModeDefault, types.Tool{Name: "file_edit", Capability: types.CapWriteLocal, RiskLevel: types.RiskMedium}, map[string]any{"action": "mystery", "path": filepath.Join(root, "project")}, VerdictAsk},
		{"preview unknown action", types.ModeDefault, types.Tool{Name: "project_preview", Capability: types.CapWriteLocal, RiskLevel: types.RiskHigh}, map[string]any{"action": "mystery", "path": filepath.Join(root, "project")}, VerdictAsk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.mode, tc.tool, jsonArgs(t, tc.args), types.TaintLow, ctx)
			if d.Verdict != tc.want {
				t.Fatalf("verdict=%v reason=%q want=%v", d.Verdict, d.Reason, tc.want)
			}
			if tc.want == VerdictAllow && d.Auto != "read" {
				t.Fatalf("status action auto=%q, want read", d.Auto)
			}
			if tc.want == VerdictAsk && !d.HardLimit && tc.mode == types.ModeAutoReview {
				t.Fatal("sensitive target must remain a hard approval boundary")
			}
		})
	}
}

func ensureTestDir(path string) error { return os.MkdirAll(path, 0700) }
