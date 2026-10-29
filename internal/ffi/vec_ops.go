// Package ffi — vec_cosine_f32 purego 桥接。
// Rust SIMD 路径；dylib 未加载时降级纯 Go 实现（graphrag.CosineSimilarity 同逻辑）。
package ffi

import (
	"math"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

var (
	vcOnce sync.Once
	// vcFFI 在 dylib 成功加载后由 vcOnce 初始化。
	// 签名对应 Rust: fn(a_ptr *const f32, a_len usize, b_ptr *const f32, b_len usize) -> f32
	vcFFI func(aPtr unsafe.Pointer, aLen uint64, bPtr unsafe.Pointer, bLen uint64) float32
)

// VecCosineF32 计算两个 float32 向量的余弦相似度。
// dylib 已加载时走 Rust SIMD 路径（约 3-5× 快于纯 Go，对高维向量有意义）；
// 否则降级纯 Go 实现，不返回错误，保证调用方逻辑简洁。
func VecCosineF32(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	lib, err := Load()
	if err == nil {
		vcOnce.Do(func() {
			purego.RegisterLibFunc(&vcFFI, lib, "vec_cosine_f32")
		})
		if vcFFI != nil {
			return vcFFI(
				unsafe.Pointer(&a[0]), uint64(len(a)),
				unsafe.Pointer(&b[0]), uint64(len(b)),
			)
		}
	}
	return vecCosinePureGo(a, b)
}

// vecCosinePureGo 纯 Go 降级路径，与 graphrag.CosineSimilarity 逻辑一致。
func vecCosinePureGo(a, b []float32) float32 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if d := math.Sqrt(na) * math.Sqrt(nb); d != 0 {
		return float32(dot / d)
	}
	return 0
}
