package elicitation

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/action/hook"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// fakeHookFirer 供测试用的 HookFirer 假实现：程序化作答/回传覆盖均可编程控制，
// 并记录 FireNotification 调用，供测试断言"是否真的通知了用户"。
type fakeHookFirer struct {
	mu sync.Mutex

	elicitDecision hook.ElicitDecision
	elicitDecided  bool

	resultOverride func(action string, content map[string]any) (string, map[string]any)

	notifications []string
	notifyCh      chan struct{} // 非 nil 时每次 FireNotification 都会发送一个信号
}

func (f *fakeHookFirer) FireElicitation(_ context.Context, _, _, _, _, _ string, _ json.RawMessage) (hook.ElicitDecision, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.elicitDecision, f.elicitDecided
}

func (f *fakeHookFirer) FireElicitationResult(_ context.Context, _, _, _, action string, content map[string]any) (string, map[string]any) {
	f.mu.Lock()
	override := f.resultOverride
	f.mu.Unlock()
	if override != nil {
		return override(action, content)
	}
	return action, content
}

func (f *fakeHookFirer) FireNotification(_ context.Context, message, _ string) {
	f.mu.Lock()
	f.notifications = append(f.notifications, message)
	ch := f.notifyCh
	f.mu.Unlock()
	if ch != nil {
		ch <- struct{}{}
	}
}

func simpleFormSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
}

// ─── 步骤 1：hook 程序化作答 ───────────────────────────────────────────────

