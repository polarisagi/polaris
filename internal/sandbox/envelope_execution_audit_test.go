package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type envelopeAuditPolicy struct{}

func (envelopeAuditPolicy) IsAuthorized(context.Context, string, string, string, map[string]any) (bool, error) {
	return true, nil
}
func (envelopeAuditPolicy) Review(context.Context, types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

type envelopeExecutionAuditFake struct {
	beginErr      error
	beginID       string
	returnEmptyID bool
	completeErr   error
	beginCalls    int
	completeCalls int
	lastSuccess   bool
	lastErrorCode string
}

func (f *envelopeExecutionAuditFake) BeginExecution(context.Context, protocol.ExecutionAuditIntent) (string, error) {
	f.beginCalls++
	if f.beginErr != nil {
		return "", f.beginErr
	}
	if f.beginID != "" {
		return f.beginID, nil
	}
	if f.returnEmptyID {
		return "", nil
	}
	return "execution-test", nil
}
func (f *envelopeExecutionAuditFake) CompleteExecution(_ context.Context, _ string, success bool, errorCode string) error {
	f.completeCalls++
	f.lastSuccess = success
	f.lastErrorCode = errorCode
	return f.completeErr
}

func newAuditedEnvelope(t *testing.T, auditor protocol.ExecutionAuditor, fn InProcessFn) *ExecEnvelope {
	t.Helper()
	inproc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	inproc.Register("audit-tool", fn)
	router := NewSandboxRouter(inproc, nil, nil, "linux", 1)
	return NewExecEnvelope(envelopeAuditPolicy{}, router, 1, "linux", nil).WithExecutionAuditor(auditor)
}

func auditedSideEffectRequest() ExecRequest {
	return ExecRequest{
		Principal: PrincipalAgent, Kind: KindToolExecute, Resource: "audit-tool", TrustTier: types.TrustSystem,
		Tool:  types.Tool{Name: "audit-tool", Source: types.ToolBuiltin, SideEffects: []types.SideEffect{types.SideProcessSpawn}},
		Input: []byte(`{"x":1}`),
	}
}

func TestExecEnvelope_PhysicalExecutionRequiresIntentAndPreservesResultOnCompletionError(t *testing.T) {
	runs := 0
	fake := &envelopeExecutionAuditFake{completeErr: apperr.New(apperr.CodeStorageUnavailable, "offline")}
	envelope := newAuditedEnvelope(t, fake, func(context.Context, []byte) ([]byte, error) {
		runs++
		return []byte("done"), nil
	})
	res, err := envelope.Execute(context.Background(), auditedSideEffectRequest())
	if err != nil || res == nil || !res.Success || string(res.Output) != "done" {
		t.Fatalf("completion audit failure must preserve action result, result=%#v err=%v", res, err)
	}
	if runs != 1 || fake.beginCalls != 1 || fake.completeCalls != 1 {
		t.Fatalf("action/audit calls: runs=%d begin=%d complete=%d", runs, fake.beginCalls, fake.completeCalls)
	}
}

type recoveryAuditLogger struct {
	mu          sync.Mutex
	entries     []map[string]any
	failPhase   string
	failEnabled bool
}

func (l *recoveryAuditLogger) Log(_ context.Context, _ string, meta map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failEnabled && meta["phase"] == l.failPhase {
		return apperr.New(apperr.CodeStorageUnavailable, "audit sink unavailable")
	}
	copy := make(map[string]any, len(meta))
	for key, value := range meta {
		copy[key] = value
	}
	l.entries = append(l.entries, copy)
	return nil
}

func TestExecEnvelope_CompletionFailureBlocksThenFlushesBeforeNextAction(t *testing.T) {
	logger := &recoveryAuditLogger{failPhase: "completion", failEnabled: true}
	gate := security.NewExecutionAuditGate(logger)
	runs := 0
	envelope := newAuditedEnvelope(t, gate, func(context.Context, []byte) ([]byte, error) {
		runs++
		return []byte("done"), nil
	})
	if res, err := envelope.Execute(context.Background(), auditedSideEffectRequest()); err != nil || res == nil || !res.Success {
		t.Fatalf("first action result must survive completion-log failure: res=%#v err=%v", res, err)
	}
	if runs != 1 {
		t.Fatalf("first physical action must execute once, got %d", runs)
	}
	if _, err := envelope.Execute(context.Background(), auditedSideEffectRequest()); !apperr.IsCode(err, apperr.CodeStorageUnavailable) {
		t.Fatalf("pending completion must block another physical action, got %v", err)
	}
	if runs != 1 {
		t.Fatalf("blocked action must not run, got %d physical actions", runs)
	}
	logger.mu.Lock()
	logger.failEnabled = false
	logger.mu.Unlock()
	if res, err := envelope.Execute(context.Background(), auditedSideEffectRequest()); err != nil || res == nil || !res.Success {
		t.Fatalf("recovered gate must flush completion then permit new action: res=%#v err=%v", res, err)
	}
	if runs != 2 {
		t.Fatalf("only the first and recovered actions should run, got %d", runs)
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.entries) != 4 || logger.entries[0]["phase"] != "intent" || logger.entries[1]["phase"] != "completion" ||
		logger.entries[2]["phase"] != "intent" || logger.entries[3]["phase"] != "completion" {
		t.Fatalf("recovery must persist the pending completion before the next intent: %#v", logger.entries)
	}
}

func TestExecEnvelope_NilAuditorAndIntentFailureDoNotRunSideEffect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		auditor protocol.ExecutionAuditor
	}{
		{name: "nil auditor"},
		{name: "intent failure", auditor: &envelopeExecutionAuditFake{beginErr: apperr.New(apperr.CodeStorageUnavailable, "offline")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := 0
			envelope := newAuditedEnvelope(t, tc.auditor, func(context.Context, []byte) ([]byte, error) {
				runs++
				return nil, nil
			})
			if _, err := envelope.Execute(context.Background(), auditedSideEffectRequest()); err == nil {
				t.Fatal("physical execution must be refused without a persisted intent")
			}
			if runs != 0 {
				t.Fatalf("side effect ran before intent: runs=%d", runs)
			}
		})
	}
}

