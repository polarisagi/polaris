package adapter

import "testing"

func TestControlVectorStore_ImportGetDelete(t *testing.T) {
	s := NewControlVectorStore()

	if _, ok := s.Get("missing"); ok {
		t.Fatal("expected empty store to report missing label as not found")
	}

	s.Import("polite", []float32{0.1, 0.2, 0.3}, 0) // layer<=0 → default 15
	cv, ok := s.Get("polite")
	if !ok {
		t.Fatal("expected polite to be registered")
	}
	if cv.Layer != 15 {
		t.Errorf("expected default layer 15, got %d", cv.Layer)
	}
	if len(cv.Vector) != 3 {
		t.Errorf("expected vector len 3, got %d", len(cv.Vector))
	}

	s.Import("terse", []float32{1, 2}, 20)
	if got := s.List(); len(got) != 2 || got[0] != "polite" || got[1] != "terse" {
		t.Fatalf("expected sorted [polite terse], got %v", got)
	}

	if !s.Delete("polite") {
		t.Fatal("expected Delete to report true for existing label")
	}
	if s.Delete("polite") {
		t.Fatal("expected second Delete of same label to report false")
	}
	if _, ok := s.Get("polite"); ok {
		t.Fatal("expected polite to be gone after Delete")
	}
	if got := s.List(); len(got) != 1 || got[0] != "terse" {
		t.Fatalf("expected only [terse] remaining, got %v", got)
	}
}

func TestControlVectorStore_ImportOverwrites(t *testing.T) {
	s := NewControlVectorStore()
	s.Import("label", []float32{1}, 5)
	s.Import("label", []float32{1, 2, 3}, 10)

	cv, ok := s.Get("label")
	if !ok || cv.Layer != 10 || len(cv.Vector) != 3 {
		t.Fatalf("expected re-import to overwrite: %+v ok=%v", cv, ok)
	}
}
