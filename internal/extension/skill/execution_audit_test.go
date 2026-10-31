package skill

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeSkillExecutionAuditor struct {
	beginErr      error
	emptyID       bool
	completeErr   error
	beginCalls    int
	completeCalls int
	lastSuccess   bool
}

func (f *fakeSkillExecutionAuditor) BeginExecution(context.Context, protocol.ExecutionAuditIntent) (string, error) {
	f.beginCalls++
	if f.beginErr != nil {
		return "", f.beginErr
	}
	if f.emptyID {
		return "", nil
	}
	return "skill-execution", nil
}

func (f *fakeSkillExecutionAuditor) CompleteExecution(_ context.Context, _ string, success bool, _ string) error {
	f.completeCalls++
	f.lastSuccess = success
	return f.completeErr
}

func TestScriptSkillExecutor_AuditFailureDoesNotRunOrRetryAction(t *testing.T) {
	reg := newTestSQLiteRegistry(t)
	ctx := context.Background()
	if err := reg.Register(ctx, types.SkillMeta{Name: "skill:audit", Version: "1", Trust: types.TrustLocal, ScriptPath: "/script.py"}); err != nil {
		t.Fatal(err)
	}
	runner := &countingSkillRunner{response: []byte("result")}
	beginFailure := &fakeSkillExecutionAuditor{beginErr: apperr.New(apperr.CodeStorageUnavailable, "offline")}
	exec := NewScriptSkillExecutor(reg, runner, nil).WithPolicy(allowAllPolicyGate{}).WithExecutionAuditor(beginFailure)
	if _, err := exec.ExecuteSkill(ctx, "skill:audit", []byte("args")); err == nil || runner.calls != 0 {
		t.Fatalf("failed intent must block physical runner: err=%v calls=%d", err, runner.calls)
	}

	completeFailure := &fakeSkillExecutionAuditor{completeErr: apperr.New(apperr.CodeStorageUnavailable, "offline")}
	exec = NewScriptSkillExecutor(reg, runner, nil).WithPolicy(allowAllPolicyGate{}).WithExecutionAuditor(completeFailure)
	out, err := exec.ExecuteSkill(ctx, "skill:audit", []byte("different args"))
	if err != nil || string(out) != "result" || runner.calls != 1 || completeFailure.completeCalls != 1 {
		t.Fatalf("completion audit failure must preserve the one real result: out=%q err=%v runs=%d completes=%d", out, err, runner.calls, completeFailure.completeCalls)
	}
}

func TestScriptSkillExecutor_EmptyIntentIDFailsClosed(t *testing.T) {
	reg := newTestSQLiteRegistry(t)
	ctx := context.Background()
	if err := reg.Register(ctx, types.SkillMeta{Name: "skill:empty-audit", Version: "1", Trust: types.TrustLocal, ScriptPath: "/script.py"}); err != nil {
		t.Fatal(err)
	}
	runner := &countingSkillRunner{response: []byte("must not run")}
	exec := NewScriptSkillExecutor(reg, runner, nil).WithPolicy(allowAllPolicyGate{}).WithExecutionAuditor(&fakeSkillExecutionAuditor{emptyID: true})
	if _, err := exec.ExecuteSkill(ctx, "skill:empty-audit", nil); !apperr.IsCode(err, apperr.CodeStorageUnavailable) {
		t.Fatalf("empty intent ID must fail closed, got %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("script ran without a stable audit ID: %d", runner.calls)
	}
}

type countingSkillRunner struct {
	calls    int
	response []byte
}

func (r *countingSkillRunner) RunScript(context.Context, string, string, []byte, types.TrustTier) ([]byte, error) {
	r.calls++
	return r.response, nil
}

func TestSkillInjection_NilAuditorFailsClosed(t *testing.T) {
	reg, digest := registerInjectedSkill(t, "x !`echo hi`")
	exec := NewScriptSkillExecutor(reg, nil, nil).WithInjector(&SkillInjector{
		Wrapper: passthroughWrapper{}, Policy: allowGate{},
		Trust: mapTrust{InjectionTrustKey("skill:inj"): digest},
	})
	if _, err := exec.ExecuteSkill(context.Background(), "skill:inj", nil); !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("dynamic command must fail closed without auditor, got %v", err)
	}
}

func TestSkillInjection_EmptyIntentIDFailsClosed(t *testing.T) {
	reg, digest := registerInjectedSkill(t, "x !`echo hi`")
	wrapper := &countingStdioWrapper{}
	exec := NewScriptSkillExecutor(reg, nil, nil).WithInjector(&SkillInjector{
		Wrapper: wrapper, Policy: allowGate{}, Auditor: &fakeSkillExecutionAuditor{emptyID: true},
		Trust: mapTrust{InjectionTrustKey("skill:inj"): digest},
	})
	if _, err := exec.ExecuteSkill(context.Background(), "skill:inj", nil); !apperr.IsCode(err, apperr.CodeStorageUnavailable) {
		t.Fatalf("empty injection intent ID must fail closed, got %v", err)
	}
	if wrapper.calls != 0 {
		t.Fatalf("injection command ran without a stable audit ID: %d", wrapper.calls)
	}
}

type countingStdioWrapper struct{ calls int }

func (w *countingStdioWrapper) WrapArgv(ctx context.Context, sctx protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	w.calls++
	return passthroughWrapper{}.WrapArgv(ctx, sctx)
}