func TestElicit_HookAccept_ValidContent(t *testing.T) {
	f := &fakeHookFirer{
		elicitDecided:  true,
		elicitDecision: hook.ElicitDecision{Action: "accept", Content: map[string]any{"name": "octocat"}},
	}
	b := NewBroker(f, 0)
	req := mcp.ElicitRequest{ServerName: "srv", Mode: "form", Message: "give name", RequestedSchema: simpleFormSchema()}

	res, err := b.Elicit(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != "accept" || res.Content["name"] != "octocat" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(f.notifications) != 0 {
		t.Fatalf("hook 已决定时不应弹用户对话框，但收到通知: %v", f.notifications)
	}
}

func TestElicit_HookAccept_InvalidContent_DeclinesWithoutDialog(t *testing.T) {
	f := &fakeHookFirer{
		elicitDecided:  true,
		elicitDecision: hook.ElicitDecision{Action: "accept", Content: map[string]any{"wrong_field": 1}}, // 缺少必填 name
	}
	b := NewBroker(f, 0)
	req := mcp.ElicitRequest{ServerName: "srv", Mode: "form", Message: "give name", RequestedSchema: simpleFormSchema()}

	res, err := b.Elicit(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != "decline" {
		t.Fatalf("expected decline for invalid content, got %+v", res)
	}
	if len(f.notifications) != 0 {
		t.Fatalf("非法内容降级为 decline 不应弹用户对话框: %v", f.notifications)
	}
}

func TestElicit_HookBlocked_EquivalentToExitCode2_Declines(t *testing.T) {
	// exit 2 / decision:block 在 hook.Runner.FireElicitation 中固定折算为
	// ElicitDecision{Action:"decline"}，decided=true（见 internal/action/hook/firer.go）。
	f := &fakeHookFirer{elicitDecided: true, elicitDecision: hook.ElicitDecision{Action: "decline"}}
	b := NewBroker(f, 0)
	req := mcp.ElicitRequest{ServerName: "srv", Mode: "form", Message: "m", RequestedSchema: simpleFormSchema()}

	res, err := b.Elicit(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != "decline" {
		t.Fatalf("expected decline, got %+v", res)
	}
}

// ─── 步骤 2~3：用户交互路径 ───────────────────────────────────────────────

func TestElicit_UserRespond_Accept(t *testing.T) {
	f := &fakeHookFirer{notifyCh: make(chan struct{}, 1)}
	b := NewBroker(f, time.Minute)
	req := mcp.ElicitRequest{ServerName: "srv", SessionID: "sess-1", Mode: "form", Message: "give name", RequestedSchema: simpleFormSchema()}

	resultCh := make(chan mcp.ElicitResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := b.Elicit(context.Background(), req)
		resultCh <- res
		errCh <- err
	}()

	<-f.notifyCh // 确认真的通知了用户
	var id string
	for i := 0; i < 100; i++ {
		views := b.Pending("sess-1")
		if len(views) == 1 {
			id = views[0].ID
			break
		}
		time.Sleep(time.Millisecond)
	}
	if id == "" {
		t.Fatal("pending elicitation 未登记")
	}

	if err := b.Respond(id, mcp.ElicitResult{Action: "accept", Content: map[string]any{"name": "alice"}}); err != nil {
		t.Fatalf("Respond failed: %v", err)
	}

	res := <-resultCh
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != "accept" || res.Content["name"] != "alice" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := b.Pending(""); len(got) != 0 {
		t.Fatalf("结束后 pending 应被移除，实际剩余: %+v", got)
	}
}

func TestElicit_Timeout_ReturnsCancel(t *testing.T) {
	f := &fakeHookFirer{}
	b := NewBroker(f, 20*time.Millisecond)
	req := mcp.ElicitRequest{ServerName: "srv", Mode: "form", Message: "m", RequestedSchema: simpleFormSchema()}

	res, err := b.Elicit(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != "cancel" {
		t.Fatalf("expected cancel on timeout, got %+v", res)
	}
	if got := b.Pending(""); len(got) != 0 {
		t.Fatalf("超时后 pending 应被移除: %+v", got)
	}
}

func TestElicit_CtxCancel_ReturnsCancel(t *testing.T) {
	f := &fakeHookFirer{}
	b := NewBroker(f, time.Minute)
	req := mcp.ElicitRequest{ServerName: "srv", Mode: "form", Message: "m", RequestedSchema: simpleFormSchema()}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan mcp.ElicitResult, 1)
	go func() {
		res, _ := b.Elicit(ctx, req)
		resultCh <- res
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case res := <-resultCh:
		if res.Action != "cancel" {
			t.Fatalf("expected cancel on ctx cancel, got %+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Elicit to return after ctx cancel")
	}
}

func TestElicit_ElicitationResultHookOverride(t *testing.T) {
	f := &fakeHookFirer{
		notifyCh: make(chan struct{}, 1),
		resultOverride: func(_ string, _ map[string]any) (string, map[string]any) {
			return "decline", nil // hook 覆盖用户的 accept 为 decline
		},
	}
	b := NewBroker(f, time.Minute)
	req := mcp.ElicitRequest{ServerName: "srv", Mode: "form", Message: "m", RequestedSchema: simpleFormSchema()}

	resultCh := make(chan mcp.ElicitResult, 1)
	go func() {
		res, _ := b.Elicit(context.Background(), req)
		resultCh <- res
	}()
	<-f.notifyCh

	var id string
	for i := 0; i < 100; i++ {
		views := b.Pending("")
		if len(views) == 1 {
			id = views[0].ID
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := b.Respond(id, mcp.ElicitResult{Action: "accept", Content: map[string]any{"name": "bob"}}); err != nil {
		t.Fatalf("Respond failed: %v", err)
	}

	res := <-resultCh
	if res.Action != "decline" {
		t.Fatalf("expected hook override to decline, got %+v", res)
	}
}

// ─── Respond 校验 ───────────────────────────────────────────────────────

func TestRespond_UnknownID_NotFound(t *testing.T) {
	b := NewBroker(nil, 0)
	err := b.Respond("no-such-id", mcp.ElicitResult{Action: "cancel"})
	if !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("expected CodeNotFound, got %v", err)
	}
}

func TestRespond_InvalidAction_InvalidInput(t *testing.T) {
	f := &fakeHookFirer{notifyCh: make(chan struct{}, 1)}
	b := NewBroker(f, time.Minute)
	req := mcp.ElicitRequest{ServerName: "srv", Mode: "form", Message: "m", RequestedSchema: simpleFormSchema()}
	go func() { _, _ = b.Elicit(context.Background(), req) }()
	<-f.notifyCh
	id := waitForOnePending(t, b)

	err := b.Respond(id, mcp.ElicitResult{Action: "maybe"})
	if !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected CodeInvalidInput, got %v", err)
	}
	// 校验失败不摘除 pending：允许前端修正后重试。
	if got := b.Pending(""); len(got) != 1 {
		t.Fatalf("invalid action 不应摘除 pending: %+v", got)
	}
}

func TestRespond_URLMode_AcceptWithContent_Rejected(t *testing.T) {
	f := &fakeHookFirer{notifyCh: make(chan struct{}, 1)}
	b := NewBroker(f, time.Minute)
	req := mcp.ElicitRequest{ServerName: "srv", Mode: "url", Message: "open this", URL: "https://example.com/auth"}
	go func() { _, _ = b.Elicit(context.Background(), req) }()
	<-f.notifyCh
	id := waitForOnePending(t, b)

	err := b.Respond(id, mcp.ElicitResult{Action: "accept", Content: map[string]any{"leak": "no"}})
	if !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected CodeInvalidInput for url-mode accept with content, got %v", err)
	}
}

func TestRespond_URLMode_AcceptWithoutContent_OK(t *testing.T) {
	f := &fakeHookFirer{notifyCh: make(chan struct{}, 1)}
	b := NewBroker(f, time.Minute)
	req := mcp.ElicitRequest{ServerName: "srv", Mode: "url", Message: "open this", URL: "https://example.com/auth"}
	resultCh := make(chan mcp.ElicitResult, 1)
	go func() {
		res, _ := b.Elicit(context.Background(), req)
		resultCh <- res
	}()
	<-f.notifyCh
	id := waitForOnePending(t, b)

	if err := b.Respond(id, mcp.ElicitResult{Action: "accept"}); err != nil {
		t.Fatalf("Respond failed: %v", err)
	}
	res := <-resultCh
	if res.Action != "accept" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestRespond_FormMode_InvalidContent_400Class(t *testing.T) {
	f := &fakeHookFirer{notifyCh: make(chan struct{}, 1)}
	b := NewBroker(f, time.Minute)
	req := mcp.ElicitRequest{ServerName: "srv", Mode: "form", Message: "m", RequestedSchema: simpleFormSchema()}
	go func() { _, _ = b.Elicit(context.Background(), req) }()
	<-f.notifyCh
	id := waitForOnePending(t, b)

	err := b.Respond(id, mcp.ElicitResult{Action: "accept", Content: map[string]any{}}) // 缺少必填 name
	if !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected CodeInvalidInput, got %v", err)
	}
}

// ─── Pending 过滤 ─────────────────────────────────────────────────────────

func TestPending_FiltersBySession(t *testing.T) {
	f := &fakeHookFirer{}
	b := NewBroker(f, time.Minute)

	go func() {
		_, _ = b.Elicit(context.Background(), mcp.ElicitRequest{ServerName: "s1", SessionID: "sess-a", Mode: "form", Message: "m", RequestedSchema: simpleFormSchema()})
	}()
	go func() {
		_, _ = b.Elicit(context.Background(), mcp.ElicitRequest{ServerName: "s2", SessionID: "sess-b", Mode: "form", Message: "m", RequestedSchema: simpleFormSchema()})
	}()

	var all []PendingView
	for i := 0; i < 200; i++ {
		all = b.Pending("")
		if len(all) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 pending total, got %d", len(all))
	}
	if got := b.Pending("sess-a"); len(got) != 1 || got[0].SessionID != "sess-a" {
		t.Fatalf("session 过滤失败: %+v", got)
	}
}

func waitForOnePending(t *testing.T, b *Broker) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		views := b.Pending("")
		if len(views) == 1 {
			return views[0].ID
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pending elicitation 未按期登记")
	return ""
}
