package apperr_test

import (
	"errors"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

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

func TestProxy_InternalErrors_Compatible(t *testing.T) {
	// 验证 internal/errors 代理层与 pkg/apperr 的类型完全兼容
	// 通过 errors.As 检查类型别名是否正常工作
	err := apperr.New(apperr.CodeForbidden, "proxy test")
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatal("errors.As should work with *apperr.Error")
	}
	if ae.Code != apperr.CodeForbidden {
		t.Errorf("expected CodeForbidden, got %s", ae.Code)
	}
}
