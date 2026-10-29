package mesh

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security"
	storerepo "github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ddl, err := schema.FS.ReadFile("048_mesh.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return db
}

// fakeBlackboard 只实现委派服务用到的三个方法；其余方法调用即 panic（嵌入 nil 接口），
// 防止服务悄悄依赖了未声明的黑板能力。
type fakeBlackboard struct {
	protocol.Blackboard
	mu       sync.Mutex
	posted   []*types.TaskEntry
	snaps    map[string]*types.TaskSnapshot
	postErr  error
	canceled []string
}

func newFakeBB() *fakeBlackboard { return &fakeBlackboard{snaps: map[string]*types.TaskSnapshot{}} }

func (f *fakeBlackboard) PostTask(_ context.Context, e *types.TaskEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return f.postErr
	}
	f.posted = append(f.posted, e)
	f.snaps[e.ID] = &types.TaskSnapshot{ID: e.ID, Status: types.TaskPending, Type: e.Type, Namespace: e.Namespace,
		Intent: e.Intent, SpawnDepth: e.SpawnDepth}
	return nil
}

func (f *fakeBlackboard) PeekTask(_ context.Context, id string) (*types.TaskSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.snaps[id]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeBlackboard) CancelTask(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, id)
	if s, ok := f.snaps[id]; ok {
		s.Status = types.TaskFailed
		s.Result = []byte("cancelled_by_saga_coordinator")
	}
	return nil
}

func (f *fakeBlackboard) set(id string, st types.TaskStatus, result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[id].Status = st
	f.snaps[id].Result = []byte(result)
}

func testIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := GetOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newTestService(t *testing.T) (*Service, *fakeBlackboard) {
	t.Helper()
	bb := newFakeBB()
	return NewService(storerepo.NewSQLiteMeshRepository(newTestDB(t)), bb, testIdentity(t), DefaultConfig()), bb
}

func peer(node string) Principal {
	return Principal{TokenID: "ptk-x", PeerName: node, PeerNodeID: node, Scopes: AllScopes()}
}

// ─── 身份 ────────────────────────────────────────────────────────────────

func TestIdentity_StableAcrossLoads(t *testing.T) {
	dir := t.TempDir()
	a, err := GetOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GetOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.NodeID != b.NodeID || !a.PublicKey.Equal(b.PublicKey) {
		t.Fatalf("identity must be stable across restarts: %s vs %s", a.NodeID, b.NodeID)
	}
	if a.NodeID != NodeIDFromPublicKey(a.PublicKey) || !strings.HasPrefix(a.NodeID, "pn-") {
		t.Fatalf("node id must be derived from the public key, got %s", a.NodeID)
	}
}

// ─── 令牌 ────────────────────────────────────────────────────────────────

