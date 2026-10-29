package apperr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Code 是 apperr.Code 的本地别名，仅用于 TestHTTPStatus 的测试表格简写。
type Code = apperr.Code

func TestNew_ErrorString(t *testing.T) {
	err := apperr.New(apperr.CodeInternal, "something failed")
	if err.Code != apperr.CodeInternal {
		t.Errorf("expected code %s, got %s", apperr.CodeInternal, err.Code)
	}
	if err.Message != "something failed" {
		t.Errorf("expected message 'something failed', got %s", err.Message)
	}
	if err.Error() != "[INTERNAL] something failed" {
		t.Errorf("unexpected Error() string: %s", err.Error())
	}
}

func TestWrap_WithCause(t *testing.T) {
	cause := errors.New("root cause")
	err := apperr.Wrap(apperr.CodeTimeout, "operation timed out", cause)
	if err.Code != apperr.CodeTimeout {
		t.Errorf("expected %s, got %s", apperr.CodeTimeout, err.Code)
	}
	if err.Cause != cause {
		t.Error("expected cause to match")
	}
	expected := "[TIMEOUT] operation timed out: root cause"
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}
}

func TestUnwrap(t *testing.T) {
	cause := errors.New("deep cause")
	err := apperr.Wrap(apperr.CodeInternal, "outer", cause)
	if !errors.Is(err, cause) {
		t.Error("errors.Is should find wrapped cause")
	}
}

func TestNew_NoCause(t *testing.T) {
	err := apperr.New(apperr.CodeNotFound, "entity missing")
	if err.Cause != nil {
		t.Error("expected nil cause")
	}
	if err.Unwrap() != nil {
		t.Error("Unwrap() should return nil for no cause")
	}
}

func TestAllCodes(t *testing.T) {
	codes := []apperr.Code{
		apperr.CodeOK, apperr.CodeInvalidInput, apperr.CodeNotFound, apperr.CodeAlreadyExists,
		apperr.CodeUnauthorized, apperr.CodeForbidden, apperr.CodeTimeout, apperr.CodeCancelled,
		apperr.CodeResourceExhausted, apperr.CodeInternal, apperr.CodeProviderExhausted,
		apperr.CodeNetworkUnavailable, apperr.CodeTaintViolation,
	}
	for _, code := range codes {
		err := apperr.New(code, "test")
		if err.Code != code {
			t.Errorf("code mismatch: expected %s got %s", code, err.Code)
		}
	}
}

func TestSentinels_NotNil(t *testing.T) {
	sentinels := []error{
		apperr.ErrNotImplemented, apperr.ErrEmptyIndex, apperr.ErrResourceExhausted, apperr.ErrTimeout,
		apperr.ErrCancelled, apperr.ErrUnauthorized, apperr.ErrForbidden, apperr.ErrInvalidInput,
		apperr.ErrNotFound, apperr.ErrAlreadyExists, apperr.ErrInternal,
		apperr.ErrProviderExhausted, apperr.ErrNetworkUnavailable, apperr.ErrTaintViolation,
		apperr.ErrTier0SandboxLimit,
	}
	for _, s := range sentinels {
		if s == nil {
			t.Errorf("sentinel error should not be nil: %v", s)
		}
	}
}

func TestIsCode(t *testing.T) {
	err := apperr.New(apperr.CodeNotFound, "not found")
	if !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Error("IsCode should return true for matching code")
	}
	if apperr.IsCode(err, apperr.CodeInternal) {
		t.Error("IsCode should return false for non-matching code")
	}
	// 链式包装场景
	wrapped := fmt.Errorf("outer: %w", err)
	if !apperr.IsCode(wrapped, apperr.CodeNotFound) {
		t.Error("IsCode should work through error chain")
	}
	// nil 不 panic
	if apperr.IsCode(nil, apperr.CodeInternal) {
		t.Error("IsCode(nil, ...) should return false")
	}
}

func TestCodeOf(t *testing.T) {
	err := apperr.New(apperr.CodeForbidden, "forbidden")
	if got := apperr.CodeOf(err); got != apperr.CodeForbidden {
		t.Errorf("expected CodeForbidden, got %s", got)
	}
	// 链式包装
	wrapped := fmt.Errorf("wrap: %w", err)
	if got := apperr.CodeOf(wrapped); got != apperr.CodeForbidden {
		t.Errorf("CodeOf through chain: expected CodeForbidden, got %s", got)
	}
	// 非 *Error 类型 → 兜底 CodeInternal
	plain := errors.New("plain error")
	if got := apperr.CodeOf(plain); got != apperr.CodeInternal {
		t.Errorf("CodeOf non-apperr: expected CodeInternal, got %s", got)
	}
	// nil → CodeInternal
	if got := apperr.CodeOf(nil); got != apperr.CodeInternal {
		t.Errorf("CodeOf(nil): expected CodeInternal, got %s", got)
	}
}

func TestHTTPStatus(t *testing.T) {
	cases := []struct {
		code Code
		want int
	}{
		{apperr.CodeOK, 200},
		{apperr.CodeInvalidInput, 400},
		{apperr.CodeUnauthorized, 401},
		{apperr.CodeForbidden, 403},
		{apperr.CodeNotFound, 404},
		{apperr.CodeConflict, 409},
		{apperr.CodeAlreadyExists, 409},
		{apperr.CodeResourceExhausted, 429},
		{apperr.CodeProviderExhausted, 429},
		{apperr.CodeInternal, 500},
		{apperr.CodeUnimplemented, 501},
		{apperr.CodeNetworkUnavailable, 502},
		{apperr.CodeTimeout, 504},
		{apperr.CodeCancelled, 504},
		{apperr.CodeTaintViolation, 403},
		{apperr.CodeSandboxTier0Limit, 503},
	}
	for _, tc := range cases {
		if got := apperr.HTTPStatus(tc.code); got != tc.want {
			t.Errorf("HTTPStatus(%s): want %d, got %d", tc.code, tc.want, got)
		}
	}
}

func TestError_Is_CodeBased(t *testing.T) {
	// errors.Is 现在基于 Code 匹配，与 sentinel 语义一致
	err1 := apperr.New(apperr.CodeNotFound, "user missing")
	err2 := apperr.New(apperr.CodeNotFound, "session missing")
	if !errors.Is(err1, err2) {
		t.Error("errors.Is should match when Code is equal (code-based equality)")
	}
	err3 := apperr.New(apperr.CodeInternal, "other error")
	if errors.Is(err1, err3) {
		t.Error("errors.Is should NOT match when Code differs")
	}
	// sentinel 匹配
	if !errors.Is(apperr.New(apperr.CodeNotFound, "anything"), apperr.ErrNotFound) {
		t.Error("any CodeNotFound error should match sentinel ErrNotFound via errors.Is")
	}
}

func TestProxy_InternalErrors_Compatible(t *testing.T) {
	// internal/errors 包不存在（CLAUDE.md 历史误写），pkg/apperr 是唯一统一错误类型
	// 通过 errors.As 检查类型兼容性
	err := apperr.New(apperr.CodeForbidden, "proxy test")
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatal("errors.As should work with *apperr.Error")
	}
	if ae.Code != apperr.CodeForbidden {
		t.Errorf("expected CodeForbidden, got %s", ae.Code)
	}
}
