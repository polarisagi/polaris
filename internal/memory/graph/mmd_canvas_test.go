package graph

import (
	"fmt"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol/repo"
)

func toolRow(tool string, ok bool, payload string) repo.TrajectoryRow {
	return repo.TrajectoryRow{ToolName: tool, ToolOK: &ok, Payload: payload}
}

func TestRenderMmdCanvas_Empty(t *testing.T) {
	if got := RenderMmdCanvas(nil); got != "" {
		t.Errorf("无步骤应返回空字符串，got %q", got)
	}
	if got := RenderMmdCanvas(MmdStepsFromTrajectory(nil)); got != "" {
		t.Errorf("空轨迹应返回空字符串，got %q", got)
	}
}

func TestRenderMmdCanvas_SingleSuccessNode(t *testing.T) {
	rendered := RenderMmdCanvas(MmdStepsFromTrajectory([]repo.TrajectoryRow{
		toolRow("read_file", true, `{"tool":"read_file","result":{"path":"config.go"}}`),
	}))
	for _, want := range []string{"graph LR", "read_file", mmdStatusSuccess, "fill:#4a4", "config.go", `N1["`} {
		if !strings.Contains(rendered, want) {
			t.Errorf("渲染结果应包含 %q:\n%s", want, rendered)
		}
	}
}

func TestRenderMmdCanvas_FailedNodeUsesErrorText(t *testing.T) {
	rendered := RenderMmdCanvas(MmdStepsFromTrajectory([]repo.TrajectoryRow{
		toolRow("bash", false, `{"result":{"error":"make build 失败"}}`),
	}))
	if !strings.Contains(rendered, mmdStatusFailed) || !strings.Contains(rendered, "fill:#d64") {
		t.Errorf("失败节点应有 ✗ 与红色样式:\n%s", rendered)
	}
	if !strings.Contains(rendered, "make build 失败") {
		t.Errorf("失败摘要应取 error 文本:\n%s", rendered)
	}
}

func TestRenderMmdCanvas_MultiStepFlow(t *testing.T) {
	rendered := RenderMmdCanvas(MmdStepsFromTrajectory([]repo.TrajectoryRow{
		toolRow("read_file", true, `{}`),
		toolRow("bash", false, `not json`),
		{EventType: "llm_call", Payload: `{}`}, // 非工具行不入画布
		toolRow("edit_file", true, `{}`),
	}))
	if !strings.Contains(rendered, "N1") || !strings.Contains(rendered, "N3") || strings.Contains(rendered, "N4") {
		t.Errorf("应恰有 N1~N3:\n%s", rendered)
	}
	if strings.Count(rendered, "-->") != 2 {
		t.Errorf("3 步应有 2 条顺序边:\n%s", rendered)
	}
	if !strings.Contains(rendered, "bash "+mmdStatusFailed+" | failed") {
		t.Errorf("坏 payload 的失败步骤摘要应降级为 failed:\n%s", rendered)
	}
}

func TestRenderMmdCanvas_LabelTruncation(t *testing.T) {
	long := strings.Repeat("非常长的摘要文本", 10)
	rendered := RenderMmdCanvas(MmdStepsFromTrajectory([]repo.TrajectoryRow{
		toolRow("tool", true, fmt.Sprintf(`{"result":{"s":%q}}`, long)),
	}))
	if strings.Count(rendered, "非常长的摘要文本") > 6 {
		t.Error("超长 summary 应被截断")
	}
	if !strings.Contains(rendered, "…") {
		t.Error("截断应带省略号")
	}
}

// 超过 mmdMaxNodes 时保留最近的步骤，并重新编号，不 panic。
func TestMmdStepsFromTrajectory_KeepsMostRecent(t *testing.T) {
	rows := make([]repo.TrajectoryRow, 0, mmdMaxNodes+10)
	for i := range mmdMaxNodes + 10 {
		rows = append(rows, toolRow(fmt.Sprintf("tool%d", i), true, `{}`))
	}
	steps := MmdStepsFromTrajectory(rows)
	if len(steps) != mmdMaxNodes {
		t.Fatalf("节点数应封顶 %d，got %d", mmdMaxNodes, len(steps))
	}
	if steps[0].Tool != "tool10" || steps[0].NodeID != "N1" || steps[len(steps)-1].Tool != fmt.Sprintf("tool%d", mmdMaxNodes+9) {
		t.Errorf("应保留最近步骤并从 N1 重编号: first=%+v last=%+v", steps[0], steps[len(steps)-1])
	}
}

func TestRenderMmdCanvas_SpecialCharsEscaped(t *testing.T) {
	rendered := RenderMmdCanvas(MmdStepsFromTrajectory([]repo.TrajectoryRow{
		toolRow(`bash[rm -rf]`, true, `{"result":{"o":"say \"hello\""}}`),
	}))
	if strings.Contains(rendered, "bash[rm") || strings.Contains(rendered, `\"`) {
		t.Errorf("方括号/双引号应被转义:\n%s", rendered)
	}
}

func TestMmdStepsFromTrajectory_NullToolOKDrawnFailed(t *testing.T) {
	steps := MmdStepsFromTrajectory([]repo.TrajectoryRow{{ToolName: "x", Payload: `{}`}})
	if len(steps) != 1 || steps[0].Status != mmdStatusFailed {
		t.Fatalf("NULL tool_ok 应按失败画: %+v", steps)
	}
}
