package tool

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeSessionEventWriter struct {
	sessionID      string
	toolName       string
	input, output  map[string]any
	writeCallCount int
}

func (f *fakeSessionEventWriter) WriteToolCallEvent(sessionID, toolName string, input, output map[string]any) {
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
	writeToolCallOutcome(ctx, writer, "my_tool", []byte("{also not valid"), res, "")

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
	writeToolCallOutcome(ctx, writer, "my_tool", []byte(`{"x":1}`), res, "")

	if writer.writeCallCount != 1 {
		t.Fatalf("expected WriteToolCallEvent to be called exactly once, got %d", writer.writeCallCount)
	}
	if writer.input["x"] != float64(1) {
		t.Errorf("expected input[x]=1, got %v", writer.input)
	}
	if writer.output["ok"] != true {
		t.Errorf("expected output[ok]=true, got %v", writer.output)
	}
}
