package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakePluginCtx struct{}

func (fakePluginCtx) ResolveSkillRenderContext(context.Context, string) (pluginspec.RenderInput, error) {
	return pluginspec.RenderInput{
		PluginRoot: "/plugins/deploy", PluginData: "/data/deploy",
		UserConfig: map[string]string{"region": "eu"}, SensitiveKeys: map[string]bool{"token": true},
	}, nil
}

func TestScriptSkillExecutor_RendersInstructionSkill(t *testing.T) {
	reg := newTestSQLiteRegistry(t)
	ctx := context.Background()
	meta := types.SkillMeta{
		Name: "skill:deploy__ship", Version: "1", Trust: types.TrustLocal, PluginID: "pl_1",
		SkillDir: "/plugins/deploy/skills/ship", DisplayName: "deploy:ship", Kind: "skill",
		Spec:         `{"arguments":["env"]}`,
		Instructions: "Ship to $env in ${user_config.region} with ${user_config.token}; run ${CLAUDE_SKILL_DIR}/scripts/go.sh from ${CLAUDE_PLUGIN_ROOT}. Session ${CLAUDE_SESSION_ID}.",
	}
	if err := reg.Register(ctx, meta); err != nil {
		t.Fatal(err)
	}
	exec := NewScriptSkillExecutor(reg, nil, nil).WithPluginContext(fakePluginCtx{})
	ctx = context.WithValue(ctx, protocol.CtxSessionIDKey{}, "s-1")
	out, err := exec.ExecuteSkill(ctx, "skill:deploy__ship", []byte(`{"arguments":"prod"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "Base directory for this skill: /plugins/deploy/skills/ship\n\n" +
		"Ship to prod in eu with <sensitive:token>; run /plugins/deploy/skills/ship/scripts/go.sh from /plugins/deploy. Session s-1."
	if string(out) != want {
		t.Fatalf("got  %q\nwant %q", out, want)
	}
}

func TestSQLiteRegistry_RoundTripsStandardSkillFields(t *testing.T) {
	reg := newTestSQLiteRegistry(t)
	ctx := context.Background()
	in := types.SkillMeta{
		Name: "skill:a", Version: "1", Trust: types.TrustLocal, Description: "d", DisplayName: "p:a", Kind: "command",
		DisableModelInvocation: true, SkillDir: "/s", ScriptPath: "/s/src/index.ts", Spec: `{"argument_hint":"[x]"}`,
	}
	if err := reg.Register(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Get(ctx, "skill:a", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "d" || got.DisplayName != "p:a" || got.Kind != "command" || !got.DisableModelInvocation ||
		got.DisableUserInvocation || got.SkillDir != "/s" || got.ScriptPath != "/s/src/index.ts" || !strings.Contains(got.Spec, "[x]") {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
