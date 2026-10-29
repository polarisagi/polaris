package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/types"
)

// captureAuditLogger 记录审计写入的 action 与元数据。
type captureAuditLogger struct {
	mu      sync.Mutex
	actions []string
	metas   []map[string]any
}

func (c *captureAuditLogger) Log(_ context.Context, action string, meta map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actions = append(c.actions, action)
	c.metas = append(c.metas, meta)
	return nil
}

// hitlAutoApprovedCount 读取 hitl_auto_approved_total 中 checkpoint_type=code_act_taint 的累计值。
func hitlAutoApprovedCount(t *testing.T, r *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != "polaris.security.hitl_auto_approved_total" {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value("checkpoint_type"); ok && v.AsString() == "code_act_taint" {
					n += dp.Value
				}
			}
		}
	}
	return n
}

// TestCodeAct_FullProfile_TaintBypassIsAudited 验证 R5-04：full 档下 SanitizeToSafe 失败仍放行，
// 但必须写一条 hitl_auto_approved 审计并计数 polaris.security.hitl_auto_approved_total。
func TestCodeAct_FullProfile_TaintBypassIsAudited(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping code_act production test")
	}
	origCfg := config.Get()
	defer config.Update(origCfg)
	var newCfg config.Config
	if origCfg != nil {
		newCfg = *origCfg
	}
	newCfg.Security.TrustProfile = "full"
	newCfg.Thresholds.M4Kernel.LoopMode = "staged"
	config.Update(&newCfg)

	// InitMetrics 全局只有首次调用生效，故复用 e2e 契约测试的 reader，不另行初始化。
	getE2EAutoTerminations()
	reader := e2eMetricReader

	caAdapter := setupCodeActProductionEngine(t, newCodeActTestBridgeExecutor())
	ag := NewAgentWithDefaults("test-r5-04-full-audit")
	ag.SetCodeAct(caAdapter)
	ag.InjectToolExecutor(&mockToolExecutor{})
	audit := &captureAuditLogger{}
	ag.InjectTaintAuditor(audit)

	code := `import polaris
res = polaris.call("secret_scanner")
print("FULL_SECRET:" + res.get("secret", ""))
`
	input, _ := json.Marshal(map[string]any{"language": "python", "code": code})

	before := hitlAutoApprovedCount(t, reader)
	if _, err := ag.runCodeAct(context.Background(), "python", input, types.TaintNone); err != nil {
		t.Fatalf("full 档不应阻断: %v", err)
	}

	audit.mu.Lock()
	defer audit.mu.Unlock()
	n := 0
	for i, a := range audit.actions {
		if a == "hitl_auto_approved" && audit.metas[i]["checkpoint_type"] == "code_act_taint" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("hitl_auto_approved(code_act_taint) 审计条数 = %d，期望 1（全部 action: %v）", n, audit.actions)
	}
	if delta := hitlAutoApprovedCount(t, reader) - before; delta != 1 {
		t.Errorf("hitl_auto_approved_total{checkpoint_type=code_act_taint} 增量 = %d，期望 1", delta)
	}
}
