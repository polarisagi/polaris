package graphrag

import (
	"context"
	"testing"
)

func TestCosineSimilarity_Extra(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{1, 0}
	if CosineSimilarity(a, b) != 1.0 {
		t.Error("expected 1.0")
	}
	if CosineSimilarity([]float32{1, 0}, []float32{0, 1}) != 0 {
		t.Error("expected 0.0")
	}
	if CosineSimilarity([]float32{}, []float32{}) != 0 {
		t.Error("expected 0.0 for empty")
	}
}

func TestRandomProjection(t *testing.T) {
	rp := &RandomProjection{
		projectionMatrix: [][]float64{{1.0, 0.0}, {0.0, 1.0}},
	}
	res := rp.Project([]float32{3.0, 4.0})
	if len(res) != 2 || res[0] != 3.0 || res[1] != 4.0 {
		t.Errorf("unexpected project result: %v", res)
	}
}

func TestClusterer_ClusterEntities(t *testing.T) {
	c := NewClusterer(0) // Mini-batch K-Means
	emb := [][]float32{
		{1, 0},
		{0, 1},
		{1, 0.1},
	}
	labels := c.ClusterEntities(emb)
	if len(labels) != 3 {
		t.Fatalf("expected 3 labels, got %d", len(labels))
	}

	_ = c.kmeans.GetK()
	_ = c.kmeans.GetCenters()

	c1 := NewClusterer(1) // DBSCAN
	labels1 := c1.ClusterEntities(emb)
	if len(labels1) != 3 {
		t.Fatalf("expected 3 labels, got %d", len(labels1))
	}
}

func TestBIRCH(t *testing.T) {
	b := &BIRCH{}
	b.Insert([]float64{1.0, 2.0})
	if b.cfTree == nil || len(b.cfTree.entries) != 1 {
		t.Fatalf("expected 1 entry")
	}
}

func TestLeidenDetector(t *testing.T) {
	ld := &LeidenDetector{}
	adj := [][]float64{
		{0, 1, 0},
		{1, 0, 1},
		{0, 1, 0},
	}
	labels := ld.DetectCommunities(adj)
	if len(labels) != 3 {
		t.Fatalf("expected 3 labels")
	}
}

func TestClusterer_Cluster(t *testing.T) {
	ctx := context.Background()
	c := NewClusterer(1)

	adj := [][]float64{
		{0, 1},
		{1, 0},
	}
	entities := []*Entity{
		{ID: "e1", Name: "e1"},
		{ID: "e2", Name: "e2"},
	}

	labels, err := c.Cluster(ctx, nil, entities, adj)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("expected 2 labels")
	}
}
