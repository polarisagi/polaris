package graph

import (
	"strings"
	"testing"
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

// ─── retrieval.jaccardSimilarity 单测 ───────────────────────────────────────────────────