func TestExecEnvelope_EmptyIntentIDDoesNotRunSideEffect(t *testing.T) {
	runs := 0
	envelope := newAuditedEnvelope(t, &envelopeExecutionAuditFake{returnEmptyID: true}, func(context.Context, []byte) ([]byte, error) {
		runs++
		return []byte("ran"), nil
	})
	if _, err := envelope.Execute(context.Background(), auditedSideEffectRequest()); !apperr.IsCode(err, apperr.CodeStorageUnavailable) {
		t.Fatalf("empty execution intent ID must fail closed, got %v", err)
	}
	if runs != 0 {
		t.Fatalf("provider ran without a stable audit ID: %d", runs)
	}
}

func TestSameJSONSemanticsPreservesLargeIntegerChanges(t *testing.T) {
	equalPairs := [][2]string{
		{`1`, `1.0`},
		{`1`, `1e0`},
		{`-0`, `0`},
		{`{"b":[1, {"x":-0}],"a":1e1000000000}`, `{"a":10e999999999,"b":[1.0,{"x":0.0}]}`},
		{`9007199254740993`, `9007199254740993.0`},
	}
	for _, pair := range equalPairs {
		if !sameJSONSemantics([]byte(pair[0]), []byte(pair[1])) {
			t.Errorf("equivalent JSON values differ: %s vs %s", pair[0], pair[1])
		}
	}
	differentPairs := [][2]string{
		{`9007199254740992`, `9007199254740993`},
		{`1`, `true`},
		{`[1,2]`, `[2,1]`},
		{`{"n":1}`, `{"n":1}{"n":2}`},
		{`{"n":1}`, `{"n":1} garbage`},
	}
	for _, pair := range differentPairs {
		if sameJSONSemantics([]byte(pair[0]), []byte(pair[1])) {
			t.Errorf("different or invalid JSON values were considered equal: %s vs %s", pair[0], pair[1])
		}
	}
}

type postToolFeedbackFake struct{}