func TestTokens_IssueVerifyRevoke(t *testing.T) {
	ctx := context.Background()
	ts := NewTokenStore(storerepo.NewSQLiteMeshRepository(newTestDB(t)))
	plain, info, err := ts.Issue(ctx, "desktop", "pn-aaa", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Fatalf("token must carry prefix %s", TokenPrefix)
	}
	p, err := ts.Verify(ctx, plain)
	if err != nil || p.PeerNodeID != "pn-aaa" || !p.Has(ScopeSend) {
		t.Fatalf("verify: %+v %v", p, err)
	}
	if err := ts.Revoke(ctx, info.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Verify(ctx, plain); apperr.CodeOf(err) != apperr.CodeUnauthorized || err.Error() != ErrTokenRevoked.Error() {
		t.Fatalf("revoked token must be rejected, got %v", err)
	}
	if err := ts.Revoke(ctx, "ptk-nope"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("revoking unknown token must be NotFound, got %v", err)
	}
}

func TestTokens_Supersede(t *testing.T) {
	ctx := context.Background()
	ts := NewTokenStore(storerepo.NewSQLiteMeshRepository(newTestDB(t)))
	bound, _, _ := ts.Issue(ctx, "desktop", "pn-aaa", nil, time.Hour)
	oldPending, _, _ := ts.Issue(ctx, "desktop", PendingNodeID("desktop"), nil, time.Hour)
	other, _, _ := ts.Issue(ctx, "vps", PendingNodeID("vps"), nil, time.Hour)
	newest, info, _ := ts.Issue(ctx, "desktop", PendingNodeID("desktop"), nil, time.Hour)

	// 新邀请只作废同名的旧待绑定令牌：已绑定的、别的对端的、自己都保留。
	if n, err := ts.Supersede(ctx, "desktop", info.TokenID, true); err != nil || n != 1 {
		t.Fatalf("pending-only supersede must revoke exactly the old invite, got %d %v", n, err)
	}
	if _, err := ts.Verify(ctx, oldPending); err == nil {
		t.Fatal("superseded invite token must stop working")
	}
	for _, tok := range []string{bound, other, newest} {
		if _, err := ts.Verify(ctx, tok); err != nil {
			t.Fatalf("unrelated token must survive: %v", err)
		}
	}
	// 配对完成后，同名其余令牌（含已绑定的旧令牌）一并作废。
	if n, err := ts.Supersede(ctx, "desktop", info.TokenID, false); err != nil || n != 1 {
		t.Fatalf("full supersede must revoke the old bound token, got %d %v", n, err)
	}
	if _, err := ts.Verify(ctx, bound); err == nil {
		t.Fatal("old bound token must stop working after re-pairing")
	}
	if _, err := ts.Verify(ctx, other); err != nil {
		t.Fatal("other peer's token must survive")
	}
}

func TestTokens_FirstUseRetiresOlderTokensOfSameNode(t *testing.T) {
	ctx := context.Background()
	ts := NewTokenStore(storerepo.NewSQLiteMeshRepository(newTestDB(t)))
	base := time.Now()
	ts.now = func() time.Time { return base }
	old, _, _ := ts.Issue(ctx, "desktop", "pn-aaa", nil, time.Hour)
	other, _, _ := ts.Issue(ctx, "vps", "pn-bbb", nil, time.Hour)
	ts.now = func() time.Time { return base.Add(time.Second) }
	newer, _, _ := ts.Issue(ctx, "desktop", "pn-aaa", nil, time.Hour)

	if _, err := ts.Verify(ctx, old); err != nil { // 旧令牌先被用：不影响更新的
		t.Fatal(err)
	}
	if _, err := ts.Verify(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Verify(ctx, old); err == nil {
		t.Fatal("older token of the same node must be retired once the newer one is used")
	}
	if _, err := ts.Verify(ctx, other); err != nil {
		t.Fatal("tokens of other nodes must be untouched")
	}
}

func TestTokens_ExpiredAndUnknown(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ts := NewTokenStore(storerepo.NewSQLiteMeshRepository(db))
	plain, _, err := ts.Issue(ctx, "vps", "", []string{ScopeRead}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ts.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := ts.Verify(ctx, plain); err == nil || err.Error() != ErrTokenExpired.Error() {
		t.Fatalf("expired token must be rejected, got %v", err)
	}
	if _, err := ts.Verify(ctx, TokenPrefix+"forged"); err == nil || err.Error() != ErrTokenUnknown.Error() {
		t.Fatalf("unknown token must be rejected, got %v", err)
	}
	// 库里只存摘要：明文不得出现在任何列里。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mesh_peer_tokens WHERE token_hash = ? OR token_id = ?`, plain, plain).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plaintext token must never be stored (n=%d, err=%v)", n, err)
	}
	if _, _, err := ts.Issue(ctx, "x", "", []string{"admin"}, 0); apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Fatalf("unknown scope must be refused, got %v", err)
	}
}

// ─── 链校验 ──────────────────────────────────────────────────────────────

func TestValidateChain(t *testing.T) {
	const self, caller = "pn-self", "pn-caller"
	cases := []struct {
		name string
		d    Delegation
		want RejectReason
	}{
		{"ok", Delegation{SpawnDepth: 1, Chain: []string{caller}}, ""},
		{"loop A→B→A", Delegation{SpawnDepth: 2, Chain: []string{self, caller}}, RejectChain},
		{"depth exceeded", Delegation{SpawnDepth: 4, Chain: []string{caller}}, RejectDepth},
		{"forged tail", Delegation{SpawnDepth: 1, Chain: []string{"pn-someone-else"}}, RejectChain},
		{"too long", Delegation{SpawnDepth: 1, Chain: append(make([]string, maxChainLen), caller)}, RejectChain},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ValidateChain(c.d, self, caller, 3)
			got := RejectReason("")
			if r != nil {
				got = r.Reason
			}
			if got != c.want {
				t.Fatalf("want %q, got %q (%v)", c.want, got, r)
			}
		})
	}
	if d := NormalizeDelegation(nil, caller); d.SpawnDepth != 1 || len(d.Chain) != 1 || d.Chain[0] != caller {
		t.Fatalf("missing extension must normalize to depth=1 chain=[caller], got %+v", d)
	}
}

// ─── 委派服务 ────────────────────────────────────────────────────────────

