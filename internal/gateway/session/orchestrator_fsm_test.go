package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeSink struct {
	events []Event
}

func (s *fakeSink) Emit(e Event) error {
	s.events = append(s.events, e)
	return nil
}

func TestOrchestrator_FSMSlidingWindowLeak(t *testing.T) {
	cfg := config.Config{}
	cfg.Thresholds.Session.LeakScanWindowBytes = 64
	config.Update(&cfg)

	o := &orchestrator{}

	g := guard.NewSystemPromptGuard(3)
	g.AddFragment("alpha beta gamma delta epsilon")

	var replyBuilder []byte
	var errBuilder string
	windowSize := 64
	var leakWindow []byte
	var viewIDs []string

	sink := &fakeSink{}

	o.handleFSMEvent(context.Background(), sink, "sess-1", types.AgentStreamEvent{
		Type:    types.AgentStreamEventToken,
		Content: "alpha ",
	}, g, &replyBuilder, &errBuilder, new(apperr.Code), &leakWindow, windowSize, &viewIDs)
	require.Equal(t, "alpha ", string(replyBuilder))

	o.handleFSMEvent(context.Background(), sink, "sess-1", types.AgentStreamEvent{
		Type:    types.AgentStreamEventToken,
		Content: "beta ",
	}, g, &replyBuilder, &errBuilder, new(apperr.Code), &leakWindow, windowSize, &viewIDs)
	require.Equal(t, "alpha beta ", string(replyBuilder))

	o.handleFSMEvent(context.Background(), sink, "sess-1", types.AgentStreamEvent{
		Type:    types.AgentStreamEventToken,
		Content: "gamma",
	}, g, &replyBuilder, &errBuilder, new(apperr.Code), &leakWindow, windowSize, &viewIDs)
	require.Equal(t, "alpha beta ", string(replyBuilder))

	o.handleFSMEvent(context.Background(), sink, "sess-1", types.AgentStreamEvent{
		Type:    types.AgentStreamEventToken,
		Content: " normal text",
	}, g, &replyBuilder, &errBuilder, new(apperr.Code), &leakWindow, windowSize, &viewIDs)
	require.Equal(t, "alpha beta  normal text", string(replyBuilder))

	// Turn 2
	var leakWindowTurn2 []byte
	o.handleFSMEvent(context.Background(), sink, "sess-1", types.AgentStreamEvent{
		Type:    types.AgentStreamEventToken,
		Content: "gamma",
	}, g, &replyBuilder, &errBuilder, new(apperr.Code), &leakWindowTurn2, windowSize, &viewIDs)
	require.Equal(t, "alpha beta  normal textgamma", string(replyBuilder))
}

// TestOrchestrator_ToolResultWithUIEmitsToolUIStatus 验证 M8f-1：带 UI 的工具结果
// 事件除常规 "tool_result" 外，额外发一条 KindStatus "tool_ui"，落库视图快照，
// 且不影响模型可见文本本身（本用例不涉及 reply 累积，Content 走独立断言）。
func TestOrchestrator_ToolResultWithUIEmitsToolUIStatus(t *testing.T) {
	persistence := newFakePersistence()
	o := &orchestrator{persistence: persistence}
	g := guard.NewSystemPromptGuard(3)

	var replyBuilder []byte
	var errBuilder string
	var leakWindow []byte
	var viewIDs []string
	sink := &fakeSink{}

	ev := types.AgentStreamEvent{
		Type:    types.AgentStreamEventToolResult,
		Content: "dashboard refreshed",
		UI: &types.ToolUIRef{
			ViewID:      "view-1",
			ServerID:    "srv1",
			ResourceURI: "ui://srv1/dash",
			ToolName:    "dash",
			ToolInput:   []byte(`{"x":1}`),
			ToolResult:  []byte(`{"content":[{"type":"text","text":"ok"}]}`),
		},
	}
	stop := o.handleFSMEvent(context.Background(), sink, "sess-1", ev, g, &replyBuilder, &errBuilder, new(apperr.Code), &leakWindow, 64, &viewIDs)
	require.False(t, stop)

	// 常规 tool_result + 新增 tool_ui 两条状态事件都要出现。
	var sawToolResult, sawToolUI bool
	for _, e := range sink.events {
		if e.Kind != KindStatus {
			continue
		}
		switch e.Payload["type"] {
		case "tool_result":
			sawToolResult = true
			require.Equal(t, "dashboard refreshed", e.Payload["message"])
		case "tool_ui":
			sawToolUI = true
			require.Equal(t, "view-1", e.Payload["view_id"])
			require.Equal(t, "srv1", e.Payload["server_id"])
			require.Equal(t, "ui://srv1/dash", e.Payload["resource_uri"])
		}
	}
	require.True(t, sawToolResult, "expected tool_result status event")
	require.True(t, sawToolUI, "expected tool_ui status event")

	// 视图已落库，且 viewID 被记入本轮累计列表，供后续 LinkAppViewsToMessage 使用。
	require.Len(t, persistence.appViews, 1)
	require.Equal(t, "view-1", persistence.appViews[0].ViewID)
	require.Equal(t, []string{"view-1"}, viewIDs)
}

func TestHandleFSMErrorUsesKnownCodeAndSafeMessage(t *testing.T) {
	o := &orchestrator{}
	sink := &fakeSink{}
	g := guard.NewSystemPromptGuard(3)
	var reply []byte
	var inferErr string
	var errorCode apperr.Code
	var leakWindow []byte
	var viewIDs []string
	marker := "provider-path-token-sentinel"
	o.handleFSMEvent(context.Background(), sink, "session-safe", types.AgentStreamEvent{
		Type: types.AgentStreamEventError, Content: marker, ErrorCode: "not_an_apperr_code",
	}, g, &reply, &inferErr, &errorCode, &leakWindow, 64, &viewIDs)
	if len(sink.events) != 1 {
		t.Fatalf("expected one error event, got %d", len(sink.events))
	}
	ev := sink.events[0]
	if ev.Payload["code"] != string(apperr.CodeInternal) || ev.Payload["kind"] != "fsm_error" || ev.Payload["message"] != "模型调用失败，请稍后重试" {
		t.Fatalf("unexpected error payload: %#v", ev.Payload)
	}
	encoded, err := json.Marshal(ev)
	if err != nil || strings.Contains(string(encoded), marker) {
		t.Fatalf("unsafe error event serialization %q: %v", encoded, err)
	}
	var ae *apperr.Error
	if !errors.As(ev.Err, &ae) || ae.Code != apperr.CodeInternal || ae.Cause != nil {
		t.Fatalf("Event.Err must retain only a cause-free safe error: %#v", ev.Err)
	}
}