func (postToolFeedbackFake) FirePreToolUse(context.Context, string, map[string]any) PreToolUseResult {
	return PreToolUseResult{}
}
func (postToolFeedbackFake) FirePostToolUse(context.Context, string, map[string]any, string, bool, string) string {
	return "feedback"
}

func TestExecEnvelope_PostHookFeedbackNeverLowersReviewedTaint(t *testing.T) {
	inproc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	inproc.RegisterRich("audit-tool", func(context.Context, SandboxSpec) (*types.ToolResult, error) {
		return &types.ToolResult{Success: true, Output: []byte("done"), TaintLevel: types.TaintUserReviewed}, nil
	}, types.TaintNone)
	router := NewSandboxRouter(inproc, nil, nil, "linux", 1)
	envelope := NewExecEnvelope(envelopeAuditPolicy{}, router, 1, "linux", nil).WithExecutionAuditor(&envelopeExecutionAuditFake{})
	envelope.SetHookFirer(postToolFeedbackFake{})
	res, err := envelope.Execute(context.Background(), auditedSideEffectRequest())
	if err != nil || res == nil {
		t.Fatalf("execution failed: result=%#v err=%v", res, err)
	}
	if res.TaintLevel != types.TaintUserReviewed {
		t.Fatalf("hook feedback must only raise taint; got %v want %v", res.TaintLevel, types.TaintUserReviewed)
	}
}

func TestExecEnvelope_PanicCompletesIntentAsAborted(t *testing.T) {
	fake := &envelopeExecutionAuditFake{}
	envelope := newAuditedEnvelope(t, fake, func(context.Context, []byte) ([]byte, error) {
		panic("provider panic")
	})
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_, _ = envelope.Execute(context.Background(), auditedSideEffectRequest())
	}()
	if !panicked {
		t.Fatal("provider panic should continue propagating")
	}
	if fake.beginCalls != 1 || fake.completeCalls != 1 || fake.lastSuccess || fake.lastErrorCode != string(apperr.CodeInternal) {
		t.Fatalf("panic must complete pending intent without exposing panic value: %#v", fake)
	}
}

type sandboxExecutionProviderFake struct{ calls int }

func (p *sandboxExecutionProviderFake) Run(context.Context, SandboxSpec) (*types.ToolResult, error) {
	p.calls++
	return &types.ToolResult{Success: true}, nil
}

func TestExecEnvelope_UsesEffectiveCapabilityWhenMetadataOmitsCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool types.Tool
	}{
		{name: "MCP defaults to network write", tool: types.Tool{Name: "mcp-write", Source: types.ToolMCP, RiskLevel: types.RiskHigh}},
		{name: "skill defaults to local write", tool: types.Tool{Name: "skill-script", Source: types.ToolSkill, RiskLevel: types.RiskMedium}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inproc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
			inproc.Register(tc.tool.Name, func(context.Context, []byte) ([]byte, error) { return []byte("executed"), nil })
			wasm := &sandboxExecutionProviderFake{}
			router := NewSandboxRouter(inproc, nil, wasm, "linux", 1)
			fakeAudit := &envelopeExecutionAuditFake{}
			envelope := NewExecEnvelope(envelopeAuditPolicy{}, router, 1, "linux", nil).WithExecutionAuditor(fakeAudit)
			res, err := envelope.Execute(context.Background(), ExecRequest{
				Principal: PrincipalAgent, Kind: KindToolExecute, Resource: tc.tool.Name,
				Tool: tc.tool, TrustTier: types.TrustSystem, Input: []byte(`{}`),
			})
			if err == nil && (res == nil || res.Success || !strings.Contains(res.Error, "capability token")) {
				t.Fatal("default write capability must require a valid capability token")
			}
			if wasm.calls != 0 || fakeAudit.beginCalls != 0 {
				t.Fatalf("rejected call must not audit intent or reach provider: wasm=%d audits=%d", wasm.calls, fakeAudit.beginCalls)
			}
		})
	}
}

type envelopeHookFake struct{ result PreToolUseResult }

func (h envelopeHookFake) FirePreToolUse(context.Context, string, map[string]any) PreToolUseResult {
	return h.result
}
func (envelopeHookFake) FirePostToolUse(context.Context, string, map[string]any, string, bool, string) string {
	return ""
}

