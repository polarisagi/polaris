// Package policy — Cedar 策略引擎 purego 桥接。
// 历史: 原 cgo 实现已按 ADR-0011 Phase 2 迁移到 purego。
// 架构文档: docs/arch/M11-Policy-Safety.md §3
package policy

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	sffi "github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// Cedar dylib 函数指针——`bindCedar` 通过 sync.Once 懒绑定。
var (
	cedarOnce sync.Once
	cedarErr  error

	cedarLoadPolicies func(ptr uintptr, length uintptr, outErrPtr *uintptr, outErrLen *uintptr) int32
	cedarEvaluate     func(pPtr, pLen, aPtr, aLen, rPtr, rLen, ctxPtr, ctxLen uintptr, outPtr *uintptr, outLen *uintptr) int32
	cedarPolicyCount  func() int32
	cedarFreeBytes    func(ptr uintptr, length uintptr)
)

// bindCedar 加载 substrate dylib（共享）并绑定 cedar_* 函数指针。
// 幂等。失败时返回 error；后续调用沿用首次错误，避免重复尝试加载。
func bindCedar() error {
	cedarOnce.Do(func() {
		lib, err := sffi.Load()
		if err != nil {
			cedarErr = err
			return
		}
		purego.RegisterLibFunc(&cedarLoadPolicies, lib, "cedar_load_policies")
		purego.RegisterLibFunc(&cedarEvaluate, lib, "cedar_evaluate")
		purego.RegisterLibFunc(&cedarPolicyCount, lib, "cedar_policy_count")
		purego.RegisterLibFunc(&cedarFreeBytes, lib, "cedar_free_bytes")
	})
	return cedarErr
}

// strToBytes 将 string 转为 []byte（NUL-free），返回 ptr+len 供 FFI 使用。
// 调用方必须在 FFI 调用结束后执行 runtime.KeepAlive(返回的 []byte)。
func strToBytes(s string) ([]byte, uintptr, uintptr) {
	if len(s) == 0 {
		return nil, 0, 0
	}
	b := []byte(s)
	return b, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b))
}

// readBytesAndFree 读取 Rust 返回的 (ptr, len) 字节串并立即释放。
// 严格遵循 ADR-0011 "立即拷贝 + 立即归还" 模式。
func readBytesAndFree(ptr, length uintptr) string {
	if ptr == 0 || length == 0 {
		return ""
	}
	b := make([]byte, length)
	for i := uintptr(0); i < length; i++ {
		b[i] = *(*byte)(unsafe.Pointer(ptr + i))
	}
	cedarFreeBytes(ptr, length)
	return string(b)
}

// CedarEngine 封装 Cedar 策略引擎的 FFI 调用。
// 替代原 cgo 实现；接口与原 (CedarEngine).{LoadPolicies,Evaluate,PolicyCount} 完全一致。
type CedarEngine struct{}

func NewCedarEngine() *CedarEngine {
	return &CedarEngine{}
}

// LoadPolicies 加载 Cedar 策略集合（替换全局 PolicyStore）。
func (e *CedarEngine) LoadPolicies(policies string) error {
	if err := bindCedar(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cedar load lib", err)
	}
	bPolicies, pPtr, pLen := strToBytes(policies)
	var outErrPtr, outErrLen uintptr
	rc := cedarLoadPolicies(pPtr, pLen, &outErrPtr, &outErrLen)
	runtime.KeepAlive(bPolicies)
	msg := readBytesAndFree(outErrPtr, outErrLen)
	if rc != 0 {
		if msg == "" {
			msg = "unknown error"
		}
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("cedar_load_policies failed (code %d): %s", rc, msg))
	}
	return nil
}

// Evaluate 评估 Cedar 策略。返回 (是否允许, 判定理由, 错误)。
// 参数为合法 Cedar EntityUID 格式（如 `User::"alice"`）；ctx 经 JSON 序列化传入。
func (e *CedarEngine) Evaluate(principal, action, resource string, ctx map[string]any) (bool, string, error) {
	if err := bindCedar(); err != nil {
		return false, "", apperr.Wrap(apperr.CodeInternal, "cedar load lib", err)
	}
	if ctx == nil {
		ctx = map[string]any{}
	}
	ctxBytes, err := json.Marshal(ctx)
	if err != nil {
		return false, "", apperr.Wrap(apperr.CodeInternal, "context json marshal", err)
	}

	bPrincipal, pPtr, pLen := strToBytes(principal)
	bAction, aPtr, aLen := strToBytes(action)
	bResource, rPtr, rLen := strToBytes(resource)
	ctxStr := string(ctxBytes)
	bCtx, ctxPtr, ctxLen := strToBytes(ctxStr)
	var outPtr, outLen uintptr
	rc := cedarEvaluate(pPtr, pLen, aPtr, aLen, rPtr, rLen, ctxPtr, ctxLen, &outPtr, &outLen)
	runtime.KeepAlive(bPrincipal)
	runtime.KeepAlive(bAction)
	runtime.KeepAlive(bResource)
	runtime.KeepAlive(bCtx)
	reason := readBytesAndFree(outPtr, outLen)
	switch rc {
	case 0:
		return true, reason, nil
	case 1:
		return false, reason, nil
	default:
		return false, reason, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("cedar_evaluate internal error: code %d, reason: %s", rc, reason))
	}
}

// PolicyCount 返回当前加载的策略数量。加载失败时返回 0。
func (e *CedarEngine) PolicyCount() int {
	if err := bindCedar(); err != nil {
		return 0
	}
	return int(cedarPolicyCount())
}
