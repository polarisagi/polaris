package graphrag

import (
	"context"
	"testing"
)

func TestExtractEntitiesByPattern(t *testing.T) {
	text := "This is a test for PolarisEngine version 1.2.3 running on api.example.com and locating at usr/bin/local."
	entities := extractEntitiesByPattern(text)
	if len(entities) == 0 {
		t.Fatalf("expected some entities")
	}

	foundVersion := false
	foundDomain := false
	foundPath := false
	foundPascal := false

	for _, e := range entities {
		if e.Type == "version" && e.Name == "1.2.3" {
			foundVersion = true
		}
		if e.Type == "domain" && e.Name == "api.example.com" {
			foundDomain = true
		}
		if e.Type == "path" && e.Name == "usr/bin/local" {
			foundPath = true
		}
		if e.Type == "identifier" && e.Name == "PolarisEngine" {
			foundPascal = true
		}
	}

	if !foundVersion || !foundDomain || !foundPascal || !foundPath {
		t.Errorf("missing some entity types: version=%v domain=%v pascal=%v path=%v", foundVersion, foundDomain, foundPascal, foundPath)
	}
}

func TestEntityDictMatcher(t *testing.T) {
	matcher := &EntityDictMatcher{
		exactMap: map[string]*Entity{"test": {Name: "Test"}},
		fuzzyMap: map[string][]*Entity{"example": {{Name: "Example"}}},
	}

	rate := matcher.Match("test exact match")
	if rate == 0 {
		t.Errorf("expected > 0 hit rate")
	}

	rateFuzzy := matcher.Match("exampel match")
	if rateFuzzy == 0 {
		t.Errorf("expected > 0 fuzzy hit rate")
	}

	entities := matcher.GetKnownEntities()
	if len(entities) != 1 {
		t.Errorf("expected 1 exact entity")
	}
}

func TestTFIDFFilter(t *testing.T) {
	filter := &TFIDFFilter{
		idfWeights: map[string]float64{"test": 1.5, "novel": 2.0},
		centroid:   []float32{0.5, 0.5},
	}

	score := filter.NoveltyScore("test novel")
	if score <= 0 {
		t.Errorf("expected > 0 novelty score")
	}

	emptyScore := filter.NoveltyScore("")
	if emptyScore != 0.5 {
		t.Errorf("expected 0.5 for empty")
	}
}

func TestEntityExtractor_Extract(t *testing.T) {
	ctx := context.Background()
	matcher := &EntityDictMatcher{
		exactMap: map[string]*Entity{"Test": {Name: "Test", Type: "concept"}},
	}
	ee := &EntityExtractor{
		dictMatcher: matcher,
		tfidfFilter: &TFIDFFilter{},
	}

	entities, err := ee.Extract(ctx, "Test exact")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(entities) != 1 || entities[0].Name != "Test" {
		t.Errorf("expected Test from dict")
	}
}

func TestRelationExtractor_Extract(t *testing.T) {
	ctx := context.Background()
	re := &RelationExtractor{}

	entities := []*Entity{
		{ID: "e1", Name: "e1"},
		{ID: "e2", Name: "e2"},
	}

	rels, err := re.Extract(ctx, entities)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("expected 1 fallback relation")
	}
	if rels[0].RelationType != "uses" {
		t.Errorf("expected uses, got %s", rels[0].RelationType)
	}
}
