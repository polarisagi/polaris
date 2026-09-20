package synthetic

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeSkillReg struct {
	skills map[string]types.SkillMeta
}

func (f *fakeSkillReg) Register(_ context.Context, m types.SkillMeta) error {
	f.skills[m.Name] = m
	return nil
}
func (f *fakeSkillReg) Get(_ context.Context, name, _ string) (*types.SkillMeta, error) {
	if m, ok := f.skills[name]; ok {
		return &m, nil
	}
	return nil, apperr.New(apperr.CodeNotFound, "nf")
}
func (f *fakeSkillReg) List(context.Context, types.SkillFilter) ([]types.SkillMeta, error) {
	return nil, nil
}
func (f *fakeSkillReg) Deprecate(context.Context, string, string, string) error { return nil }

func llmReturning(content string) *MockProvider {
	return &MockProvider{
		InferFunc: func(context.Context, []types.Message, ...types.InferOption) (*types.ProviderResponse, error) {
			return &types.ProviderResponse{Content: content}, nil
		},
	}
}

// GR-7.1-004：合成技能只能以待审候选（Deprecated）落库，工具名不得取自 LLM
// 自拟名字，且不得覆盖已存在的同名技能。
func TestSyntheticSkill_PersistedAsPendingCandidate(t *testing.T) {
	ctx := context.Background()
	reg := &fakeSkillReg{skills: map[string]types.SkillMeta{}}
	prov := llmReturning(`{"name":"read_file","description":"d","input_schema":{"type":"object"}}`)
	tool, err := NewSyntheticSkillGen(prov, reg).Generate(ctx, "gap_tool", "d")
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name != "gap_tool" {
		t.Fatalf("tool name taken from LLM output: %q", tool.Name)
	}
	if tool.TrustTier != types.TrustUntrusted || tool.SandboxTier == types.SandboxInProcess {
		t.Fatalf("synthesized tool must be untrusted and not pinned in-process: %+v", tool)
	}
	m, ok := reg.skills["skill:gap_tool"]
	if !ok || !m.Deprecated {
		t.Fatalf("candidate must be persisted as deprecated (pending review): %+v ok=%v", m, ok)
	}

	reg.skills["skill:trusted"] = types.SkillMeta{Name: "skill:trusted", Instructions: "original"}
	if _, err := NewSyntheticSkillGen(prov, reg).Generate(ctx, "trusted", "d"); err != nil {
		t.Fatal(err)
	}
	if reg.skills["skill:trusted"].Instructions != "original" {
		t.Fatal("existing skill overwritten by synthesized candidate")
	}
}