type envelopeCallAuthorizerFake struct {
	grant        protocol.PermissionGrant
	allow        bool
	seenGrant    protocol.PermissionGrant
	seenTaint    types.TaintLevel
	seenArgs     []byte
	callCount    int
	denialReason string
}

func (a *envelopeCallAuthorizerFake) AuthorizeCall(ctx context.Context, _ types.Tool, args []byte, taint types.TaintLevel) (protocol.PermissionGrant, string, bool) {
	a.callCount++
	a.seenGrant = protocol.PermissionGrantFrom(ctx)
	a.seenTaint = taint
	a.seenArgs = append([]byte(nil), args...)
	return a.grant, a.denialReason, a.allow
}

func hookRewriteEnvelope(input []byte, update map[string]any, authorizer protocol.ToolCallAuthorizer) (context.Context, ExecRequest, *ExecResult, error) {
	e := NewExecEnvelope(envelopeAuditPolicy{}, nil, 1, "linux", nil)
	e.SetHookFirer(envelopeHookFake{result: PreToolUseResult{UpdatedInput: update}})
	ctx := protocol.WithPermissionGrant(context.Background(), protocol.GrantExplicit)
	if authorizer != nil {
		ctx = protocol.WithToolCallAuthorizer(ctx, authorizer)
	}
	req := ExecRequest{Resource: "write_file", Tool: types.Tool{Name: "write_file", InputSchema: map[string]any{
		"type": "object", "required": []string{"path"},
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
	}}, Input: input}
	updatedCtx, denied, err := e.applyPreToolUse(ctx, &req, time.Now())
	return updatedCtx, req, denied, err
}

func TestExecEnvelope_HookRewriteReauthorizesWithHighTaintAndReplacesGrant(t *testing.T) {
	authorizer := &envelopeCallAuthorizerFake{grant: protocol.GrantAuto, allow: true}
	ctx, req, denied, err := hookRewriteEnvelope([]byte(`{"path":"a.txt"}`), map[string]any{"path": "b.txt"}, authorizer)
	if err != nil || denied != nil {
		t.Fatalf("authorized rewrite unexpectedly denied: result=%#v err=%v", denied, err)
	}
	if authorizer.callCount != 1 || authorizer.seenGrant != protocol.GrantNone {
		t.Fatalf("must reauthorize with old grant cleared: calls=%d seen=%v", authorizer.callCount, authorizer.seenGrant)
	}
	if authorizer.seenTaint != types.TaintHigh || req.TaintLevel != types.TaintHigh {
		t.Fatalf("hook rewrite must be raised to High: authorized=%v request=%v", authorizer.seenTaint, req.TaintLevel)
	}
	if protocol.PermissionGrantFrom(ctx) != protocol.GrantAuto || string(req.Input) != `{"path":"b.txt"}` {
		t.Fatalf("updated input must carry only its new grant: grant=%v input=%s", protocol.PermissionGrantFrom(ctx), req.Input)
	}
}

func TestExecEnvelope_HookSemanticNoopDoesNotReauthorize(t *testing.T) {
	authorizer := &envelopeCallAuthorizerFake{grant: protocol.GrantAuto, allow: true}
	ctx, req, denied, err := hookRewriteEnvelope([]byte(`{"path":"a.txt"}`), map[string]any{"path": "a.txt"}, authorizer)
	if err != nil || denied != nil || authorizer.callCount != 0 {
		t.Fatalf("semantic no-op should not reauthorize: calls=%d result=%#v err=%v", authorizer.callCount, denied, err)
	}
	if protocol.PermissionGrantFrom(ctx) != protocol.GrantExplicit || string(req.Input) != `{"path":"a.txt"}` {
		t.Fatalf("semantic no-op must preserve original authorization and JSON: grant=%v input=%s", protocol.PermissionGrantFrom(ctx), req.Input)
	}
}

