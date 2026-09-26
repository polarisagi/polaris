package hook

import (
	"context"
	"encoding/json"
	"testing"
)

// TestFireElicitation_AcceptWithContent 验证 hook 可程序化作答 elicitation/create，
// 跳过用户对话框（claude_hooks_elicitation.md §Elicitation output）。
func TestFireElicitation_AcceptWithContent(t *testing.T) {
	src := Source{Key: "user:x", Scope: ScopeUser, Trusted: true, Config: mustConfig(t, `{"hooks":{
	  "Elicitation":[{"matcher":"srv","hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"hookEventName\":\"Elicitation\",\"action\":\"accept\",\"content\":{\"username\":\"alice\"}}}'"}]}]}}`)}
	r := newTestRunner(t, src)
	schema := json.RawMessage(`{"type":"object","properties":{"username":{"type":"string"}}}`)

	res, decided := r.FireElicitation(context.Background(), "srv", "form", "please provide credentials", "", "", schema)
	if !decided {
		t.Fatal("expected the hook to decide")
	}
	if res.Action != "accept" || res.Content["username"] != "alice" {
		t.Fatalf("unexpected decision: %+v", res)
	}
}

// TestFireElicitation_ExitTwoDeclines exit 2 拒绝该次 elicitation，hookSpecificOutput
// 被忽略（规范原文），结果强制为 decline。
func TestFireElicitation_ExitTwoDeclines(t *testing.T) {
	src := Source{Key: "user:x", Scope: ScopeUser, Trusted: true, Config: mustConfig(t, `{"hooks":{
	  "Elicitation":[{"hooks":[{"type":"command","command":"echo denied >&2; exit 2"}]}]}}`)}
	r := newTestRunner(t, src)

	res, decided := r.FireElicitation(context.Background(), "srv", "form", "msg", "", "", nil)
	if !decided || res.Action != "decline" {
		t.Fatalf("exit 2 must decline: decided=%v res=%+v", decided, res)
	}
}

// TestFireElicitation_NoMatchLeavesUndecided 没有命中的 hook 时不得程序化决定，
// 调用方应继续走正常的用户交互路径（对话框 / URL 打开确认）。
func TestFireElicitation_NoMatchLeavesUndecided(t *testing.T) {
	r := newTestRunner(t)
	res, decided := r.FireElicitation(context.Background(), "srv", "form", "msg", "", "", nil)
	if decided {
		t.Fatalf("no matching hook must not decide: %+v", res)
	}
}

// TestFireElicitationResult_OverridesAction 验证 ElicitationResult hook 能在用户作答后、
// 回传服务器前覆盖 action（claude_hooks_elicitation.md §ElicitationResult output）。
func TestFireElicitationResult_OverridesAction(t *testing.T) {
	src := Source{Key: "user:x", Scope: ScopeUser, Trusted: true, Config: mustConfig(t, `{"hooks":{
	  "ElicitationResult":[{"hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"hookEventName\":\"ElicitationResult\",\"action\":\"decline\"}}'"}]}]}}`)}
	r := newTestRunner(t, src)

	action, content := r.FireElicitationResult(context.Background(), "srv", "form", "elicit-1", "accept", map[string]any{"username": "alice"})
	if action != "decline" {
		t.Fatalf("expected the hook override to win: action=%q content=%v", action, content)
	}
}

// TestFireElicitationResult_ExitTwoForcesDecline exit 2 强制覆盖为 decline，原内容丢弃。
func TestFireElicitationResult_ExitTwoForcesDecline(t *testing.T) {
	src := Source{Key: "user:x", Scope: ScopeUser, Trusted: true, Config: mustConfig(t, `{"hooks":{
	  "ElicitationResult":[{"hooks":[{"type":"command","command":"exit 2"}]}]}}`)}
	r := newTestRunner(t, src)

	action, content := r.FireElicitationResult(context.Background(), "srv", "form", "elicit-1", "accept", map[string]any{"username": "alice"})
	if action != "decline" || content != nil {
		t.Fatalf("exit 2 must force decline with no content: action=%q content=%v", action, content)
	}
}

// TestFireElicitationResult_NoMatchPassthrough 没有命中的 hook 时原样透传用户答复。
func TestFireElicitationResult_NoMatchPassthrough(t *testing.T) {
	r := newTestRunner(t)
	action, content := r.FireElicitationResult(context.Background(), "srv", "form", "elicit-1", "accept", map[string]any{"username": "alice"})
	if action != "accept" || content["username"] != "alice" {
		t.Fatalf("expected passthrough, got action=%q content=%v", action, content)
	}
}
