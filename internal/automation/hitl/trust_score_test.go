package hitl

import (
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func lowRiskPrompt() types.HITLPrompt {
	return types.HITLPrompt{
		CheckpointType: "code_act_warning",
		AgentID:        "agent-1",
		RiskLevel:      1,
		TaintLevel:     types.TaintNone,
	}
}

func approveN(s *TrustScorer, p types.HITLPrompt, n int) {
	for range n {
		s.RecordDecision(p, true)
	}
}

// TestTrustScorer_DisabledByDefault 未配置时必须完全等价于机制不存在。
// 安全机制的默认值只能是"关"。
func TestTrustScorer_DisabledByDefault(t *testing.T) {
	var nilScorer *TrustScorer
	if nilScorer.Enabled() {
		t.Fatal("nil scorer must report disabled")
	}
	if nilScorer.ShouldDowngrade(lowRiskPrompt()) {
		t.Fatal("nil scorer must never downgrade")
	}
	nilScorer.RecordDecision(lowRiskPrompt(), true) // 不得 panic

	zero := NewTrustScorer(TrustPolicy{}) // MinApprovals=0
	if zero.Enabled() {
		t.Fatal("MinApprovals=0 must report disabled")
	}
	approveN(zero, lowRiskPrompt(), 100)
	if zero.ShouldDowngrade(lowRiskPrompt()) {
		t.Fatal("disabled scorer must never downgrade regardless of approval count")
	}
}

// TestTrustScorer_DowngradesAfterThreshold 达到阈值后降级；未达到时不降级。
func TestTrustScorer_DowngradesAfterThreshold(t *testing.T) {
	s := NewTrustScorer(TrustPolicy{MinApprovals: 3, Window: time.Hour})
	p := lowRiskPrompt()

	approveN(s, p, 2)
	if s.ShouldDowngrade(p) {
		t.Fatal("must not downgrade below the configured threshold")
	}
	approveN(s, p, 1)
	if !s.ShouldDowngrade(p) {
		t.Fatal("must downgrade once the threshold is reached")
	}
}

// TestTrustScorer_DenialResetsTrust 任何一次人工拒绝都意味着"这类请求仍需人看"，
// 信任必须立即清零而非缓慢衰减。
func TestTrustScorer_DenialResetsTrust(t *testing.T) {
	s := NewTrustScorer(TrustPolicy{MinApprovals: 3, Window: time.Hour})
	p := lowRiskPrompt()

	approveN(s, p, 5)
	if !s.ShouldDowngrade(p) {
		t.Fatal("precondition: should be downgradable")
	}
	s.RecordDecision(p, false)
	if s.ShouldDowngrade(p) {
		t.Fatal("a single human denial must reset accumulated trust to zero")
	}
	approveN(s, p, 2)
	if s.ShouldDowngrade(p) {
		t.Fatal("trust must re-accumulate from scratch after a denial")
	}
}

// TestTrustScorer_HardFloorsNeverDowngrade 硬地板：无论累积多少次批准，
// 这几类请求永远不参与降级。这组条件与 resolveTimeoutAction 的地板一致——
// 一旦被放宽，会出现"超时不敢自动放行、但疲劳降级放行了"的荒谬组合。
func TestTrustScorer_HardFloorsNeverDowngrade(t *testing.T) {
	cases := map[string]func(p *types.HITLPrompt){
		"tainted content":      func(p *types.HITLPrompt) { p.TaintLevel = types.TaintMedium },
		"high taint":           func(p *types.HITLPrompt) { p.TaintLevel = types.TaintHigh },
		"high risk level":      func(p *types.HITLPrompt) { p.RiskLevel = 3 },
		"device control":       func(p *types.HITLPrompt) { p.CheckpointType = types.CheckpointDeviceControlReview },
		"l4 self-improve":      func(p *types.HITLPrompt) { p.CheckpointType = "l4_multi_sig" },
		"unattributable agent": func(p *types.HITLPrompt) { p.AgentID = "" },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := NewTrustScorer(TrustPolicy{MinApprovals: 2, Window: time.Hour})
			p := lowRiskPrompt()
			mutate(&p)

			approveN(s, p, 50) // 远超阈值
			if s.ShouldDowngrade(p) {
				t.Fatalf("hard floor %q must never be downgraded, no matter the approval history", name)
			}
		})
	}
}

// TestTrustScorer_WindowExpiry 信任不应无限期留存——"三个月前批过 10 次"
// 不构成今天放行的理由。
func TestTrustScorer_WindowExpiry(t *testing.T) {
	s := NewTrustScorer(TrustPolicy{MinApprovals: 2, Window: time.Millisecond})
	p := lowRiskPrompt()

	approveN(s, p, 3)
	time.Sleep(5 * time.Millisecond)
	if s.ShouldDowngrade(p) {
		t.Fatal("trust must expire once the window elapses")
	}
}

// TestTrustScorer_ScopedPerAgentAndType 信任不得跨 Agent 或跨 checkpoint 类型
// 泄漏——A Agent 的批准记录不能惠及 B Agent。
func TestTrustScorer_ScopedPerAgentAndType(t *testing.T) {
	s := NewTrustScorer(TrustPolicy{MinApprovals: 2, Window: time.Hour})
	p := lowRiskPrompt()
	approveN(s, p, 5)

	otherAgent := p
	otherAgent.AgentID = "agent-2"
	if s.ShouldDowngrade(otherAgent) {
		t.Fatal("trust must not leak across agents")
	}

	otherType := p
	otherType.CheckpointType = "security_review"
	if s.ShouldDowngrade(otherType) {
		t.Fatal("trust must not leak across checkpoint types")
	}
}
