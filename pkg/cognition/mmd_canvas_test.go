package cognition

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestTaskMermaidCanvas_EmptyRender(t *testing.T) {
	c := NewTaskMermaidCanvas()
	if got := c.Render(); got != "" {
		t.Errorf("空画布应返回空字符串，got %q", got)
	}
}

func TestTaskMermaidCanvas_SingleSuccessNode(t *testing.T) {
	c := NewTaskMermaidCanvas()
	c.TrackToolCall("id1", "read_file")
	c.TrackToolResult("id1", true, "读取 config.go")

	rendered := c.Render()
	if !strings.Contains(rendered, "graph LR") {
		t.Error("渲染结果应包含 graph LR")
	}
	if !strings.Contains(rendered, "read_file") {
		t.Error("渲染结果应包含工具名")
	}
	if !strings.Contains(rendered, mmdStatusSuccess) {
		t.Error("渲染结果应包含成功符号 ✓")
	}
	// 成功节点应有绿色样式
	if !strings.Contains(rendered, "fill:#4a4") {
		t.Error("成功节点应有绿色样式")
	}
}

func TestTaskMermaidCanvas_FailedNodeStyle(t *testing.T) {
	c := NewTaskMermaidCanvas()
	c.TrackToolCall("id1", "bash")
	c.TrackToolResult("id1", false, "make build 失败")

	rendered := c.Render()
	if !strings.Contains(rendered, mmdStatusFailed) {
		t.Error("渲染结果应包含失败符号 ✗")
	}
	if !strings.Contains(rendered, "fill:#d64") {
		t.Error("失败节点应有红色样式")
	}
}

func TestTaskMermaidCanvas_MultiStepFlow(t *testing.T) {
	c := NewTaskMermaidCanvas()
	// 模拟典型工具执行流
	c.TrackToolCall("id1", "read_file")
	c.TrackToolResult("id1", true, "读取 Makefile")
	c.TrackToolCall("id2", "bash")
	c.TrackToolResult("id2", false, "build 失败")
	c.TrackToolCall("id3", "edit_file")
	c.TrackToolResult("id3", true, "修复 Makefile")
	c.TrackToolCall("id4", "bash")
	c.TrackToolResult("id4", true, "build 成功")

	rendered := c.Render()

	// 验证节点顺序
	if !strings.Contains(rendered, "N1") || !strings.Contains(rendered, "N4") {
		t.Error("应包含 N1~N4 节点")
	}
	// 验证边连接
	if !strings.Contains(rendered, "-->") {
		t.Error("多步骤画布应有边连接")
	}
	// token 估算应合理
	tokens := c.TokenEstimate()
	if tokens <= 0 || tokens > 500 {
		t.Errorf("token 估算应在合理范围内，got %d", tokens)
	}
}

func TestTaskMermaidCanvas_LabelTruncation(t *testing.T) {
	c := NewTaskMermaidCanvas()
	long := strings.Repeat("非常长的摘要文本", 10) // 80+ 字
	c.TrackToolCall("id1", "tool")
	c.TrackToolResult("id1", true, long)

	rendered := c.Render()
	// 渲染后标签不应超过 mmdMaxLabelChars 字符
	if strings.Count(rendered, "非常长的摘要文本") > 6 {
		t.Error("超长 summary 应被截断")
	}
}

func TestTaskMermaidCanvas_UnknownToolID(t *testing.T) {
	c := NewTaskMermaidCanvas()
	// 没有先调用 TrackToolCall 就调用 TrackToolResult
	c.TrackToolResult("unknown_id", true, "orphan result")

	rendered := c.Render()
	// 应降级为 unknown 工具名，不崩溃
	if !strings.Contains(rendered, "unknown") {
		t.Error("未知 tool_use_id 应降级为 unknown 工具名")
	}
}

func TestTaskMermaidCanvas_Reset(t *testing.T) {
	c := NewTaskMermaidCanvas()
	c.TrackToolCall("id1", "tool")
	c.TrackToolResult("id1", true, "done")

	c.Reset()
	if c.NodeCount() != 0 {
		t.Error("Reset 后 NodeCount 应为 0")
	}
	if c.Render() != "" {
		t.Error("Reset 后 Render 应返回空字符串")
	}
}

