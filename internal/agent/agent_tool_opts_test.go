package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// stubCatalog 只实现 Schemas；其余方法本测试不会触达。
type stubCatalog struct{ schemas []types.ToolSchema }

func (s *stubCatalog) List(context.Context, types.TrustTier) []protocol.CatalogEntry { return nil }
func (s *stubCatalog) Lookup(string) (protocol.CatalogEntry, bool) {
	return protocol.CatalogEntry{}, false
}
func (s *stubCatalog) Register(protocol.CatalogEntry) {}
func (s *stubCatalog) Unregister(string)              {}
func (s *stubCatalog) Invalidate()                    {}
func (s *stubCatalog) Schemas(context.Context, types.TrustTier) []types.ToolSchema {
	return s.schemas
}

func applyOpts(opts []types.InferOption) types.InferOptions {
	var o types.InferOptions
	for _, f := range opts {
		f(&o)
	}
	return o
}

func TestStateWantsTools(t *testing.T) {
	cases := []struct {
		state          types.AgentState
		uniform        bool
		attach, forbid bool
		name           string
	}{
		{types.AgentStatePlan, false, true, false, "plan/off"},
		{types.AgentStatePlan, true, true, false, "plan/on"},
		{types.AgentStatePerceive, false, false, false, "perceive/off"},
		{types.AgentStatePerceive, true, true, true, "perceive/on"},
		{types.AgentStateReflect, false, false, false, "reflect/off"},
		{types.AgentStateReflect, true, true, true, "reflect/on"},
		{types.AgentStateRespond, false, false, false, "respond/off"},
		{types.AgentStateRespond, true, true, true, "respond/on"},
		{types.AgentStateValidate, true, false, false, "validate 从不携带 tools"},
	}
	for _, c := range cases {
		attach, forbid := stateWantsTools(c.state, c.uniform)
		if attach != c.attach || forbid != c.forbid {
			t.Errorf("%s: got (%v,%v) want (%v,%v)", c.name, attach, forbid, c.attach, c.forbid)
		}
	}
}

// 开关为假：仅 Plan 有 tools 且不设 ToolChoice，其余阶段无任何 tools 选项（与现状一致）。
// 开关为真：Perceive/Reflect/Respond 下发与 Plan 相同的 tools，并设 ToolChoice=none。
func TestToolInferOptions_UniformToolsSwitch(t *testing.T) {
	tools := []types.ToolSchema{{Name: "a"}, {Name: "b"}}
	a := &Agent{sCtx: &fsm.StateContext{SessionID: "s"}, catalog: &stubCatalog{schemas: tools}}
	ctx := context.Background()

	plan := applyOpts(a.toolInferOptions(ctx, types.AgentStatePlan, false))
	if len(plan.Tools) != 2 || plan.ToolChoice != "" {
		t.Fatalf("Plan(off) 应带 tools 且不设 ToolChoice：%+v", plan)
	}
	for _, st := range []types.AgentState{types.AgentStatePerceive, types.AgentStateReflect, types.AgentStateRespond} {
		if got := a.toolInferOptions(ctx, st, false); got != nil {
			t.Fatalf("开关关闭时 %v 不应有 tools 选项", st)
		}
		on := applyOpts(a.toolInferOptions(ctx, st, true))
		if len(on.Tools) != 2 || on.Tools[0].Name != plan.Tools[0].Name || on.Tools[1].Name != plan.Tools[1].Name {
			t.Fatalf("开关开启时 %v 的 tools 必须与 Plan 一致：%+v", st, on.Tools)
		}
		if on.ToolChoice != "none" {
			t.Fatalf("开关开启时 %v 必须 ToolChoice=none：%q", st, on.ToolChoice)
		}
	}
	// Plan 在开关开启时不禁止调用工具。
	if p := applyOpts(a.toolInferOptions(ctx, types.AgentStatePlan, true)); p.ToolChoice != "" {
		t.Fatalf("Plan 必须允许调用工具：%q", p.ToolChoice)
	}
}

// 目录为空或无 schema：任何状态、任何开关都不发空 tools，也不发孤立的 tool_choice。
func TestToolInferOptions_NoCatalogOrEmptySchemas(t *testing.T) {
	ctx := context.Background()
	noCat := &Agent{sCtx: &fsm.StateContext{SessionID: "s"}}
	empty := &Agent{sCtx: &fsm.StateContext{SessionID: "s"}, catalog: &stubCatalog{}}
	for _, a := range []*Agent{noCat, empty} {
		for _, uniform := range []bool{false, true} {
			if got := a.toolInferOptions(ctx, types.AgentStateRespond, uniform); got != nil {
				t.Fatalf("无 tools 时不应产生选项：%v", got)
			}
		}
	}
}
