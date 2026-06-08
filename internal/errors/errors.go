package errors

import (
	"errors"
	"fmt"
)

// Sentinel errors.
var (
	ErrNotImplemented    = errors.New("not implemented")
	ErrEmptyIndex        = errors.New("index is empty (valid initial state)")
	ErrResourceExhausted = errors.New("resource exhausted")
	ErrTimeout           = errors.New("operation timed out")
	ErrCancelled         = errors.New("operation cancelled")
	ErrUnauthorized      = errors.New("unauthorized")
	ErrForbidden         = errors.New("forbidden")
	ErrInvalidInput      = errors.New("invalid input")
	ErrNotFound          = errors.New("not found")
	ErrAlreadyExists     = errors.New("already exists")
	ErrInternal          = errors.New("internal error")

	// Infrastructure / uncontrollable errors — must NOT count against quality metrics.
	ErrProviderExhausted  = errors.New("all LLM providers exhausted; non-logic failure")
	ErrNetworkUnavailable = errors.New("network unavailable; non-logic failure")

	// Safety errors — one-veto: any occurrence must trigger safety_fail in eval harness.
	// 不得归入 uncontrollable 组：污点违规是安全红线，必须计入安全指标并触发一票否决。
	ErrTaintViolation = errors.New("taint gate rejected: external data entering instruction slot")
)

// Code categorises errors for observability and routing.
type Code string

const (
	CodeOK                 Code = "OK"
	CodeInvalidInput       Code = "INVALID_INPUT"
	CodeNotFound           Code = "NOT_FOUND"
	CodeAlreadyExists      Code = "ALREADY_EXISTS"
	CodeUnauthorized       Code = "UNAUTHORIZED"
	CodeForbidden          Code = "FORBIDDEN"
	CodeTimeout            Code = "TIMEOUT"
	CodeCancelled          Code = "CANCELLED"
	CodeResourceExhausted  Code = "RESOURCE_EXHAUSTED"
	CodeInternal           Code = "INTERNAL"
	CodeProviderExhausted  Code = "PROVIDER_EXHAUSTED"
	CodeNetworkUnavailable Code = "NETWORK_UNAVAILABLE"
	CodeTaintViolation     Code = "TAINT_VIOLATION"
)

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

func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

func Wrap(code Code, msg string, cause error) *Error {
	return &Error{Code: code, Message: msg, Cause: cause}
}
