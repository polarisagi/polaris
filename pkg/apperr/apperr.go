// Package apperr 提供 Polaris 应用层统一错误类型。
//
// 所有模块在构造和返回错误时必须使用本包，禁止裸 error 泄漏调用链。
// 外部扩展（插件/CLI 工具/SDK）通过 errors.As 判断错误类型。
//
// 错误构造：
//
//	apperr.New(apperr.CodeNotFound, "session not found")
//	apperr.Wrap(apperr.CodeInternal, "db write failed", err)
//
// 错误判断（推荐用辅助函数，避免重复写 errors.As 模板）：
//
//	if apperr.IsCode(err, apperr.CodeNotFound) { ... }
//	code := apperr.CodeOf(err)   // 提取 Code，链中无 *Error 时返回 CodeInternal
//
// HTTP 状态码映射（供 gateway 统一转换）：
//
//	status := apperr.HTTPStatus(apperr.CodeOf(err))
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Code 错误分类码（用于可观测性路由和调用方程序化处理）。
type Code string

const (
	CodeOK                 Code = "OK"
	CodeInvalidInput       Code = "INVALID_INPUT"
	CodeNotFound           Code = "NOT_FOUND"
	CodeAlreadyExists      Code = "ALREADY_EXISTS"
	CodeConflict           Code = "CONFLICT"
	CodeUnauthorized       Code = "UNAUTHORIZED"
	CodeForbidden          Code = "FORBIDDEN"
	CodeTimeout            Code = "TIMEOUT"
	CodeCancelled          Code = "CANCELLED"
	CodeResourceExhausted  Code = "RESOURCE_EXHAUSTED"
	CodeInternal           Code = "INTERNAL"
	CodeUnimplemented      Code = "UNIMPLEMENTED"
	CodeProviderExhausted  Code = "PROVIDER_EXHAUSTED"
	CodeNetworkUnavailable Code = "NETWORK_UNAVAILABLE"
	CodeTaintViolation     Code = "TAINT_VIOLATION"
	CodeSandboxTier0Limit  Code = "SANDBOX_TIER0_LIMIT"
	// CodeStorageUnavailable 标记持久化存储层写入/读取失败（如 SurrealDB kv-mem 连接中断、
	// 磁盘写满等），与 CodeInternal（泛化内部错误）区分，供上层（如 Agent FSM）识别为
	// "需要熔断而非重试" 的信号（GD-13-003，见 docs/arch/decisions/ADR-0046）。
	CodeStorageUnavailable Code = "STORAGE_UNAVAILABLE"
)

// Error 是 Polaris 统一应用错误类型。
// Code 用于程序化路由，Message 用于日志，Cause 用于链式溯源。
type Error struct {
	Code       Code
	Message    string
	Cause      error
	RetryAfter int // 可选，建议的重试间隔（秒），用于自动转换为 Retry-After HTTP 头
}

// WithRetryAfter 设置重试建议间隔并返回自身。
func (e *Error) WithRetryAfter(seconds int) *Error {
	e.RetryAfter = seconds
	return e
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// Is 报告 e 是否与 target 等价（仅比较 Code，与 Message/Cause 无关）。
// 使 errors.Is(err, &apperr.Error{Code: apperr.CodeNotFound}) 可用。
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return e.Code == t.Code
	}
	return false
}

// New 构造一个不含 Cause 的应用错误。
func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// Wrap 构造一个带 Cause 的应用错误，用于链式溯源。
func Wrap(code Code, msg string, cause error) *Error {
	return &Error{Code: code, Message: msg, Cause: cause}
}

// IsCode 报告 err 链中是否存在 Code == code 的 *Error。
// 替代四行 errors.As 模板，是最常用的错误判断方式：
//
//	if apperr.IsCode(err, apperr.CodeNotFound) { ... }
func IsCode(err error, code Code) bool {
	var ae *Error
	return errors.As(err, &ae) && ae.Code == code
}

// CodeOf 从 err 链中提取第一个 *Error 的 Code。
// 链中无 *Error 时返回 CodeInternal（安全兜底，不返回零值）。
//
//	status := apperr.HTTPStatus(apperr.CodeOf(err))
func CodeOf(err error) Code {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return CodeInternal
}

// HTTPStatus 将 Code 映射到对应的 HTTP 状态码。
// 供 gateway 层统一调用，避免各 handler 手写魔法数字。
//
//	if err != nil {
//		if ae, ok := err.(*apperr.Error); ok && ae.RetryAfter > 0 {
//			w.Header().Set("Retry-After", strconv.Itoa(ae.RetryAfter))
//		}
//		http.Error(w, err.Error(), apperr.HTTPStatus(apperr.CodeOf(err)))
//	}
func HTTPStatus(code Code) int {
	switch code {
	case CodeOK:
		return http.StatusOK
	case CodeInvalidInput:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeAlreadyExists, CodeConflict:
		return http.StatusConflict
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden, CodeTaintViolation:
		return http.StatusForbidden
	case CodeTimeout, CodeCancelled:
		return http.StatusGatewayTimeout
	case CodeResourceExhausted, CodeProviderExhausted:
		return http.StatusTooManyRequests
	case CodeUnimplemented:
		return http.StatusNotImplemented
	case CodeNetworkUnavailable:
		return http.StatusBadGateway
	case CodeSandboxTier0Limit:
		return http.StatusServiceUnavailable
	default: // CodeInternal 及未知 Code
		return http.StatusInternalServerError
	}
}

// 2026-07-14（ADR-0051）：包级 Is 删除——纯委托 errors.Is(err, target)，全仓
// （含测试）零调用点；包头文档注释列出的官方推荐用法（New/Wrap/IsCode/CodeOf/
// HTTPStatus）本就不包含它，是无附加值的冗余包装（不同于 (*Error).Is 方法本身，
// 后者是 errors.Is 识别 *apperr.Error 的必要多态钩子，被间接大量使用，予以保留）。
