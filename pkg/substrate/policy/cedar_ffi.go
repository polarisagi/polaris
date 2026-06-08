// Package policy — Cedar 策略引擎 purego 桥接。
// 历史: 原 cgo 实现已按 ADR-0011 Phase 2 迁移到 purego。
// 架构文档: docs/arch/M11-Policy-Safety.md §3
package policy

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	perrors "github.com/polarisagi/polaris/internal/errors"
	sffi "github.com/polarisagi/polaris/pkg/substrate/ffi"
)

// Cedar dylib 函数指针——`bindCedar` 通过 sync.Once 懒绑定。
var (
	cedarOnce sync.Once
	cedarErr  error

	cedarLoadPolicies func(text string, outErr *uintptr) int32
	cedarEvaluate     func(p, a, r, ctx string, outReason *uintptr) int32
	cedarPolicyCount  func() int32
	cedarFreeString   func(ptr uintptr)
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
		purego.RegisterLibFunc(&cedarFreeString, lib, "cedar_free_string")
	})
	return cedarErr
}

// readCStringAndFree 读取 NUL-terminated C 字符串并立即调用 cedar_free_string 归还。
// 严格遵循 ADR-0011 风险段"立即拷贝 + 立即归还"模式，防 use-after-free。
func readCStringAndFree(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	s := goStringFromPtr(ptr)
	cedarFreeString(ptr)
	return s
}

// goStringFromPtr 从 uintptr 指向的 C 字符串拷贝出 Go string（按 NUL 终结）。
// 注: purego 已支持 string 入参自动转换；此函数用于出参方向。
func goStringFromPtr(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	var n uintptr
	for {
		b := *(*byte)(unsafe.Pointer(ptr + n))
		if b == 0 {
			break
		}
		n++
	}
	if n == 0 {
		return ""
	}
	bytes := make([]byte, n)
	for i := uintptr(0); i < n; i++ {
		bytes[i] = *(*byte)(unsafe.Pointer(ptr + i))
	}
	return string(bytes)
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
		return perrors.Wrap(perrors.CodeInternal, "cedar load lib", err)
	}
	var outErr uintptr
	rc := cedarLoadPolicies(policies, &outErr)
	if rc != 0 {
		msg := readCStringAndFree(outErr)
		if msg == "" {
			msg = "unknown error"
		}
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("cedar_load_policies failed (code %d): %s", rc, msg))
	}
	// 成功路径下 outErr 应为 0；防御性归还以防 Rust 侧未来变更
	if outErr != 0 {
		cedarFreeString(outErr)
	}
	return nil
}

// Evaluate 评估 Cedar 策略。返回 (是否允许, 判定理由, 错误)。
// 参数为合法 Cedar EntityUID 格式（如 `User::"alice"`）；ctx 经 JSON 序列化传入。
func (e *CedarEngine) Evaluate(principal, action, resource string, ctx map[string]any) (bool, string, error) {
	if err := bindCedar(); err != nil {
		return false, "", perrors.Wrap(perrors.CodeInternal, "cedar load lib", err)
	}
	if ctx == nil {
		ctx = map[string]any{}
	}
	ctxBytes, err := json.Marshal(ctx)
	if err != nil {
		return false, "", perrors.Wrap(perrors.CodeInternal, "context json marshal", err)
	}

	var outReason uintptr
	rc := cedarEvaluate(principal, action, resource, string(ctxBytes), &outReason)
	reason := readCStringAndFree(outReason)
	switch rc {
	case 0:
		return true, reason, nil
	case 1:
		return false, reason, nil
	default:
		return false, reason, perrors.New(perrors.CodeInternal,
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
