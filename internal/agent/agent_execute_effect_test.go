package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/agent/schemavalidate"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestToolCallsToDAGJSON_ValidatesAgainstPlanDagSchema 验证原生 tool_calls 转换出的
// DAGModel JSON 满足 plan_dag Schema（尤其是 edges 必须是空数组而非 null——
// json.Marshal 对 nil slice 会产出 null，触发 schemas.json 里 edges 的 type:"array" 校验失败）。
func TestToolCallsToDAGJSON_ValidatesAgainstPlanDagSchema(t *testing.T) {
	calls := []types.InferToolCall{
		{ID: "call_1", Name: "fetch_url", Input: []byte(`{"url":"https://example.com"}`)},
		{ID: "", Name: "no_args_tool", Input: nil},
	}

	out, err := toolCallsToDAGJSON(calls)
	if err != nil {
		t.Fatalf("toolCallsToDAGJSON failed: %v", err)
	}

	if schemaErr := schemavalidate.Validate("plan_dag", out); schemaErr != nil {
		t.Fatalf("synthesized DAGModel JSON failed plan_dag schema validation: %v\njson: %s", schemaErr, out)
	}

	var plan types.DAGModel
	if err := json.Unmarshal(out, &plan); err != nil {
		t.Fatalf("round-trip unmarshal failed: %v", err)
	}
	if len(plan.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(plan.Nodes))
	}
	if plan.Nodes[0].ID != "call_1" || plan.Nodes[0].Action != "fetch_url" {
		t.Errorf("node[0] mismatch: %+v", plan.Nodes[0])
	}
	if plan.Nodes[0].Params["url"] != "https://example.com" {
		t.Errorf("node[0] params mismatch: %+v", plan.Nodes[0].Params)
	}
	// 空 ID 应回退生成合成 ID，不留空字符串（plan_dag Schema 要求 id 必填非空语义）。
	if plan.Nodes[1].ID == "" || plan.Nodes[1].Action != "no_args_tool" {
		t.Errorf("node[1] mismatch: %+v", plan.Nodes[1])
	}
	if plan.Edges == nil {
		t.Error("expected non-nil (empty) Edges slice")
	}
}

// TestDoStreamInfer_StreamToolCall 验证原生 StreamToolCall 事件被 doStreamInfer 正确
// 累积进 ProviderResponse.ToolCalls——此前该分支不存在，StreamToolCall 事件被静默丢弃，
// 是"WithTools 全链路死管线"的最后一环缺失。
func TestDoStreamInfer_StreamToolCall(t *testing.T) {
	a := &Agent{sCtx: &fsm.StateContext{}}

	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{
		Type:    types.StreamToolCall,
		Content: `{"id":"call_1","name":"fetch_url","input":{"url":"https://example.com"}}`,
	}
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: ""}
	close(ch)

	resp, err := a.doStreamInfer(context.Background(), ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 accumulated tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "fetch_url" || resp.ToolCalls[0].ID != "call_1" {
		t.Errorf("tool call mismatch: %+v", resp.ToolCalls[0])
	}
}

// TestDoStreamInfer_StreamToolCall_MalformedPayloadSkipped 验证畸形 StreamToolCall
// payload 不会导致 doStreamInfer 整体失败（fail-open 跳过该条，其余流内容照常聚合）。
func TestDoStreamInfer_StreamToolCall_MalformedPayloadSkipped(t *testing.T) {
	a := &Agent{sCtx: &fsm.StateContext{}}

	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: types.StreamToolCall, Content: `not-json`}
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: "hello"}
	close(ch)

	resp, err := a.doStreamInfer(context.Background(), ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("expected malformed tool call to be skipped, got %d", len(resp.ToolCalls))
	}
	if resp.Content != "hello" {
		t.Errorf("expected text content to still accumulate, got %q", resp.Content)
	}
}
