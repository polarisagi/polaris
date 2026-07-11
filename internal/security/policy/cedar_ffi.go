// Package policy — Cedar 策略引擎 purego 桥接。
// 历史: 原 cgo 实现已按 ADR-0011 Phase 2 迁移到 purego。
// 架构文档: docs/arch/M11-Policy-Safety.md §3
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	sffi "github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// cedarFFITarget 是 InstrFFILatencyMs/InstrFFIErrorTotal 的 ffi_target label 值。
const cedarFFITarget = "cedar"

// Cedar dylib 函数指针——`bindCedar` 通过 sync.Once 懒绑定。
var (
	cedarOnce sync.Once
	cedarErr  error

	cedarLoadPolicies func(ptr uintptr, length uintptr, timeoutMs uint64, outErrPtr *uintptr, outErrLen *uintptr) int32 //nolint:gochecknoglobals // purego 绑定函数指针，sync.Once 幂等懒绑定，架构性必需（同包 cedarEvaluate/cedarFreeBytes 同类声明先例）
	cedarEvaluate     func(pPtr, pLen, aPtr, aLen, rPtr, rLen, ctxPtr, ctxLen uintptr, timeoutMs uint64, outPtr *uintptr, outLen *uintptr) int32
	cedarPolicyCount  func(timeoutMs uint64) int32 //nolint:gochecknoglobals // 同上
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

// cedarAdminOpTimeoutMs 是 SyncPolicies/PolicyCount 这类低频管理操作的取锁超时
// 预算（GR-7.2）。二者不在 FSM 决策热路径上（不同于 cedar_evaluate 的 10ms 级
// 严格预算），给一个宽松但有界的值，与 internal/sandbox.SandboxSpec.CPUQuotaMs
// "0 = 默认 5000ms" 的既有仓库惯例保持一致量级。
const cedarAdminOpTimeoutMs uint64 = 5000

// SyncPolicies 加载 Cedar 策略集合（替换全局 PolicyStore）。
func (e *CedarEngine) SyncPolicies(policies string) error {
	if err := bindCedar(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cedar load lib", err)
	}
	bPolicies, pPtr, pLen := strToBytes(policies)
	var outErrPtr, outErrLen uintptr
	rc := cedarLoadPolicies(pPtr, pLen, cedarAdminOpTimeoutMs, &outErrPtr, &outErrLen)
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

// Evaluate 执行策略引擎查询。如果超时，返回 err 并携带超时信息。
//
// 2026-07-04 审计修复（Task 14）：接入 InstrFFILatencyMs/InstrFFIErrorTotal
// （ffi_target=cedar）。allow(rc=0)/deny(rc=1) 是正常的策略裁决结果，不计入
// FFI 失败率；仅 timeout(rc=-5) 与其余非法 rc 视为 FFI 层调用失败。
func (e *CedarEngine) Evaluate(principal, action, resource string, ctx map[string]any, timeoutMs uint64) (allowed bool, reasonOut string, err error) {
	start := time.Now()
	defer func() {
		var ffiErr error
		if err != nil {
			ffiErr = err
		}
		metrics.RecordFFICall(context.Background(), cedarFFITarget, float64(time.Since(start).Milliseconds()), ffiErr)
	}()

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
	rc := cedarEvaluate(pPtr, pLen, aPtr, aLen, rPtr, rLen, ctxPtr, ctxLen, timeoutMs, &outPtr, &outLen)
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
	case -5: // CEDAR_ERR_TIMEOUT
		return false, reason, apperr.New(apperr.CodeInternal, "cedar_evaluate timeout (>10ms)")
	default:
		return false, reason, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("cedar_evaluate internal error: code %d, reason: %s", rc, reason))
	}
}

// PolicyCount 返回当前加载的策略数量。加载/取锁失败（含超时）时返回 0。
//
// GR-7.2 修复：此前直接 `return int(cedarPolicyCount())`，未检查负数错误码——
// cedar_policy_count 原实现在锁中毒时返回 -1，会被这里悄悄当成"策略数为 -1"
// 返回给调用方，而不是按文档承诺的"失败时返回 0"降级。现在锁获取新增超时
// （CEDAR_ERR_TIMEOUT=-5）之后这个漏判会更容易触发，一并修正。
func (e *CedarEngine) PolicyCount() int {
	if err := bindCedar(); err != nil {
		return 0
	}
	rc := cedarPolicyCount(cedarAdminOpTimeoutMs)
	if rc < 0 {
		return 0
	}
	return int(rc)
}
