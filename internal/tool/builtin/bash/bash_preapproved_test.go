package bash

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/classifier"
	"github.com/polarisagi/polaris/internal/security/policy"
)

type countingHITL struct{ calls int }

func (c *countingHITL) RequestApproval(context.Context, string, map[string]any) error {
	c.calls++
	return nil
}

// 统一权限判定已放行/获批的调用，工具内的命令风险 HITL 不再重复询问；未标记时照常询问。
func TestEvaluateBashRisk_PreApprovedSkipsInToolPrompt(t *testing.T) {
	cl := classifier.NewDefaultClassifier()
	pol := policy.ProfilePolicy(policy.ProfileStandard)

	h := &countingHITL{}
	if _, err := evaluateBashRisk(context.Background(), "rm -rf build", pol, cl, h); err != nil || h.calls != 1 {
		t.Fatalf("without the marker the in-tool prompt must fire once, calls=%d err=%v", h.calls, err)
	}
	h = &countingHITL{}
	ctx := protocol.WithPreApproved(context.Background())
	if _, err := evaluateBashRisk(ctx, "rm -rf build", pol, cl, h); err != nil || h.calls != 0 {
		t.Fatalf("pre-approved call must not prompt again, calls=%d err=%v", h.calls, err)
	}
}
