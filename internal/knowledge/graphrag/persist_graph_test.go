package graphrag

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type recSemMem struct {
	protocol.SemanticMemory
	ents map[string]*types.Entity
	rels []types.Relation
	next int64
}

func (m *recSemMem) UpsertFact(_ context.Context, e types.Entity, taint types.TaintLevel) error {
	k := e.Type + "/" + e.Name
	if _, ok := m.ents[k]; !ok {
		m.next++
		e.DBID = m.next
		e.TaintLevel = taint
		m.ents[k] = &e
	}
	return nil
}
func (m *recSemMem) GetEntity(_ context.Context, typ, name string) (*types.Entity, error) {
	return m.ents[typ+"/"+name], nil
}
func (m *recSemMem) UpsertRelation(_ context.Context, r types.Relation, _ types.TaintLevel) error {
	m.rels = append(m.rels, r)
	return nil
}

type fakeLLM struct{ gotText string }

func (f *fakeLLM) ExtractEntities(context.Context, string) ([]*Entity, error) {
	return []*Entity{{ID: "Polaris", Name: "Polaris", Type: "project"}, {ID: "Go", Name: "Go", Type: "tool"}}, nil
}
func (f *fakeLLM) ExtractRelations(_ context.Context, _ []*Entity, text string) ([]*Relation, error) {
	f.gotText = text
	return []*Relation{{FromEntityID: "Polaris", ToEntityID: "Go", RelationType: "uses"},
		{FromEntityID: "Polaris", ToEntityID: "Ghost", RelationType: "uses"}}, nil
}

// GR-7.2-004 + 连带发现：关系抽取必须拿到正文；Run 必须把实体与关系落库，
// 且不为不在实体集合中的端点建悬空边。
func TestGraphBuildPipeline_PersistsEntitiesAndRelations(t *testing.T) {
	mem := &recSemMem{ents: map[string]*types.Entity{}}
	llm := &fakeLLM{}
	p := NewGraphBuildPipeline(llm, 0, mem)
	p.SetDocFetcher(&mockDocFetcher{})
	if err := p.Run(context.Background(), "doc1"); err != nil {
		t.Fatal(err)
	}
	if llm.gotText != "Polaris is a good engine." {
		t.Fatalf("relation extraction got %q instead of document text", llm.gotText)
	}
	if len(mem.ents) < 2 {
		t.Fatalf("entities not persisted: %d", len(mem.ents))
	}
	if len(mem.rels) != 1 || mem.rels[0].FromDBID == 0 || mem.rels[0].ToDBID == 0 {
		t.Fatalf("expected exactly one resolved relation, got %+v", mem.rels)
	}
	for _, e := range mem.ents {
		if e.TaintLevel < types.TaintMedium {
			t.Fatalf("document entity persisted below TaintMedium: %+v", e)
		}
	}
}