func TestExecEnvelope_HookNumericLexicalNoopPreservesInputAndAuthorization(t *testing.T) {
	runs := 0
	inproc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	inproc.Register("numeric-tool", func(_ context.Context, input []byte) ([]byte, error) {
		runs++
		return append([]byte(nil), input...), nil
	})
	envelope := NewExecEnvelope(envelopeAuditPolicy{}, NewSandboxRouter(inproc, nil, nil, "linux", 1), 1, "linux", nil)
	envelope.SetHookFirer(envelopeHookFake{result: PreToolUseResult{UpdatedInput: map[string]any{"n": json.Number("1.0")}}})
	authorizer := &envelopeCallAuthorizerFake{grant: protocol.GrantAuto, allow: true}
	ctx := protocol.WithToolCallAuthorizer(protocol.WithPermissionGrant(context.Background(), protocol.GrantExplicit), authorizer)
	original := []byte(`{"n":1}`)
	req := ExecRequest{Kind: KindToolExecute, Resource: "numeric-tool", Tool: types.Tool{Name: "numeric-tool", Source: types.ToolBuiltin,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "number"}}}}, Input: original}
	result, err := envelope.Execute(ctx, req)
	if err != nil || result == nil || !result.Success {
		t.Fatalf("numeric lexical no-op should execute: result=%#v err=%v", result, err)
	}
	if authorizer.callCount != 0 || runs != 1 || string(req.Input) != string(original) || string(result.Output) != string(original) {
		t.Fatalf("numeric lexical no-op changed authorization, input, or execution: calls=%d runs=%d request=%s output=%s", authorizer.callCount, runs, req.Input, result.Output)
	}
}

func TestExecEnvelope_HookNumericChangeDenyBlocksExecution(t *testing.T) {
	runs := 0
	inproc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	inproc.Register("numeric-tool", func(context.Context, []byte) ([]byte, error) {
		runs++
		return []byte("ran"), nil
	})
	envelope := NewExecEnvelope(envelopeAuditPolicy{}, NewSandboxRouter(inproc, nil, nil, "linux", 1), 1, "linux", nil)
	envelope.SetHookFirer(envelopeHookFake{result: PreToolUseResult{UpdatedInput: map[string]any{"n": json.Number("2")}}})
	authorizer := &envelopeCallAuthorizerFake{allow: false, denialReason: "approval required"}
	ctx := protocol.WithToolCallAuthorizer(protocol.WithPermissionGrant(context.Background(), protocol.GrantExplicit), authorizer)
	req := ExecRequest{Kind: KindToolExecute, Resource: "numeric-tool", Tool: types.Tool{Name: "numeric-tool", Source: types.ToolBuiltin,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "number"}}}}, Input: []byte(`{"n":1}`)}
	result, err := envelope.Execute(ctx, req)
	if err != nil || result == nil || result.Success || authorizer.callCount != 1 || runs != 0 {
		t.Fatalf("numeric change must be denied before execution: result=%#v err=%v calls=%d runs=%d", result, err, authorizer.callCount, runs)
	}
}

func TestExecEnvelope_HookRewriteRequiresAuthorizerAndValidSchema(t *testing.T) {
	if _, _, denied, err := hookRewriteEnvelope([]byte(`{"path":"a.txt"}`), map[string]any{"path": "b.txt"}, nil); err != nil || denied == nil {
		t.Fatalf("missing authorizer must deny changed input: result=%#v err=%v", denied, err)
	}
	authorizer := &envelopeCallAuthorizerFake{grant: protocol.GrantAuto, allow: true}
	_, _, denied, err := hookRewriteEnvelope([]byte(`{"path":"a.txt"}`), map[string]any{"path": 42}, authorizer)
	if err != nil || denied == nil || authorizer.callCount != 0 {
		t.Fatalf("schema-invalid hook input must be rejected before authorization: calls=%d result=%#v err=%v", authorizer.callCount, denied, err)
	}
	authorizer.allow = false
	authorizer.denialReason = "approval required"
	_, _, denied, err = hookRewriteEnvelope([]byte(`{"path":"a.txt"}`), map[string]any{"path": "b.txt"}, authorizer)
	if err != nil || denied == nil {
		t.Fatalf("Ask/Deny must reject changed input: result=%#v err=%v", denied, err)
	}
}
