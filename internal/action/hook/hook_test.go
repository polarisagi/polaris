package hook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	r := NewRunner(&Registry{groups: map[Event][]MatcherGroup{}})
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
	runner := NewRunner(reg)
	results := runner.Fire(context.Background(), HookInput{
		Event:     EventPostToolUse,
		ToolName:  "bash",
		SessionID: "test-session",
	})

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
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
	runner := NewRunner(reg)
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
	runner := NewRunner(reg)
	results := runner.Fire(context.Background(), HookInput{Event: EventSessionStart})
	if len(results) != 0 {
		t.Errorf("non-command handler should be skipped, got %d results", len(results))
	}
}
