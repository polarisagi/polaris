package hitl

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
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

var trustSeq int

// seedDecision 直接写一行终态记录，decided_at 由测试显式指定以控制先后与窗口。
func seedDecision(t *testing.T, db *sql.DB, p types.HITLPrompt, status, decidedBy string, decidedAt time.Time) {
	t.Helper()
	trustSeq++
	id := fmt.Sprintf("seed-%d", trustSeq)
	_, err := db.Exec(`INSERT INTO hitl_requests(id,agent_id,checkpoint_type,prompt_json,status,decided_by,created_at,decided_at)
		VALUES(?,?,?,?,?,?,?,?)`, id, p.AgentID, p.CheckpointType, "{}", status, decidedBy,
		decidedAt.UnixMilli(), decidedAt.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
}

func seedHumanApprovals(t *testing.T, db *sql.DB, p types.HITLPrompt, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedDecision(t, db, p, repo.HITLStatusApproved, repo.HITLByHuman, time.Now().Add(-time.Duration(n-i)*time.Minute))
	}
}

func newTrustFixture(t *testing.T, policy TrustPolicy) (*TrustScorer, *sql.DB) {
	t.Helper()
	gw, db := newTestGateway(t)
	return NewTrustScorer(policy, gw.store), db
}

// TestTrustScorer_DisabledByDefault 未配置时必须完全等价于机制不存在。
// 安全机制的默认值只能是"关"。
func TestTrustScorer_DisabledByDefault(t *testing.T) {
	ctx := context.Background()
	var nilScorer *TrustScorer
	if nilScorer.Enabled() {
		t.Fatal("nil scorer must report disabled")
	}
	if nilScorer.ShouldDowngrade(ctx, lowRiskPrompt()) {
		t.Fatal("nil scorer must never downgrade")
	}

	zero, db := newTrustFixture(t, TrustPolicy{}) // MinApprovals=0
	if zero.Enabled() {
		t.Fatal("MinApprovals=0 must report disabled")
	}
	seedHumanApprovals(t, db, lowRiskPrompt(), 100)
	if zero.ShouldDowngrade(ctx, lowRiskPrompt()) {
		t.Fatal("disabled scorer must never downgrade regardless of approval count")
	}

	if NewTrustScorer(TrustPolicy{MinApprovals: 1}, nil).ShouldDowngrade(ctx, lowRiskPrompt()) {
		t.Fatal("scorer without history must never downgrade")
	}
}

// TestTrustScorer_DowngradesAfterThreshold 达到阈值后降级；未达到时不降级。
func TestTrustScorer_DowngradesAfterThreshold(t *testing.T) {
	ctx := context.Background()
	s, db := newTrustFixture(t, TrustPolicy{MinApprovals: 3, Window: time.Hour})
	p := lowRiskPrompt()

	seedHumanApprovals(t, db, p, 2)
	if s.ShouldDowngrade(ctx, p) {
		t.Fatal("must not downgrade below the configured threshold")
	}
	seedHumanApprovals(t, db, p, 1)
	if !s.ShouldDowngrade(ctx, p) {
		t.Fatal("must downgrade once the threshold is reached")
	}
}

// TestTrustScorer_DenialResetsTrust 任何一次人工拒绝都意味着"这类请求仍需人看"，
// 信任必须立即清零而非缓慢衰减。
func TestTrustScorer_DenialResetsTrust(t *testing.T) {
	ctx := context.Background()
	s, db := newTrustFixture(t, TrustPolicy{MinApprovals: 3, Window: time.Hour})
	p := lowRiskPrompt()

	seedHumanApprovals(t, db, p, 5)
	if !s.ShouldDowngrade(ctx, p) {
		t.Fatal("precondition: should be downgradable")
	}
	seedDecision(t, db, p, repo.HITLStatusDenied, repo.HITLByHuman, time.Now())
	if s.ShouldDowngrade(ctx, p) {
		t.Fatal("a single human denial must reset accumulated trust to zero")
	}
	for i := 0; i < 2; i++ {
		seedDecision(t, db, p, repo.HITLStatusApproved, repo.HITLByHuman, time.Now().Add(time.Duration(i+1)*time.Second))
	}
	if s.ShouldDowngrade(ctx, p) {
		t.Fatal("trust must re-accumulate from scratch after a denial")
	}
	seedDecision(t, db, p, repo.HITLStatusApproved, repo.HITLByHuman, time.Now().Add(5*time.Second))
	if !s.ShouldDowngrade(ctx, p) {
		t.Fatal("three approvals after the denial must re-enable downgrade")
	}
}

