package hook

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockPolicyGate is a test stub satisfying protocol.PolicyGate.
type mockPolicyGate struct {
	allowed bool
}

func (m *mockPolicyGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return m.allowed, nil
}
func (m *mockPolicyGate) Review(_ context.Context, _ types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: m.allowed}, nil
}

var _ protocol.PolicyGate = (*mockPolicyGate)(nil)

// echoRunner 是一个测试用 CmdRunner 实现：通过 bash 裸执行（无沙箱隔离）。
// 仅用于 hook Runner 单元测试（验证事件匹配/并发/错误处理逻辑），不用于安全测试。
// 生产环境走 ExecEnvelope → SandboxRouter → ContainerSandbox/NativeOSSandbox → 真实
// WrapBashCmdRunner（Rust bwrap/Seatbelt 统一沙箱）。
type echoRunner struct{}

func (echoRunner) RunCmd(_ context.Context, cfg sandbox.CmdRunnerCfg) ([]byte, int, string, error) {
	// 直接用 bash 裸执行，仅用于单元测试——绕过沙箱 profile 兼容性问题
	// （macOS seatbelt deny-default 在不同版本上行为差异较大，不适合在此测试）。
	cmd := exec.Command("bash", "-c", cfg.Command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out, ee.ExitCode(), "test_bare", nil
		}
		return nil, -1, "test_bare", err
	}
	return out, 0, "test_bare", nil
}

// allowAllPolicyGate 测试用 PolicyGate：始终 allow，仅验证 Runner 自身逻辑。
type allowAllPolicyGate struct{}

func (allowAllPolicyGate) IsAuthorized(context.Context, string, string, string, map[string]any) (bool, error) {
	return true, nil
}

func (allowAllPolicyGate) Review(context.Context, types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{}, nil
}

// newTestEnvelope 构造一个真实 ExecEnvelope，底层用 echoRunner 裸执行（无沙箱隔离，
// 仅用于测试 Runner 的匹配/并发/错误处理逻辑，不测试沙箱隔离本身）。
// SandboxRouter 收到 SideProcessSpawn 会路由到 Container tier；Tier=0（测试传参）时
// AssignSandboxTier 会进一步降级到 NativeOS，故同时注入 container 和 nativeOS 两个 provider。
func newTestEnvelope(t *testing.T) *sandbox.ExecEnvelope {
	t.Helper()
	containerSbx := sandbox.NewContainerSandbox("", "linux", 0, echoRunner{})
	nativeOSSbx := sandbox.NewNativeOSSandbox(echoRunner{})
	router := sandbox.NewSandboxRouter(sandbox.NewInProcessSandbox(), containerSbx, nil, "linux", 0)
	router.WithNativeOS(nativeOSSbx)
	return sandbox.NewExecEnvelope(allowAllPolicyGate{}, router, 0, "linux", nil)
}

// ── Registry ──────────────────────────────────────────────────────────────────

