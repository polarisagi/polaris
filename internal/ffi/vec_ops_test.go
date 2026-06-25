package ffi

import (
	"math"
	"testing"
)

func TestVecCosinePureGo_Identical(t *testing.T) {
	a := []float32{1, 2, 3}
	r := vecCosinePureGo(a, a)
	if math.Abs(float64(r)-1.0) > 1e-5 {
		t.Fatalf("got %v, want ~1.0", r)
	}
}

func TestVecCosinePureGo_Orthogonal(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	r := vecCosinePureGo(a, b)
	if math.Abs(float64(r)) > 1e-5 {
		t.Fatalf("got %v, want ~0.0", r)
	}
}

func TestVecCosineF32_EmptyInput(t *testing.T) {
	if VecCosineF32(nil, nil) != 0 {
		t.Fatal("nil input should return 0")
	}
	if VecCosineF32([]float32{1}, []float32{1, 2}) != 0 {
		t.Fatal("length mismatch should return 0")
	}
}