func TestService_SubmitPostsIsolatedHighTaintTask(t *testing.T) {
	svc, bb := newTestService(t)
	task, err := svc.Submit(context.Background(), peer("pn-a"), SubmitRequest{Protocol: ProtocolA2A, Text: "summarize the logs"})
	if err != nil {
		t.Fatal(err)
	}
	if task.State != StateSubmitted || len(bb.posted) != 1 {
		t.Fatalf("expected one posted task, got state=%s posted=%d", task.State, len(bb.posted))
	}
	e := bb.posted[0]
	if e.Type != orchestrator.PeerDelegationTaskType || e.IntentTaint != types.TaintHigh ||
		e.Namespace != NamespacePrefix+task.ID || e.SpawnDepth != 1 || e.ID == task.ID {
		t.Fatalf("blackboard entry wrong: %+v", e)
	}
	if chain, ok := svc.UpstreamChain(context.Background(), e.Namespace); !ok || len(chain) != 1 || chain[0] != "pn-a" {
		t.Fatalf("upstream chain must be resolvable from the task namespace, got %v %v", chain, ok)
	}
}

func TestService_RejectsLoopAndRecordsIt(t *testing.T) {
	svc, bb := newTestService(t)
	self := svc.Self().NodeID
	task, err := svc.Submit(context.Background(), peer("pn-b"), SubmitRequest{Protocol: ProtocolMCP, Text: "x",
		Delegation: &Delegation{SpawnDepth: 2, Chain: []string{self, "pn-b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if task.State != StateRejected || len(bb.posted) != 0 || !strings.Contains(task.StatusMessage, "loop") {
		t.Fatalf("A→B→A must be rejected without posting, got %+v posted=%d", task, len(bb.posted))
	}
	got, err := svc.Get(context.Background(), peer("pn-b"), task.ID)
	if err != nil || got.State != StateRejected {
		t.Fatalf("rejection must be recorded for audit, got %+v %v", got, err)
	}
}

func TestService_OversizedInputRejected(t *testing.T) {
	svc, bb := newTestService(t)
	big := strings.Repeat("a", svc.Config().MaxInputBytes+1)
	task, err := svc.Submit(context.Background(), peer("pn-a"), SubmitRequest{Protocol: ProtocolA2A, Text: big})
	if err != nil || task.State != StateRejected || len(bb.posted) != 0 {
		t.Fatalf("oversized input must be rejected, got %+v %v", task, err)
	}
}

func TestService_IdempotentByMessageID(t *testing.T) {
	svc, bb := newTestService(t)
	ctx := context.Background()
	a, _ := svc.Submit(ctx, peer("pn-a"), SubmitRequest{Protocol: ProtocolA2A, MessageID: "m1", Text: "hi"})
	b, _ := svc.Submit(ctx, peer("pn-a"), SubmitRequest{Protocol: ProtocolA2A, MessageID: "m1", Text: "hi"})
	if a.ID != b.ID || len(bb.posted) != 1 {
		t.Fatalf("same messageId must not create a second task: %s vs %s, posted=%d", a.ID, b.ID, len(bb.posted))
	}
	c, _ := svc.Submit(ctx, peer("pn-other"), SubmitRequest{Protocol: ProtocolA2A, MessageID: "m1", Text: "hi"})
	if c.ID == a.ID {
		t.Fatal("messageId dedup must be scoped per caller")
	}
}

func TestService_CallerIsolationAndScopes(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	task, _ := svc.Submit(ctx, peer("pn-a"), SubmitRequest{Protocol: ProtocolA2A, Text: "hi"})
	if _, err := svc.Get(ctx, peer("pn-b"), task.ID); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("other callers must not see the task, got %v", err)
	}
	readOnly := Principal{PeerNodeID: "pn-a", Scopes: []string{ScopeRead}}
	if _, err := svc.Submit(ctx, readOnly, SubmitRequest{Text: "x"}); apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("send without a2a:send must be forbidden, got %v", err)
	}
	if _, err := svc.Cancel(ctx, readOnly, task.ID); apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("cancel without a2a:cancel must be forbidden, got %v", err)
	}
}

func TestService_StateMappingAndRetention(t *testing.T) {
	svc, bb := newTestService(t)
	ctx := context.Background()
	p := peer("pn-a")
	task, _ := svc.Submit(ctx, p, SubmitRequest{Protocol: ProtocolMCP, Text: "hi"})
	bbID := bb.posted[0].ID

	bb.set(bbID, types.TaskSuspended, "")
	got, _ := svc.Get(ctx, p, task.ID)
	if got.State != StateInputRequired || !strings.Contains(got.StatusMessage, "local human") {
		t.Fatalf("HITL suspension must surface as input_required with a local-only note, got %+v", got)
	}
	bb.set(bbID, types.TaskDone, "the answer")
	got, _ = svc.Get(ctx, p, task.ID)
	if got.State != StateCompleted || got.Result != "the answer" {
		t.Fatalf("completed result must be returned, got %+v", got)
	}
	// 黑板 24h 后清理终态任务：结果必须已经落在本表里。
	delete(bb.snaps, bbID)
	got, _ = svc.Get(ctx, p, task.ID)
	if got.State != StateCompleted || got.Result != "the answer" {
		t.Fatalf("result must outlive the blackboard row, got %+v", got)
	}
}

