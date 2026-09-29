package tool

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeSessionEventWriter struct {
	sessionID      string
	toolName       string
	input, output  map[string]any
	ok             bool
	latencyMs      int64
	writeCallCount int
}

func (f *fakeSessionEventWriter) WriteToolCallEvent(sessionID, toolName string, input, output map[string]any, ok bool, latencyMs int64) {
	f.ok = ok
	f.latencyMs = latencyMs
	f.writeCallCount++
	f.sessionID = sessionID
	f.toolName = toolName
	f.input = input
	f.output = output
}

// TestWriteToolCallOutcome_MalformedJSON_DegradesGracefully_S02 验证阶段02修复：
// input/output JSON 解析失败时，writeToolCallOutcome 仍必须调用
// WriteToolCallEvent（工具执行结果已经发生，不能因为 outcome 记录的丰富度
// 问题而丢失整条事件），对应字段退化为 nil map，而不是 panic 或提前 return。
// 回归锚点：修复前 `_ = json.Unmarshal(...)` 吞没错误——行为本身未变（已经是
// 忽略并继续），本测试确保重构未引入新的提前返回/panic 回归。
func TestWriteToolCallOutcome_MalformedJSON_DegradesGracefully_S02(t *testing.T) {
	writer := &fakeSessionEventWriter{}
	ctx := context.WithValue(context.Background(), protocol.CtxSessionIDKey{}, "sess-1")

	res := &types.ToolResult{Output: []byte("{not valid json")}
	writeToolCallOutcome(ctx, writer, "my_tool", []byte("{also not valid"), res, "", false, 0)

	if writer.writeCallCount != 1 {
		t.Fatalf("expected WriteToolCallEvent to be called exactly once, got %d", writer.writeCallCount)
	}
	if writer.sessionID != "sess-1" || writer.toolName != "my_tool" {
		t.Errorf("unexpected sessionID/toolName: %q/%q", writer.sessionID, writer.toolName)
	}
	if writer.input != nil {
		t.Errorf("expected input map to degrade to nil on malformed JSON, got %v", writer.input)
	}
	if writer.output != nil {
		t.Errorf("expected output map to degrade to nil on malformed JSON, got %v", writer.output)
	}
}

// TestWriteToolCallOutcome_ValidJSON_ParsesFields_S02 对照用例：合法 JSON 正常解析，
// 确保 L3 错误处理分支没有误伤成功路径。
func TestWriteToolCallOutcome_ValidJSON_ParsesFields_S02(t *testing.T) {
	writer := &fakeSessionEventWriter{}
	ctx := context.WithValue(context.Background(), protocol.CtxSessionIDKey{}, "sess-2")

	res := &types.ToolResult{Output: []byte(`{"ok":true}`)}
	writeToolCallOutcome(ctx, writer, "my_tool", []byte(`{"x":1}`), res, "", true, 42)

	if writer.writeCallCount != 1 {
		t.Fatalf("expected WriteToolCallEvent to be called exactly once, got %d", writer.writeCallCount)
	}
	if writer.input["x"] != float64(1) {
		t.Errorf("expected input[x]=1, got %v", writer.input)
	}
	if writer.output["ok"] != true {
		t.Errorf("expected output[ok]=true, got %v", writer.output)
	}
	if !writer.ok || writer.latencyMs != 42 {
		t.Errorf("成败/耗时应原样透传，got ok=%v latency=%d", writer.ok, writer.latencyMs)
	}
}

// 会话事件的成败与 PolicyEvolver 上报使用同一判据：成功/sandbox 报错两条路径各验一次。
func TestExecuteTool_SessionEventCarriesOutcome(t *testing.T) {
	r, sbx := newAllowRegistry()
	_ = r.Register(minTool("ok"))
	_ = r.Register(minTool("boom"))
	sbx.Register("ok", func(_ context.Context, _ []byte) ([]byte, error) { return []byte(`{"a":1}`), nil })
	sbx.Register("boom", func(_ context.Context, _ []byte) ([]byte, error) {
		return nil, apperr.New(apperr.CodeInternal, "kaboom")
	})
	w := &fakeSessionEventWriter{}
	rec := &mockOutcomeRecorder{}
	r.WithSessionEventWriter(w).WithOutcomeRecorder(rec)
	ctx := context.WithValue(ctxWithToken(), protocol.CtxSessionIDKey{}, "sess-x")

	if _, err := r.ExecuteTool(ctx, "ok", []byte(`{"x":1}`), types.TaintNone); err != nil {
		t.Fatal(err)
	}
	if !w.ok || w.ok != rec.calls[0].success || w.latencyMs != rec.calls[0].latencyMs {
		t.Fatalf("成功路径口径不一致: writer ok=%v lat=%d rec=%+v", w.ok, w.latencyMs, rec.calls[0])
	}
	if _, err := r.ExecuteTool(ctx, "boom", []byte(`{"x":1}`), types.TaintNone); err != nil {
		t.Fatal(err)
	}
	if w.ok || w.ok != rec.calls[1].success || w.toolName != "boom" {
		t.Fatalf("失败路径口径不一致: writer ok=%v rec=%+v", w.ok, rec.calls[1])
	}
	if w.output["error"] == nil {
		t.Fatalf("失败事件 payload 应带 error: %v", w.output)
	}
}
