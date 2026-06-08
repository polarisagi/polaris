//go:build wasip1

package sdk

import (
	"unsafe"
)

// retain 防止 GC 回收分配给宿主的内存块
var retain = make(map[uint32][]byte)

//go:wasmexport polaris_malloc
func polaris_malloc(size uint32) uint32 {
	if size == 0 {
		return 0
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buf))))
	retain[ptr] = buf
	return ptr
}

//go:wasmexport polaris_free
func polaris_free(ptr uint32) {
	delete(retain, ptr)
}

// Handler 是技能执行器的函数签名
type Handler func(input []byte) ([]byte, error)

var activeHandler Handler

// Register 注册当前 Wasm 模块的技能处理函数
func Register(h Handler) {
	activeHandler = h
}

//go:wasmexport run
func run(ptr uint32, length uint32) uint64 {
	if activeHandler == nil {
		return pack(0, 0)
	}

	// 还原输入切片
	var inBuf []byte
	if ptr != 0 && length > 0 {
		var dummy *byte
		base := unsafe.Pointer(dummy)
		addr := unsafe.Add(base, uintptr(ptr))
		inBuf = unsafe.Slice((*byte)(addr), length)
	}

	// 执行技能
	outBytes, err := activeHandler(inBuf)
	if err != nil {
		// 返回简单的错误 JSON
		outBytes = []byte(`{"error": "` + err.Error() + `"}`)
	}

	// 将输出写入新分配的 Wasm 内存
	outLen := uint32(len(outBytes))
	if outLen == 0 {
		return pack(0, 0)
	}

	outPtr := polaris_malloc(outLen)
	copy(retain[outPtr], outBytes)

	return pack(outPtr, outLen)
}

func pack(ptr, length uint32) uint64 {
	return (uint64(ptr) << 32) | uint64(length)
}
