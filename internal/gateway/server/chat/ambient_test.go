package chat

import (
	"context"
	"testing"
)

type mockEmbedder struct {
	callCount int
	retVec    []float32
}

func (m *mockEmbedder) Embed(_ context.Context, text string) []float32 {
	m.callCount++
	return m.retVec
}

func TestAmbientSkill(t *testing.T) {
	query := "hello"
	name := "test"
	desc := "test"
	inst := "test"

	// test a) Embedder == nil
	s := &PromptAssemblyService{
		Embedder: nil,
	}
	// relevanceScore is 0 for these texts
	if s.isSkillRelevant(context.Background(), nil, query, ambientSkill{name: name, desc: desc, inst: inst}) != false {
		t.Fatalf("expected false for nil embedder with 0 token overlap")
	}

	// test b) Embedder != nil but queryVec is nil
	s2 := &PromptAssemblyService{
		Embedder: &mockEmbedder{retVec: nil},
	}
	if s2.isSkillRelevant(context.Background(), nil, query, ambientSkill{name: name, desc: desc, inst: inst}) != false {
		t.Fatalf("expected false when queryVec is nil")
	}

	// test c) Embedder normal
	me := &mockEmbedder{retVec: []float32{1.0, 0.0}}
	s3 := &PromptAssemblyService{
		Embedder:       me,
		EmbedThreshold: 0.60,
	}
	// cachedSkillEmbed will be called and return me.retVec
	queryVec := []float32{1.0, 0.0}
	if s3.isSkillRelevant(context.Background(), queryVec, query, ambientSkill{name: name, desc: desc, inst: inst}) != true {
		t.Fatalf("expected true for identical vectors")
	}

	// cachedSkillEmbed should cache the vector
	me.retVec = []float32{0.0, 1.0} // change the return vector, but cache should be hit
	if s3.isSkillRelevant(context.Background(), queryVec, query, ambientSkill{name: name, desc: desc, inst: inst}) != true {
		t.Fatalf("expected true due to cache hit")
	}
}
