package store

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestSurrealStore_BasicOps(t *testing.T) {
	s, err := OpenSurrealDBCore("mem", "", 3, 2)
	if err != nil {
		t.Fatalf("OpenSurrealDBCore: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// Get 不存在的 key → ErrNotFound
	_, err = s.Get(ctx, []byte("missing"))
	if err != apperr.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Put + Get
	if err := s.Put(ctx, []byte("hello"), []byte("world")); err != nil {
		t.Fatal(err)
	}
	val, err := s.Get(ctx, []byte("hello"))
	if err != nil || string(val) != "world" {
		t.Fatalf("Get after Put: err=%v val=%s", err, val)
	}

	// Delete + Get → ErrNotFound
	if err := s.Delete(ctx, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, err = s.Get(ctx, []byte("hello"))
	if err != apperr.ErrNotFound {
		t.Fatalf("expected ErrNotFound after Delete, got %v", err)
	}
}

func TestSurrealStore_Scan(t *testing.T) {
	s, _ := OpenSurrealDBCore("mem", "", 3, 2)
	defer s.Close()
	ctx := context.Background()

	for _, pair := range [][2]string{
		{"prefix/a", "1"}, {"prefix/b", "2"}, {"other/c", "3"},
	} {
		s.Put(ctx, []byte(pair[0]), []byte(pair[1]))
	}

	iter, err := s.Scan(ctx, []byte("prefix/"))
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()

	var keys []string
	for iter.Next() {
		keys = append(keys, string(iter.Key()))
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d: %v", len(keys), keys)
	}
}

func TestSurrealStore_Vector(t *testing.T) {
	s, _ := OpenSurrealDBCore("mem", "", 3, 2)
	defer s.Close()

	s.VecUpsert("vec1", []float32{1.0, 0.0, 0.0})
	s.VecUpsert("vec2", []float32{0.0, 1.0, 0.0})

	results, err := s.VecKNN([]float32{1.0, 0.1, 0.0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].ID != "vec1" {
		t.Fatalf("expected vec1 to be first, got %v", results)
	}

	s.VecDelete("vec1")
}

func TestSurrealStore_Graph(t *testing.T) {
	s, _ := OpenSurrealDBCore("mem", "", 3, 2)
	defer s.Close()

	s.GraphRelate("nodeA", "knows", "nodeB", 1.0)

	results, err := s.GraphTraverse("nodeA", "knows", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0] != "nodeB" {
		t.Fatalf("expected nodeB, got %v", results)
	}

	scored, err := s.GraphSpreadingActivation([]string{"nodeA"}, 2, 0.8, 0.1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(scored) == 0 || scored[0].ID != "nodeB" {
		t.Fatalf("expected nodeB, got %v", scored)
	}

	s.GraphDeleteEdges("nodeA", "knows")
}

func TestSurrealStore_FTS(t *testing.T) {
	s, _ := OpenSurrealDBCore("mem", "", 3, 2)
	defer s.Close()

	s.FTSIndex("doc1", "Hello world surreal")

	results, err := s.FTSSearch("world", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].ID != "doc1" {
		t.Fatalf("expected doc1, got %v", results)
	}

	s.FTSDelete("doc1")
}

func TestSurrealStore_Capabilities(t *testing.T) {
	s, _ := OpenSurrealDBCore("mem", "", 3, 2)
	defer s.Close()

	caps := s.Capabilities()
	if caps.SupportsSQL {
		t.Fatal("expected SupportsSQL=false")
	}
	if !caps.SupportsVector {
		t.Fatal("expected SupportsVector=true")
	}
	if caps.Engine != "surreal-core-ffi/hnsw" {
		t.Fatalf("expected engine surreal-core-ffi/hnsw, got %s", caps.Engine)
	}
}
