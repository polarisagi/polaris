package hitl

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func rowOf(t *testing.T, db *sql.DB, id string) (status, decidedBy, reason, session string) {
	t.Helper()
	var by sql.NullString
	if err := db.QueryRow(`SELECT status, decided_by, reason, session_id FROM hitl_requests WHERE id=?`, id).
		Scan(&status, &by, &reason, &session); err != nil {
		t.Fatalf("row %s: %v", id, err)
	}
	return status, by.String, reason, session
}

func waitPending(t *testing.T, gw *GatewayImpl, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		_, ok := gw.waiters[id]
		gw.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waiter %s never registered", id)
}

// 二次 Respond 必须报错，且不改写首次裁决（条件更新幂等）。
func TestGateway_RespondIdempotent(t *testing.T) {
	gw, db := newTestGateway(t)
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "sess-1")
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	done := make(chan *types.HITLResponse, 1)
	go func() {
		r, _ := gw.Prompt(ctx, types.HITLPrompt{ID: "h1", CheckpointType: "x", AgentID: "a1"})
		done <- r
	}()
	waitPending(t, gw, "h1")

	if pend, err := gw.Pending(ctx); err != nil || len(pend) != 1 || pend[0].ID != "h1" {
		t.Fatalf("pending list: %+v %v", pend, err)
	}
	if err := gw.Respond(ctx, "h1", types.HITLResponse{Approved: true, Reason: "ok"}); err != nil {
		t.Fatalf("first respond: %v", err)
	}
	if r := <-done; r == nil || !r.Approved {
		t.Fatalf("prompt result: %+v", r)
	}
	err := gw.Respond(ctx, "h1", types.HITLResponse{Approved: false, Reason: "late"})
	if err == nil {
		t.Fatal("second respond must fail")
	}
	if !apperr.IsCode(err, apperr.CodeConflict) {
		t.Fatalf("want CONFLICT, got %v", err)
	}
	st, by, reason, sess := rowOf(t, db, "h1")
	if st != "approved" || by != "human" || reason != "ok" || sess != "sess-1" {
		t.Fatalf("row mutated: %s %s %s %s", st, by, reason, sess)
	}
	if pend, _ := gw.Pending(ctx); len(pend) != 0 {
		t.Fatalf("pending should be empty: %+v", pend)
	}
	// 未知 ID 同样报错。
	if err := gw.Respond(ctx, "nope", types.HITLResponse{Approved: false}); err == nil {
		t.Fatal("unknown id must fail")
	}
}

