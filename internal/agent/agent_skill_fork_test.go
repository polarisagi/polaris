package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeSkillForker struct{ renders int }

func (f *fakeSkillForker) ForkTarget(_ context.Context, skillName string) (string, bool) {
	return "Explore", skillName == "skill:research"
}

func (f *fakeSkillForker) RenderSkill(_ context.Context, skillName string, _ []byte) (string, error) {
	f.renders++
	return "rendered " + skillName, nil
}

func TestTryForkSkill_DelegatesAndResumesWithoutRerender(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)
	cat := catalog.NewMemoryCatalog()
	cat.Register(protocol.CatalogEntry{Name: "research", Source: types.ToolSkill, SkillName: "skill:research"})
	cat.Register(protocol.CatalogEntry{Name: "inline", Source: types.ToolSkill, SkillName: "skill:inline"})
	a.InjectCatalog(cat)
	forker := &fakeSkillForker{}
	a.InjectSkillForker(forker)
	ctx := context.Background()

	if _, handled, _ := a.tryForkSkill(ctx, "inline", nil, types.TaintLow); handled {
		t.Fatal("non-fork skills run as ordinary tools")
	}
	res, handled, err := a.tryForkSkill(ctx, "research", []byte(`{"arguments":"auth"}`), types.TaintLow)
	if err != nil || !handled || !res.Suspended {
		t.Fatalf("fork skill must suspend on delegation: %+v %v %v", res, handled, err)
	}
	if poster.lastPosted.Type != "agent_handoff:Explore" || string(poster.lastPosted.Intent) != "rendered skill:research" {
		t.Fatalf("posted: %+v", poster.lastPosted)
	}
	// 恢复分支：子任务完成后重入，不得再次渲染（渲染会重复执行动态注入命令）。
	poster.tasks[a.sCtx.HandoffTaskID] = &types.TaskSnapshot{ID: a.sCtx.HandoffTaskID, Status: types.TaskDone, Result: []byte("found it")}
	res, _, err = a.tryForkSkill(ctx, "research", nil, types.TaintLow)
	if err != nil || string(res.Output) != "found it" || forker.renders != 1 {
		t.Fatalf("resume: %+v %v renders=%d", res, err, forker.renders)
	}

	// 不能再委派的子 Agent 内按普通技能内联执行。
	a.SetAgentProfile(&types.AgentProfileSpec{Name: "sub"})
	if _, handled, _ := a.tryForkSkill(ctx, "research", nil, types.TaintLow); handled {
		t.Fatal("subagents run fork skills inline")
	}
}