func TestLoad_NonExistentPathsOK(t *testing.T) {
	r, err := Load("/nonexistent/path/hooks.yaml")
	if err != nil {
		t.Fatalf("Load with missing file should not error: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil Registry")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "hooks.yaml")
	os.WriteFile(p, []byte("{invalid yaml:::"), 0o644)

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoad_ValidConfig(t *testing.T) {
	yaml := `
hooks:
  PreToolUse:
    - matcher: "bash"
      hooks:
        - type: command
          command: "echo pre"
  Stop:
    - matcher: ""
      hooks:
        - type: command
          command: "echo stop"
`
	tmp := t.TempDir()
	p := filepath.Join(tmp, "hooks.yaml")
	os.WriteFile(p, []byte(yaml), 0o644)

	r, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	matched := r.Match(EventPreToolUse, "bash")
	if len(matched) != 1 {
		t.Fatalf("expected 1 match for bash, got %d", len(matched))
	}
	if matched[0].Hooks[0].Timeout != 30*time.Second {
		t.Errorf("expected default timeout 30s, got %v", matched[0].Hooks[0].Timeout)
	}
}

func TestMatch_EmptyMatcher_MatchesAll(t *testing.T) {
	r := &Registry{
		groups: map[Event][]MatcherGroup{
			EventStop: {{Matcher: "", Hooks: []HandlerConfig{{Type: "command", Command: "echo stop"}}}},
		},
	}

	if got := r.Match(EventStop, ""); len(got) != 1 {
		t.Errorf("empty matcher should match all, got %d", len(got))
	}
	if got := r.Match(EventStop, "any_tool"); len(got) != 1 {
		t.Errorf("empty matcher should match any tool, got %d", len(got))
	}
}

func TestMatch_RegexMatcher(t *testing.T) {
	r := &Registry{
		groups: map[Event][]MatcherGroup{
			EventPreToolUse: compileMatchers([]MatcherGroup{
				{Matcher: "^bash.*", Hooks: []HandlerConfig{{Type: "command", Command: "echo"}}},
			}),
		},
	}

	if got := r.Match(EventPreToolUse, "bash"); len(got) != 1 {
		t.Errorf("regex ^bash.* should match 'bash', got %d", len(got))
	}
	if got := r.Match(EventPreToolUse, "python"); len(got) != 0 {
		t.Errorf("regex ^bash.* should not match 'python', got %d", len(got))
	}
}

func TestMatch_NoMatchingEvent(t *testing.T) {
	r := &Registry{groups: map[Event][]MatcherGroup{}}
	if got := r.Match(EventSessionStart, ""); got != nil {
		t.Errorf("expected nil for unregistered event, got %v", got)
	}
}

func TestApplyDefaults_SetsTimeout(t *testing.T) {
	groups := []MatcherGroup{
		{Hooks: []HandlerConfig{{Type: "command", Timeout: 0}}},
		{Hooks: []HandlerConfig{{Type: "command", Timeout: 5 * time.Second}}},
	}
	out := applyDefaults(groups)
	if out[0].Hooks[0].Timeout != 30*time.Second {
		t.Errorf("zero timeout should be set to 30s, got %v", out[0].Hooks[0].Timeout)
	}
	if out[1].Hooks[0].Timeout != 5*time.Second {
		t.Errorf("explicit timeout should not be overridden, got %v", out[1].Hooks[0].Timeout)
	}
}

// ── Runner ────────────────────────────────────────────────────────────────────

func TestRunner_Fire_NoGroups(t *testing.T) {
	r := NewRunner(&Registry{groups: map[Event][]MatcherGroup{}}, &mockPolicyGate{allowed: true}, newTestEnvelope(t), nil, nil)
	results := r.Fire(context.Background(), HookInput{Event: EventStop})
	if results != nil {
		t.Errorf("expected nil results for unregistered event, got %v", results)
	}
}

func TestRunner_Fire_EchoCommand(t *testing.T) {
	reg := &Registry{
		groups: map[Event][]MatcherGroup{
			EventPostToolUse: compileMatchers([]MatcherGroup{{
				Matcher: "",
				Hooks: []HandlerConfig{{
					Type:    "command",
					Command: "echo hello-hook",
					Timeout: 5 * time.Second,
				}},
			}}),
		},
	}
	runner := NewRunner(reg, &mockPolicyGate{allowed: true}, newTestEnvelope(t), nil, nil)
	results := runner.Fire(context.Background(), HookInput{
		Event:     EventPostToolUse,
		ToolName:  "bash",
		SessionID: "test-session",
	})

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("unexpected error: %v", results[0].Err)
	}
	if results[0].ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", results[0].ExitCode)
	}
	if !strings.Contains(results[0].Stdout, "hello-hook") {
		t.Errorf("expected stdout to contain 'hello-hook', got %q", results[0].Stdout)
	}
}

func TestRunner_Fire_NonZeroExit(t *testing.T) {
	reg := &Registry{
		groups: map[Event][]MatcherGroup{
			EventPreToolUse: compileMatchers([]MatcherGroup{{
				Hooks: []HandlerConfig{{
					Type:    "command",
					Command: "exit 42",
					Timeout: 5 * time.Second,
				}},
			}}),
		},
	}
	runner := NewRunner(reg, &mockPolicyGate{allowed: true}, newTestEnvelope(t), nil, nil)
	results := runner.Fire(context.Background(), HookInput{Event: EventPreToolUse})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
}

