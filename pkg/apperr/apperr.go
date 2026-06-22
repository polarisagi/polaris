// Package apperr 提供 Polaris 应用层统一错误类型。
//
// 所有模块在构造和返回错误时必须使用本包，禁止裸 error 泄漏调用链。
// 外部扩展（插件/CLI 工具/SDK）通过 errors.As(err, &apperr.Error{}) 判断错误类型。
//
// 错误构造：
//
//	apperr.New(apperr.CodeNotFound, "session not found")
//	apperr.Wrap(apperr.CodeInternal, "db write failed", err)
//
// 错误判断：
//
//	var ae *apperr.Error
//	if errors.As(err, &ae) && ae.Code == apperr.CodeTaintViolation { ... }
package apperr

import (
	"errors"
	"fmt"
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
)

// Error 是 Polaris 统一应用错误类型。
// Code 用于程序化路由，Message 用于日志，Cause 用于链式溯源。
type Error struct {
	Code    Code
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// New 构造一个不含 Cause 的应用错误。
func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// Wrap 构造一个带 Cause 的应用错误，用于链式溯源。
func Wrap(code Code, msg string, cause error) *Error {
	return &Error{Code: code, Message: msg, Cause: cause}
}

// Is 委托标准库 errors.Is，供 errors.Is(err, target) 调用链使用。
func Is(err, target error) bool {
	return errors.Is(err, target)
}
