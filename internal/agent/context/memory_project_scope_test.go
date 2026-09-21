package agentctx

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// 以下 TestProjectIsolation* 用例由 tools/memory_isolation_check.go 纳入 make lint（ADR-0097 决策三修订）。

const alphaMarker = "ALPHA-MARKER-7f3c"

// fakeCog 共享 FTS 同时命中：项目 A 的情景事件、项目 B 的情景事件、一条语义实体。
type fakeCog struct{}

func (fakeCog) FTSSearch(string, int) ([]fsm.CogResult, error) {
	return []fsm.CogResult{
		{DocID: "evA", Snippet: alphaMarker + " 部署密钥", Score: 9},
		{DocID: "evB", Snippet: "BRAVO 部署流程", Score: 5},
		{DocID: "sement_Tool_go", Snippet: "GLOBAL-ENTITY Go 1.26", Score: 3},
	}, nil
}
func (fakeCog) VecKNN([]float32, int) ([]fsm.CogResult, error) { return nil, nil }

func scopedMem() *mockMemory {
	return &mockMemory{
		episodic:      &mockEpisodicMem{},
		working:       &mockWorkingMem{immutable: &mockImmutableCore{}},
		eventProjects: map[string]string{"evA": "prj_a", "evB": "prj_b"},
	}
}

func allContent(msgs []types.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestProjectIsolation_PerceiveContext 读取面 P1（情景查询带项目）与 P3（共享 FTS 按项目剔除）。
func TestProjectIsolation_PerceiveContext(t *testing.T) {
	mem := scopedMem()
	sCtx := &fsm.StateContext{
		TaskID: "t1", SessionID: "sB", ProjectID: "prj_b",
		RawIntentTS: taint.NewTaintedString("部署", taint.TaintSource{}, "test"),
		TaskModel:   &fsm.TaskModel{Goal: "部署"},
	}
	msgs, err := BuildPerceiveContext(context.Background(), mem, sCtx, fakeCog{})
	if err != nil {
		t.Fatal(err)
	}
	assertScoped(t, "perceive", mem, allContent(msgs))
}

// TestProjectIsolation_PlanContext 读取面 P2 与 P4。
func TestProjectIsolation_PlanContext(t *testing.T) {
	mem := scopedMem()
	sCtx := &fsm.StateContext{
		TaskID: "t1", SessionID: "sB", ProjectID: "prj_b",
		TaskModel: &fsm.TaskModel{Goal: "部署"},
	}
	msgs, err := BuildPlanContext(context.Background(), mem, sCtx, nil, fakeCog{})
	if err != nil {
		t.Fatal(err)
	}
	assertScoped(t, "plan", mem, allContent(msgs))
}

func assertScoped(t *testing.T, stage string, mem *mockMemory, content string) {
	t.Helper()
	if strings.Contains(content, alphaMarker) {
		t.Errorf("%s: 项目 B 的 Prompt 混入了项目 A 的情景记忆", stage)
	}
	if !strings.Contains(content, "BRAVO") {
		t.Errorf("%s: 本项目的情景命中不应被剔除", stage)
	}
	if !strings.Contains(content, "GLOBAL-ENTITY") {
		t.Errorf("%s: 语义实体属全局层，应保留", stage)
	}
	if len(mem.episodic.queries) == 0 {
		t.Fatalf("%s: 未发起情景查询", stage)
	}
	for _, q := range mem.episodic.queries {
		if q.ProjectID != "prj_b" {
			t.Errorf("%s: 情景查询未限定项目，got ProjectID=%q", stage, q.ProjectID)
		}
	}
}

// TestProjectIsolation_UnresolvedProjectFailsClosed 未解析项目的 Agent 以默认项目为界。
func TestProjectIsolation_UnresolvedProjectFailsClosed(t *testing.T) {
	if got := scopeProjectID(&fsm.StateContext{}); got != types.DefaultProjectID {
		t.Fatalf("未设置项目时应按默认项目，got %q", got)
	}
	mem := scopedMem()
	out, _ := projectScopedFTS(context.Background(), mem, fakeCog{}, "部署", 5, types.DefaultProjectID)
	for _, h := range out {
		if h.DocID == "evA" || h.DocID == "evB" {
			t.Fatalf("默认项目不应看到具名项目的事件 %s", h.DocID)
		}
	}
	if out, _ := projectScopedFTS(context.Background(), nil, fakeCog{}, "部署", 5, "prj_a"); len(out) != 0 {
		t.Fatal("无法反查归属时应整体返回空（fail-closed）")
	}
}
