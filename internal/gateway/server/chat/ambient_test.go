package chat

import (
	"testing"
)

type mockEmbedder struct {
	callCount int
	retVec    []float32
}

func (m *mockEmbedder) Embed(text string) []float32 {
	m.callCount++
	return m.retVec
}

func TestAmbientSkill(t *testing.T) {
	query := "hello"
	name := "test"
	desc := "test"
	inst := "test"

	// test a) Embedder == nil
	s := &ChatHandler{DataDir: t.TempDir(),
		Embedder: nil,
	}
	// relevanceScore is 0 for these texts
	if s.isSkillRelevant(nil, query, name, desc, inst) != false {
		t.Fatalf("expected false for nil embedder with 0 token overlap")
	}

	// test b) Embedder != nil but queryVec is nil
	s2 := &ChatHandler{DataDir: t.TempDir(),
		Embedder: &mockEmbedder{retVec: nil},
	}
	if s2.isSkillRelevant(nil, query, name, desc, inst) != false {
		t.Fatalf("expected false when queryVec is nil")
	}

	// test c) Embedder normal
	me := &mockEmbedder{retVec: []float32{1.0, 0.0}}
	s3 := &ChatHandler{DataDir: t.TempDir(),
		Embedder:       me,
		EmbedThreshold: 0.60,
	}
	// cachedSkillEmbed will be called and return me.retVec
	queryVec := []float32{1.0, 0.0}
	if s3.isSkillRelevant(queryVec, query, name, desc, inst) != true {
		t.Fatalf("expected true for identical vectors")
	}

	// cachedSkillEmbed should cache the vector
	me.retVec = []float32{0.0, 1.0} // change the return vector, but cache should be hit
	if s3.isSkillRelevant(queryVec, query, name, desc, inst) != true {
		t.Fatalf("expected true due to cache hit")
	}
}
