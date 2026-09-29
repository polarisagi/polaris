package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// fakeToolTrajectory 按会话分桶的内存轨迹，模拟 ListToolBySession 只返回该会话行的契约。
type fakeToolTrajectory struct {
	bySession map[string][]repo.TrajectoryRow
	err       error
}

func (f *fakeToolTrajectory) ListToolBySession(_ context.Context, sessionID string) ([]repo.TrajectoryRow, error) {
	return f.bySession[sessionID], f.err
}

func canvasRow(tool string, ok bool) repo.TrajectoryRow {
	return repo.TrajectoryRow{ToolName: tool, ToolOK: &ok, Payload: `{"result":{}}`}
}

func TestRenderTaskCanvas_PerSessionNoCrossTalk(t *testing.T) {
	m := &MemImpl{}
	m.SetTrajectoryReader(&fakeToolTrajectory{bySession: map[string][]repo.TrajectoryRow{
		"s1": {canvasRow("read_file", true), canvasRow("bash", false)},
		"s2": {canvasRow("web_search", true)},
	}})
	ctx := context.Background()

	c1, c2 := m.RenderTaskCanvas(ctx, "s1"), m.RenderTaskCanvas(ctx, "s2")
	if !strings.Contains(c1, "read_file") || !strings.Contains(c1, "bash") || strings.Contains(c1, "web_search") {
		t.Errorf("s1 画布应只含 s1 的工具:\n%s", c1)
	}
	if !strings.Contains(c2, "web_search") || strings.Contains(c2, "read_file") || strings.Contains(c2, "bash") {
		t.Errorf("s2 画布应只含 s2 的工具:\n%s", c2)
	}
}

func TestRenderTaskCanvas_EmptyCases(t *testing.T) {
	ctx := context.Background()
	if got := (&MemImpl{}).RenderTaskCanvas(ctx, "s"); got != "" {
		t.Errorf("未注入读取端应为空，got %q", got)
	}
	m := &MemImpl{}
	m.SetTrajectoryReader(&fakeToolTrajectory{bySession: map[string][]repo.TrajectoryRow{"s1": {canvasRow("x", true)}}})
	if got := m.RenderTaskCanvas(ctx, "no-tools-session"); got != "" {
		t.Errorf("无工具调用的会话应为空，got %q", got)
	}
	if got := m.RenderTaskCanvas(ctx, ""); got != "" {
		t.Errorf("空会话 ID 应为空，got %q", got)
	}
	m.SetTrajectoryReader(&fakeToolTrajectory{err: apperr.New(apperr.CodeInternal, "db down")})
	if got := m.RenderTaskCanvas(ctx, "s1"); got != "" {
		t.Errorf("读取失败应降级为空，got %q", got)
	}
}