func TestRunner_Fire_SkipsNonCommandType(t *testing.T) {
	reg := &Registry{
		groups: map[Event][]MatcherGroup{
			EventSessionStart: {{
				Hooks: []HandlerConfig{{Type: "webhook", Command: "http://example.com"}},
			}},
		},
	}
	runner := NewRunner(reg, &mockPolicyGate{allowed: true}, newTestEnvelope(t), nil, nil)
	results := runner.Fire(context.Background(), HookInput{Event: EventSessionStart})
	if len(results) != 0 {
		t.Errorf("non-command handler should be skipped, got %d results", len(results))
	}
}

// TestRunner_Fire_NilEnvelope_FailClosed 验证 envelope==nil 时 Runner fail-closed：
// 不裸跑，直接返回 Forbidden 错误（HE-Rule 2）。
func TestRunner_Fire_NilEnvelope_FailClosed(t *testing.T) {
	reg := &Registry{
		groups: map[Event][]MatcherGroup{
			EventPostToolUse: compileMatchers([]MatcherGroup{{
				Matcher: "",
				Hooks: []HandlerConfig{{
					Type:    "command",
					Command: "echo should-not-run",
					Timeout: 5 * time.Second,
				}},
			}}),
		},
	}
	runner := NewRunner(reg, &mockPolicyGate{allowed: true}, nil, nil, nil)
	results := runner.Fire(context.Background(), HookInput{Event: EventPostToolUse})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("expected fail-closed error when envelope is nil")
	}
	if !strings.Contains(results[0].Err.Error(), "fail-closed") {
		t.Errorf("expected fail-closed in error message, got: %v", results[0].Err)
	}
}

// ── HookFirer（sandbox.HookFirer 接口实现）───────────────────────────────────

func TestRunner_FirePreToolUse_BlocksOnNonZeroExit(t *testing.T) {
	reg := &Registry{
		groups: map[Event][]MatcherGroup{
			EventPreToolUse: compileMatchers([]MatcherGroup{{
				Hooks: []HandlerConfig{{Type: "command", Command: "echo blocked-reason; exit 1", Timeout: 5 * time.Second}},
			}}),
		},
	}
	runner := NewRunner(reg, &mockPolicyGate{allowed: true}, newTestEnvelope(t), nil, nil)
	blocked, reason := runner.FirePreToolUse(context.Background(), "bash", nil, "sess-1")
	if !blocked {
		t.Fatal("expected blocked=true when hook exits non-zero")
	}
	if !strings.Contains(reason, "blocked-reason") {
		t.Errorf("expected reason to contain hook stdout, got %q", reason)
	}
}

func TestRunner_FirePreToolUse_AllowsOnZeroExit(t *testing.T) {
	reg := &Registry{
		groups: map[Event][]MatcherGroup{
			EventPreToolUse: compileMatchers([]MatcherGroup{{
				Hooks: []HandlerConfig{{Type: "command", Command: "echo ok", Timeout: 5 * time.Second}},
			}}),
		},
	}
	runner := NewRunner(reg, &mockPolicyGate{allowed: true}, newTestEnvelope(t), nil, nil)
	blocked, _ := runner.FirePreToolUse(context.Background(), "bash", nil, "sess-1")
	if blocked {
		t.Fatal("expected blocked=false when hook exits 0")
	}
}

func TestRunner_FirePostToolUse_NoPanic(t *testing.T) {
	reg := &Registry{
		groups: map[Event][]MatcherGroup{
			EventPostToolUse: compileMatchers([]MatcherGroup{{
				Hooks: []HandlerConfig{{Type: "command", Command: "echo done", Timeout: 5 * time.Second}},
			}}),
		},
	}
	runner := NewRunner(reg, &mockPolicyGate{allowed: true}, newTestEnvelope(t), nil, nil)
	runner.FirePostToolUse(context.Background(), "bash", nil, "tool output", "sess-1")
}
