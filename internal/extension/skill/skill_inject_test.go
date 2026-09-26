package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type passthroughWrapper struct{}

func (passthroughWrapper) WrapArgv(_ context.Context, sctx protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	return &protocol.WrapArgvResult{Executable: sctx.ExecPath, Argv: sctx.ExecArgs, Env: []string{"PATH=/usr/bin:/bin"}}, nil
}

type mapTrust map[string]string

func (m mapTrust) ListHookTrust(context.Context) (map[string]string, error) { return m, nil }

type allowGate struct{}

func (allowGate) IsAuthorized(context.Context, string, string, string, map[string]any) (bool, error) {
	return true, nil
}
func (allowGate) Review(context.Context, types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

const injectedSkill = "Args: $0\nOut: !`echo $0`\nNone: !`grep nomatch /dev/null`\n```!\necho block\n```\nlast"

func registerInjectedSkill(t *testing.T, instructions string) (*SQLiteRegistryImpl, string) {
	t.Helper()
	reg := newTestSQLiteRegistry(t)
	if err := reg.Register(context.Background(), types.SkillMeta{Name: "skill:inj", Version: "1", Trust: types.TrustLocal, Instructions: instructions}); err != nil {
		t.Fatal(err)
	}
	return reg, pluginspec.InjectionDigest(pluginspec.FindInjections(instructions))
}

func TestSkillInjection_TrustedExecution(t *testing.T) {
	reg, digest := registerInjectedSkill(t, injectedSkill)
	exec := NewScriptSkillExecutor(reg, nil, nil).WithInjector(&SkillInjector{Wrapper: passthroughWrapper{}, Policy: allowGate{},
		Trust: mapTrust{InjectionTrustKey("skill:inj"): digest}})
	out, err := exec.ExecuteSkill(context.Background(), "skill:inj", []byte(`{"arguments":"'$(id)' x"}`))
	if err != nil {
		t.Fatal(err)
	}
	// 参数以单引号转义进入命令：$(id) 不会被执行；正文中的 $0 按字面参数渲染。
	want := "Args: $(id)\nOut: $(id)\nNone: \nblock\nlast"
	if string(out) != want {
		t.Fatalf("got  %q\nwant %q", out, want)
	}
}

func TestSkillInjection_PendingReviewAndDisabled(t *testing.T) {
	reg, _ := registerInjectedSkill(t, injectedSkill)
	exec := NewScriptSkillExecutor(reg, nil, nil).WithInjector(&SkillInjector{Wrapper: passthroughWrapper{}, Policy: allowGate{}, Trust: mapTrust{}})
	if _, err := exec.ExecuteSkill(context.Background(), "skill:inj", nil); !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("untrusted injections must be refused, got %v", err)
	}
	out, err := NewScriptSkillExecutor(reg, nil, nil).ExecuteSkill(context.Background(), "skill:inj", nil)
	if err != nil || !strings.Contains(string(out), injectionDisabledText) || strings.Contains(string(out), "block\n") {
		t.Fatalf("without injector commands must not run: %q %v", out, err)
	}
}

func TestSkillInjection_FailureAbortsInvocation(t *testing.T) {
	reg, digest := registerInjectedSkill(t, "x !`exit 3`")
	exec := NewScriptSkillExecutor(reg, nil, nil).WithInjector(&SkillInjector{Wrapper: passthroughWrapper{}, Policy: allowGate{},
		Trust: mapTrust{InjectionTrustKey("skill:inj"): digest}})
	if _, err := exec.ExecuteSkill(context.Background(), "skill:inj", nil); err == nil || !strings.Contains(err.Error(), "Shell command failed") {
		t.Fatalf("failing command must abort, got %v", err)
	}
}
