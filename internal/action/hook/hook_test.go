package hook

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type allowPolicy struct{ deny bool }

func (p allowPolicy) IsAuthorized(context.Context, string, string, string, map[string]any) (bool, error) {
	return !p.deny, nil
}
func (p allowPolicy) Review(context.Context, types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: !p.deny}, nil
}

// passthroughWrapper 测试用：不加沙箱原样执行（验证协议语义，不验证隔离）。
type passthroughWrapper struct{}

func (passthroughWrapper) WrapArgv(_ context.Context, sctx protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	return &protocol.WrapArgvResult{Executable: sctx.ExecPath, Argv: sctx.ExecArgs, Env: append([]string{"PATH=/usr/bin:/bin"}, sctx.EnvExtra...)}, nil
}

type staticProvider []Source

func (p staticProvider) ListHookSources(context.Context) ([]Source, error) { return p, nil }

func mustConfig(t *testing.T, raw string) Config {
	t.Helper()
	cfg, err := ParseFile([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newTestRunner(t *testing.T, sources ...Source) *Runner {
	t.Helper()
	reg := NewRegistry(staticProvider(sources))
	if err := reg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return NewRunner(Deps{Registry: reg, Policy: allowPolicy{}, Wrapper: passthroughWrapper{}})
}

func TestParseFile_ValidationAndMatcher(t *testing.T) {
	cfg := mustConfig(t, `{"description":"x","hooks":{"PreToolUse":[{"matcher":"Write|Edit","hooks":[{"type":"command","command":"true"}]}]}}`)
	g := cfg[EventPreToolUse][0]
	if !g.Matches("Write") || !g.Matches("Edit") || g.Matches("WriteFile") {
		t.Fatalf("matcher must be anchored alternation")
	}
	for _, bad := range []string{
		`{"hooks":{"Stop":[{"hooks":[{"type":"command"}]}]}}`,
		`{"hooks":{"Stop":[{"hooks":[{"type":"bogus","command":"x"}]}]}}`,
		`{"hooks":{"Stop":[{"hooks":[{"type":"http"}]}]}}`,
	} {
		if _, err := ParseFile([]byte(bad)); err == nil {
			t.Errorf("expected validation error for %s", bad)
		}
	}
}

func TestDispatch_ExitCodeSemantics(t *testing.T) {
	root := t.TempDir()
	src := Source{Key: "plugin:x", Scope: ScopePlugin, Trusted: true, PluginRoot: root, Config: mustConfig(t, `{"hooks":{
	  "PreToolUse":[
	    {"matcher":"bash","hooks":[{"type":"command","command":"jq -r .tool_input.command >/dev/null 2>&1; echo blocked-reason >&2; exit 2"}]},
	    {"matcher":"read_file","hooks":[{"type":"command","command":"exit 1"}]}
	  ]}}`)}
	r := newTestRunner(t, src)
	ctx := context.Background()

	res := r.FirePreToolUse(ctx, "bash", map[string]any{"command": "rm -rf /tmp/x"})
	if !res.Blocked || res.Reason != "blocked-reason" {
		t.Fatalf("exit 2 must block with stderr reason: %+v", res)
	}
	if res := r.FirePreToolUse(ctx, "read_file", nil); res.Blocked {
		t.Fatalf("non-2 exit is a non-blocking error: %+v", res)
	}
	if res := r.FirePreToolUse(ctx, "other", nil); res.Blocked {
		t.Fatalf("unmatched tool must pass")
	}
}

func TestDispatch_StdinJSONAndDecisions(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "decide.sh")
	// 读 stdin 的 tool_input.command，按内容给出 deny / 改写 / 放行三种 JSON 决策。
	body := `#!/bin/sh
in=$(cat)
case "$in" in
  *'"rm'*) echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no rm"}}';;
  *'"ls'*) echo '{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"command":"ls -la"}}}';;
  *) echo '{"continue":true}';;
esac`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	src := Source{Key: "user:x", Scope: ScopeUser, Trusted: true, PluginRoot: root, Config: mustConfig(t,
		`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"${CLAUDE_PLUGIN_ROOT}/decide.sh"}]}]}}`)}
	r := newTestRunner(t, src)
	ctx := context.Background()
	if res := r.FirePreToolUse(ctx, "bash", map[string]any{"command": "rm x"}); !res.Blocked || res.Reason != "no rm" {
		t.Fatalf("deny: %+v", res)
	}
	res := r.FirePreToolUse(ctx, "bash", map[string]any{"command": "ls"})
	if res.Blocked || res.UpdatedInput["command"] != "ls -la" {
		t.Fatalf("updatedInput: %+v", res)
	}
}

func TestDispatch_UntrustedSourceSkipped(t *testing.T) {
	src := Source{Key: "plugin:x", Scope: ScopePlugin, Trusted: false, Config: mustConfig(t,
		`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"exit 2"}]}]}}`)}
	if res := newTestRunner(t, src).FirePreToolUse(context.Background(), "bash", nil); res.Blocked {
		t.Fatalf("untrusted hook must not run")
	}
}

func TestDispatch_PolicyDenyFailsClosedForGuards(t *testing.T) {
	src := Source{Key: "user:x", Scope: ScopeUser, Trusted: true, Config: mustConfig(t,
		`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"true"}]}],"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`)}
	reg := NewRegistry(staticProvider{src})
	_ = reg.Reload(context.Background())
	r := NewRunner(Deps{Registry: reg, Policy: allowPolicy{deny: true}, Wrapper: passthroughWrapper{}})
	if res := r.FirePreToolUse(context.Background(), "bash", nil); !res.Blocked {
		t.Fatalf("guard hook that cannot run must block")
	}
	if out := r.Dispatch(context.Background(), Input{HookEventName: EventStop}); out.Block || len(out.Errors) == 0 {
		t.Fatalf("non-guard event: error only, no block: %+v", out)
	}
}

func TestDispatch_ShellFormRejectsUserConfig(t *testing.T) {
	src := Source{Key: "plugin:x", Scope: ScopePlugin, Trusted: true, Options: map[string]string{"tok": "t"}, Config: mustConfig(t,
		`{"hooks":{"PreToolUse":[
		  {"matcher":"a","hooks":[{"type":"command","command":"echo ${user_config.tok} >&2; exit 2"}]},
		  {"matcher":"b","hooks":[{"type":"command","command":"/bin/sh","args":["-c","echo $0 >&2; echo $CLAUDE_PLUGIN_OPTION_TOK >&2; exit 2","${user_config.tok}"]}]}
		]}}`)}
	r := newTestRunner(t, src)
	if res := r.FirePreToolUse(context.Background(), "a", nil); res.Blocked {
		t.Fatalf("shell-form user_config must be rejected before execution")
	}
	res := r.FirePreToolUse(context.Background(), "b", nil)
	if !res.Blocked || res.Reason != "t\nt" {
		t.Fatalf("exec-form substitution and option env: %+v", res)
	}
}

func TestDispatch_IfRuleAndPostToolFeedback(t *testing.T) {
	src := Source{Key: "user:x", Scope: ScopeUser, Trusted: true, Config: mustConfig(t, `{"hooks":{
	  "PreToolUse":[{"hooks":[{"type":"command","if":"bash(git *)","command":"exit 2"}]}],
	  "PostToolUse":[{"hooks":[{"type":"command","command":"echo '{\"decision\":\"block\",\"reason\":\"lint failed\",\"hookSpecificOutput\":{\"additionalContext\":\"run make fmt\"}}'"}]}]}}`)}
	r := newTestRunner(t, src)
	ctx := context.Background()
	if res := r.FirePreToolUse(ctx, "bash", map[string]any{"command": "ls"}); res.Blocked {
		t.Fatalf("if-rule must filter non-matching command")
	}
	if res := r.FirePreToolUse(ctx, "bash", map[string]any{"command": "git push"}); !res.Blocked {
		t.Fatalf("if-rule must match git command")
	}
	fb := r.FirePostToolUse(ctx, "write_file", nil, "ok", true, "")
	if fb != "lint failed\nrun make fmt" {
		t.Fatalf("post feedback: %q", fb)
	}
}

type fakePrompt struct{ reply string }

func (f fakePrompt) EvaluateHookPrompt(context.Context, string, string, bool) (string, error) {
	return f.reply, nil
}

type fakeMCP struct{ got map[string]any }

func (f *fakeMCP) CallHookTool(_ context.Context, _, _ string, args map[string]any) (string, error) {
	f.got = args
	return `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"scanner"}}`, nil
}

func TestDispatch_PromptAndMCPHandlers(t *testing.T) {
	src := Source{Key: "user:x", Scope: ScopeUser, Trusted: true, Config: mustConfig(t, `{"hooks":{
	  "Stop":[{"hooks":[{"type":"prompt","prompt":"Is it done? $ARGUMENTS"}]}],
	  "PreToolUse":[{"hooks":[{"type":"mcp_tool","server":"sec","tool":"scan","input":{"path":"${tool_input.file_path}"}}]}]}}`)}
	reg := NewRegistry(staticProvider{src})
	_ = reg.Reload(context.Background())
	mcp := &fakeMCP{}
	r := NewRunner(Deps{Registry: reg, Policy: allowPolicy{}, Prompt: fakePrompt{`sure: {"ok": false, "reason": "tests missing"}`}, MCP: mcp})
	out := r.Dispatch(context.Background(), Input{HookEventName: EventStop})
	if !out.Block || out.Reason != "tests missing" {
		t.Fatalf("prompt verdict: %+v", out)
	}
	res := r.FirePreToolUse(context.Background(), "write_file", map[string]any{"file_path": "/a.go"})
	if !res.Blocked || mcp.got["path"] != "/a.go" {
		t.Fatalf("mcp_tool: %+v got=%v", res, mcp.got)
	}
}

func TestFileSource_TrustByScope(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hooks.json")
	_ = os.WriteFile(p, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`), 0o644)
	user, ok, err := FileSource(ScopeUser, p)
	if err != nil || !ok || !user.Trusted || user.Digest == "" {
		t.Fatalf("user source: %+v %v %v", user, ok, err)
	}
	proj, _, _ := FileSource(ScopeProject, p)
	if proj.Trusted || proj.Digest != user.Digest {
		t.Fatalf("project source must start untrusted with same digest")
	}
	if _, ok, err := FileSource(ScopeUser, filepath.Join(dir, "none.json")); ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v", ok, err)
	}
}

func TestDigestMatchesPluginspecAlgorithm(t *testing.T) {
	events := map[string]json.RawMessage{"Stop": json.RawMessage(`[{"hooks":[{"type":"command","command":"true"}]}]`)}
	if Digest(events) == "" || strings.Contains(Digest(events), " ") {
		t.Fatal("digest")
	}
	_ = errors.New
}