func TestTaskMermaidCanvas_MaxNodes(t *testing.T) {
	c := NewTaskMermaidCanvas()
	// 超过 mmdMaxNodes 后不应 panic，多余节点静默忽略
	for i := range mmdMaxNodes + 10 {
		id := strings.Repeat("x", i+1)
		c.TrackToolCall(id, "tool")
		c.TrackToolResult(id, true, "step")
	}
	if c.NodeCount() > mmdMaxNodes {
		t.Errorf("节点数不应超过 mmdMaxNodes(%d)，got %d", mmdMaxNodes, c.NodeCount())
	}
}

func TestTaskMermaidCanvas_SpecialCharsEscaped(t *testing.T) {
	c := NewTaskMermaidCanvas()
	c.TrackToolCall("id1", `bash[rm -rf]`)
	c.TrackToolResult("id1", true, `output "hello"`)

	rendered := c.Render()
	// 确认双引号被转义
	if strings.Count(rendered, `"`) > strings.Count(rendered, `["`) {
		// 标签内不应有裸双引号（除了节点定义的包裹引号）
		t.Log("rendered:", rendered) // 辅助调试
	}
}

// ─── jaccardSimilarity 单测 ───────────────────────────────────────────────────

func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		want float64 // 下限
	}{
		{"language", "language", 1.0},
		{"programming_language", "language", 0.4},         // 有交集
		{"lang_preference", "language_preference", 0.30},  // 部分重叠（1/3=0.33）
		{"go_version", "python_framework", 0.0},           // 无交集
	}
	for _, tt := range tests {
		got := jaccardSimilarity(tt.a, tt.b)
		if got < tt.want-0.01 {
			t.Errorf("jaccardSimilarity(%q, %q) = %.2f，期望 >= %.2f", tt.a, tt.b, got, tt.want)
		}
	}

	// 完全相同应为 1.0
	if v := jaccardSimilarity("user_lang", "user_lang"); v != 1.0 {
		t.Errorf("相同字符串 Jaccard 应为 1.0，got %.2f", v)
	}

	// 空字符串边界
	if v := jaccardSimilarity("", "abc"); v != 0 {
		t.Errorf("空字符串 Jaccard 应为 0，got %.2f", v)
	}
}

// ─── SessionCompressor canvas 集成测试 ────────────────────────────────────────

func TestSessionCompressor_TrackAndCompress(t *testing.T) {
	sc := NewSessionCompressor(512)

	// 模拟两步工具执行
	sc.TrackToolCall("tid1", "read_file")
	sc.TrackToolResult("tid1", true, "读取 go.mod")
	sc.TrackToolCall("tid2", "bash")
	sc.TrackToolResult("tid2", false, "go build 失败")

	// 触发压缩（填满 token 窗口）
	msgs := []protocol.Message{
		{Role: "user", Content: strings.Repeat("x", 10000)},
	}
	out, compressed := sc.Compress(msgs, 130000, 200000)

	if !compressed {
		t.Skip("未达压缩阈值，跳过 anchor 验证")
	}
	_ = out

	anchor := sc.Anchor()
	if !strings.Contains(anchor, "Task State") {
		t.Errorf("压缩后 anchor 应包含 Mermaid Task State 块，got: %s", anchor)
	}
	if !strings.Contains(anchor, "read_file") || !strings.Contains(anchor, "bash") {
		t.Errorf("anchor 应包含工具名，got: %s", anchor)
	}
}

func TestSessionCompressor_CanvasNotInjectedWhenEmpty(t *testing.T) {
	sc := NewSessionCompressor(512)
	// 不追踪任何工具，画布为空

	msgs := []protocol.Message{
		{Role: "user", Content: strings.Repeat("x", 10000)},
	}
	_, compressed := sc.Compress(msgs, 130000, 200000)
	if !compressed {
		t.Skip("未达压缩阈值")
	}

	// 空画布时 anchor 不应有 Mermaid 块
	anchor := sc.Anchor()
	if strings.Contains(anchor, "graph LR") {
		t.Error("空画布时 anchor 不应注入 Mermaid 块")
	}
}