func TestService_Cancel(t *testing.T) {
	svc, bb := newTestService(t)
	ctx := context.Background()
	p := peer("pn-a")
	task, _ := svc.Submit(ctx, p, SubmitRequest{Protocol: ProtocolA2A, Text: "hi"})
	got, err := svc.Cancel(ctx, p, task.ID)
	if err != nil || got.State != StateCanceled || len(bb.canceled) != 1 {
		t.Fatalf("cancel must reach the blackboard and end canceled, got %+v %v", got, err)
	}
	if _, err := svc.Cancel(ctx, p, task.ID); apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("canceling a terminal task must conflict, got %v", err)
	}
}

func TestService_ListPaging(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	p := peer("pn-a")
	base := time.Now()
	for i := 0; i < 5; i++ {
		svc.now = func() time.Time { return base.Add(time.Duration(i) * time.Second) }
		if _, err := svc.Submit(ctx, p, SubmitRequest{Protocol: ProtocolA2A, ContextID: "ctx", Text: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = svc.Submit(ctx, peer("pn-b"), SubmitRequest{Protocol: ProtocolA2A, Text: "not yours"})
	page1, next, total, err := svc.List(ctx, p, ListFilter{PageSize: 3})
	if err != nil || len(page1) != 3 || total != 5 || next == "" {
		t.Fatalf("page1: n=%d total=%d next=%q err=%v", len(page1), total, next, err)
	}
	page2, next2, _, err := svc.List(ctx, p, ListFilter{PageSize: 3, PageToken: next})
	if err != nil || len(page2) != 2 || next2 != "" {
		t.Fatalf("page2: n=%d next=%q err=%v", len(page2), next2, err)
	}
	seen := map[string]bool{}
	for _, tk := range append(page1, page2...) {
		if seen[tk.ID] {
			t.Fatalf("duplicate across pages: %s", tk.ID)
		}
		seen[tk.ID] = true
	}
}

func TestService_PostRejectedByDepthGuardIsRejected(t *testing.T) {
	svc, bb := newTestService(t)
	bb.postErr = apperr.New(apperr.CodeForbidden, "blackboard.PostTask: SpawnDepth 4 exceeds max 3")
	task, err := svc.Submit(context.Background(), peer("pn-a"), SubmitRequest{Protocol: ProtocolA2A, Text: "x"})
	if err != nil || task.State != StateRejected {
		t.Fatalf("blackboard depth guard must surface as rejected, got %+v %v", task, err)
	}
}

func TestService_PeerProfileNarrowsOnly(t *testing.T) {
	svc, _ := newTestService(t)
	p := svc.PeerProfile(nil)
	if !p.ReadOnly || p.AllowDelegation {
		t.Fatalf("default peer profile must be read-only and non-delegating, got %+v", p)
	}
}

type captureAuditor struct{ recs []*security.AuditRecord }

func (c *captureAuditor) Record(r *security.AuditRecord) error {
	c.recs = append(c.recs, r)
	return nil
}

// TestService_AuditsAcceptAndReject HE-1：每次入站受理与拒绝都写审计，含对端节点、令牌与拒绝原因。
func TestService_AuditsAcceptAndReject(t *testing.T) {
	svc, _ := newTestService(t)
	aud := &captureAuditor{}
	svc.WithAuditor(aud)
	ctx := context.Background()
	_, _ = svc.Submit(ctx, peer("pn-a"), SubmitRequest{Protocol: ProtocolA2A, Text: "ok"})
	_, _ = svc.Submit(ctx, peer("pn-a"), SubmitRequest{Protocol: ProtocolA2A, Text: "x",
		Delegation: &Delegation{SpawnDepth: 9, Chain: []string{"pn-a"}}})
	if len(aud.recs) != 2 || aud.recs[0].Outcome != "allow" || aud.recs[1].Outcome != "deny" ||
		!strings.HasPrefix(aud.recs[1].DenyReason, "depth") || aud.recs[0].AgentID != "peer:pn-a" || aud.recs[0].CapTokenID == "" {
		t.Fatalf("unexpected audit records: %+v", aud.recs)
	}
}

func TestService_PeerProfileCarriesFileRoot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FileRoot = "/srv/peer-shared"
	svc := NewService(storerepo.NewSQLiteMeshRepository(newTestDB(t)), nil, nil, cfg)
	if p := svc.PeerProfile(nil); p.FileRoot != "/srv/peer-shared" || !p.ReadOnly || p.AllowDelegation {
		t.Fatalf("peer profile must be read-only, non-relaying and confined, got %+v", p)
	}
}