// TestTrustScorer_OnlyHumanDecisionsCount 自动决策不得计入信任：
// 降级产生的"通过"反过来加固降级依据会形成正反馈（GD-14-004 防线）。
func TestTrustScorer_OnlyHumanDecisionsCount(t *testing.T) {
	ctx := context.Background()
	s, db := newTrustFixture(t, TrustPolicy{MinApprovals: 2, Window: time.Hour})
	p := lowRiskPrompt()

	for _, by := range []string{repo.HITLByAutoApprove, repo.HITLByTrustDowngrade} {
		for i := 0; i < 10; i++ {
			seedDecision(t, db, p, repo.HITLStatusApproved, by, time.Now().Add(-time.Minute))
		}
	}
	seedDecision(t, db, p, repo.HITLStatusApproved, repo.HITLByHuman, time.Now().Add(-time.Minute))
	if s.ShouldDowngrade(ctx, p) {
		t.Fatal("auto_approve / trust_downgrade rows must not count toward trust")
	}
	// 自动拒绝也不应清零人工累积。
	seedHumanApprovals(t, db, p, 1)
	seedDecision(t, db, p, repo.HITLStatusDenied, repo.HITLByAutoDeny, time.Now())
	if !s.ShouldDowngrade(ctx, p) {
		t.Fatal("auto_deny must not reset human-accumulated trust")
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
			s, db := newTrustFixture(t, TrustPolicy{MinApprovals: 2, Window: time.Hour})
			p := lowRiskPrompt()
			mutate(&p)

			seedHumanApprovals(t, db, p, 50) // 远超阈值
			if s.ShouldDowngrade(context.Background(), p) {
				t.Fatalf("hard floor %q must never be downgraded, no matter the approval history", name)
			}
		})
	}
}

// TestTrustScorer_WindowExpiry 信任不应无限期留存——"三个月前批过 10 次"
// 不构成今天放行的理由。Window 外的批准不计。
func TestTrustScorer_WindowExpiry(t *testing.T) {
	ctx := context.Background()
	s, db := newTrustFixture(t, TrustPolicy{MinApprovals: 2, Window: time.Hour})
	p := lowRiskPrompt()

	for i := 0; i < 5; i++ {
		seedDecision(t, db, p, repo.HITLStatusApproved, repo.HITLByHuman, time.Now().Add(-2*time.Hour))
	}
	if s.ShouldDowngrade(ctx, p) {
		t.Fatal("approvals outside the window must not count")
	}
	seedDecision(t, db, p, repo.HITLStatusApproved, repo.HITLByHuman, time.Now().Add(-10*time.Minute))
	if s.ShouldDowngrade(ctx, p) {
		t.Fatal("one in-window approval is below threshold 2")
	}
	seedDecision(t, db, p, repo.HITLStatusApproved, repo.HITLByHuman, time.Now().Add(-5*time.Minute))
	if !s.ShouldDowngrade(ctx, p) {
		t.Fatal("two in-window approvals reach threshold")
	}
}

// TestTrustScorer_ScopedPerAgentAndType 信任不得跨 Agent 或跨 checkpoint 类型
// 泄漏——A Agent 的批准记录不能惠及 B Agent。
func TestTrustScorer_ScopedPerAgentAndType(t *testing.T) {
	ctx := context.Background()
	s, db := newTrustFixture(t, TrustPolicy{MinApprovals: 2, Window: time.Hour})
	p := lowRiskPrompt()
	seedHumanApprovals(t, db, p, 5)

	otherAgent := p
	otherAgent.AgentID = "agent-2"
	if s.ShouldDowngrade(ctx, otherAgent) {
		t.Fatal("trust must not leak across agents")
	}

	otherType := p
	otherType.CheckpointType = "security_review"
	if s.ShouldDowngrade(ctx, otherType) {
		t.Fatal("trust must not leak across checkpoint types")
	}
	// 别的 Agent 的人工拒绝不得清零本 Agent 的信任。
	seedDecision(t, db, otherAgent, repo.HITLStatusDenied, repo.HITLByHuman, time.Now())
	if !s.ShouldDowngrade(ctx, p) {
		t.Fatal("another agent's denial must not reset this agent's trust")
	}
}

type failingHistory struct{}

func (failingHistory) CountHumanApprovals(context.Context, string, string, int64) (int, error) {
	return 0, apperr.New(apperr.CodeInternal, "db down")
}

// TestTrustScorer_QueryErrorFailsClosed 查表失败必须不降级：安全边界无法确认历史时回到人工审批。
func TestTrustScorer_QueryErrorFailsClosed(t *testing.T) {
	s := NewTrustScorer(TrustPolicy{MinApprovals: 1, Window: time.Hour}, failingHistory{})
	if s.ShouldDowngrade(context.Background(), lowRiskPrompt()) {
		t.Fatal("query error must not downgrade")
	}
}
