//go:build wasip1

// Package sdk 是 Go skill 编写者面向 Polaris Wasm 运行时的客户端库。
// 编译目标: GOARCH=wasm GOOS=wasip1
//
// ABI 约定（宿主调用顺序）:
//  1. 宿主通过 polaris_malloc 在 Wasm 线性内存中分配输入缓冲
//  2. 宿主将 JSON 输入写入该缓冲区
//  3. 宿主调用 run(ptr, length) → packed uint64（高32=输出ptr，低32=输出len）
//  4. 宿主读取输出并调用 polaris_free 释放
package sdk

import (
	"unsafe"
)

// retain 持有所有由 polaris_malloc 分配的字节切片，防止 GC 回收。
// key 为切片起始地址（uint32 截断自 uintptr），与宿主侧线性内存偏移一致。
//nolint:gochecknoglobals // Wasm 模块实例隔离，单线程运行，允许全局状态
var retain = map[uint32][]byte{}

// activeHandler 由 Register 设置，run 调用时执行。
//nolint:gochecknoglobals // 见上
var activeHandler func(input []byte) ([]byte, error)

// pack 将 (ptr, length) 编码为 uint64：高 32 位存 ptr，低 32 位存 length。
func pack(ptr, length uint32) uint64 {
	return uint64(ptr)<<32 | uint64(length)
}

// polaris_malloc 在 Go 堆（即 Wasm 线性内存）分配 size 字节，返回指针地址。
// size == 0 时返回 0（不分配）。
//
//export polaris_malloc
func polaris_malloc(size uint32) uint32 {
	if size == 0 {
		return 0
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(&buf[0])))
	retain[ptr] = buf
	return ptr
}

// polaris_free 释放由 polaris_malloc 分配的缓冲区，允许 GC 回收。
//
//export polaris_free
func polaris_free(ptr uint32) {
	delete(retain, ptr)
}

// Register 注册 skill 处理函数。每个 Wasm skill 模块在 init() 或 main() 中
// 调用一次 Register，后续宿主通过 run 触发执行。
func Register(handler func(input []byte) ([]byte, error)) {
	activeHandler = handler
}

// run 是宿主侧的主调用入口（exported）。
//
// ptr/length 描述宿主已写入的输入缓冲区。ptr == 0 时视为空输入（nil）。
// 返回 pack(outPtr, outLen)：宿主通过 outPtr 读取 JSON 输出，读完后调用
// polaris_free(outPtr) 释放。handler 返回 error 时输出为空（返回 0）。
//
//export run
func run(ptr, length uint32) uint64 {
	if activeHandler == nil {
		return 0
	}

	// 从线性内存构造输入切片（零拷贝）
	var inBuf []byte
	if ptr != 0 && length > 0 {
		inBuf = unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)
	}

	out, err := activeHandler(inBuf)
	if err != nil || len(out) == 0 {
		return 0
	}

	// 分配输出缓冲并写入，交由宿主读取后 polaris_free
	outPtr := polaris_malloc(uint32(len(out)))
	if outPtr == 0 {
		return 0
	}
	copy(retain[outPtr], out)
	return pack(outPtr, uint32(len(out)))
}