// kill_pause 超时必须落 timeout/timeout_kill，且不再出现在 Pending；之后的迟到批准被拒。
func TestGateway_KillPauseTimeoutRecorded(t *testing.T) {
	gw, db := newTestGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := gw.Prompt(ctx, types.HITLPrompt{ID: "h-kp", CheckpointType: "x"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	st, by, _, _ := rowOf(t, db, "h-kp")
	if st != "timeout" || by != "timeout_kill" {
		t.Fatalf("row: %s %s", st, by)
	}
	if pend, _ := gw.Pending(context.Background()); len(pend) != 0 {
		t.Fatalf("ghost pending: %+v", pend)
	}
	if err := gw.Respond(context.Background(), "h-kp", types.HITLResponse{Approved: true}); err == nil {
		t.Fatal("late approval after timeout must be rejected")
	}
}

// 超时自动放行/拒绝的 decided_by 必须是 auto_*，而非 human。
func TestGateway_AutoTimeoutDecidedBy(t *testing.T) {
	gw, db := newTestGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if r, err := gw.Prompt(ctx, types.HITLPrompt{
		ID: "h-aa", CheckpointType: types.CheckpointDeviceControlReview, PermissionMode: types.ModeFullAccess,
	}); err != nil || !r.Approved {
		t.Fatalf("auto approve: %+v %v", r, err)
	}
	if st, by, _, _ := rowOf(t, db, "h-aa"); st != "approved" || by != "auto_approve" {
		t.Fatalf("auto_approve row: %s %s", st, by)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if r, err := gw.Prompt(ctx2, types.HITLPrompt{ID: "h-ad", CheckpointType: "x", TaintLevel: types.TaintMedium}); err != nil || r.Approved {
		t.Fatalf("auto deny: %+v %v", r, err)
	}
	if st, by, _, _ := rowOf(t, db, "h-ad"); st != "denied" || by != "auto_deny" {
		t.Fatalf("auto_deny row: %s %s", st, by)
	}
}

// 信任降级放行必须落 approved/trust_downgrade 一行（降级不等于静默）。
func TestGateway_TrustDowngradeRecorded(t *testing.T) {
	gw, db := newTestGateway(t)
	gw.SetTrustScorer(NewTrustScorer(TrustPolicy{MinApprovals: 2, Window: time.Hour}, gw.store))
	p := lowRiskPrompt()
	p.ID = "h-td"
	seedHumanApprovals(t, db, p, 2)

	r, err := gw.Prompt(context.Background(), p)
	if err != nil || !r.Approved {
		t.Fatalf("downgrade: %+v %v", r, err)
	}
	st, by, reason, _ := rowOf(t, db, "h-td")
	if st != "approved" || by != "trust_downgrade" || reason != "auto_approved_by_trust_score" {
		t.Fatalf("row: %s %s %s", st, by, reason)
	}
	// 降级行本身不得继续累积信任。
	n, err := gw.store.CountHumanApprovals(context.Background(), p.CheckpointType, p.AgentID, 0)
	if err != nil || n != 2 {
		t.Fatalf("trust_downgrade must not count: n=%d err=%v", n, err)
	}
}

// 端到端：人工批准落账后可被 TrustScorer 读到；人工拒绝清零。
func TestGateway_HumanDecisionsFeedTrust(t *testing.T) {
	gw, _ := newTestGateway(t)
	ctx := context.Background()
	scorer := NewTrustScorer(TrustPolicy{MinApprovals: 1, Window: time.Hour}, gw.store)
	p := lowRiskPrompt()

	decide := func(id string, approved bool) {
		p.ID = id
		done := make(chan struct{})
		go func() { _, _ = gw.Prompt(ctx, p); close(done) }()
		waitPending(t, gw, id)
		if err := gw.Respond(ctx, id, types.HITLResponse{Approved: approved}); err != nil {
			t.Fatal(err)
		}
		<-done
		time.Sleep(3 * time.Millisecond) // decided_at 为毫秒精度，拉开先后
	}
	decide("e1", true)
	if !scorer.ShouldDowngrade(ctx, p) {
		t.Fatal("one human approval with threshold 1 should downgrade")
	}
	decide("e2", false)
	if scorer.ShouldDowngrade(ctx, p) {
		t.Fatal("human denial must reset")
	}
}

// L3 门禁 P0 失败自动拒绝落 denied/l3_gate。
func TestGateway_L3AutoDenyRecorded(t *testing.T) {
	gw, db := newTestGateway(t)
	p := types.HITLPrompt{ID: "h-l3", CheckpointType: "l4_multi_sig"}
	resp := gw.autoDenyOnP0Regression(context.Background(), &p)
	if resp == nil || resp.Approved {
		t.Fatalf("resp: %+v", resp)
	}
	if st, by, reason, _ := rowOf(t, db, "h-l3"); st != "denied" || by != "l3_gate" || reason != "auto_denied_p0_regression_failed" {
		t.Fatalf("row: %s %s %s", st, by, reason)
	}
}

// 冷却期内批准被拒，且行保持 pending（未被抢占）。
func TestGateway_CooldownKeepsPending(t *testing.T) {
	gw, db := newTestGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		_, _ = gw.Prompt(ctx, types.HITLPrompt{ID: "h-cd", CheckpointType: "x", EligibleApproveTime: time.Now().Add(time.Hour).Unix()})
	}()
	waitPending(t, gw, "h-cd")
	if err := gw.Respond(ctx, "h-cd", types.HITLResponse{Approved: true}); !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("want FORBIDDEN, got %v", err)
	}
	if st, _, _, _ := rowOf(t, db, "h-cd"); st != repo.HITLStatusPending {
		t.Fatalf("status: %s", st)
	}
}
