package errors

import (
	"errors"
	"testing"
)

func TestNew_ErrorString(t *testing.T) {
	err := New(CodeInternal, "something failed")
	if err.Code != CodeInternal {
		t.Errorf("expected code %s, got %s", CodeInternal, err.Code)
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
	err := Wrap(CodeTimeout, "operation timed out", cause)
	if err.Code != CodeTimeout {
		t.Errorf("expected %s, got %s", CodeTimeout, err.Code)
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
	err := Wrap(CodeInternal, "outer", cause)
	if !errors.Is(err, cause) {
		t.Error("errors.Is should find wrapped cause")
	}
}

func TestNew_NoCause(t *testing.T) {
	err := New(CodeNotFound, "entity missing")
	if err.Cause != nil {
		t.Error("expected nil cause")
	}
	if err.Unwrap() != nil {
		t.Error("Unwrap() should return nil for no cause")
	}
}

func TestAllCodes(t *testing.T) {
	codes := []Code{
		CodeOK, CodeInvalidInput, CodeNotFound, CodeAlreadyExists,
		CodeUnauthorized, CodeForbidden, CodeTimeout, CodeCancelled,
		CodeResourceExhausted, CodeInternal, CodeProviderExhausted,
		CodeNetworkUnavailable, CodeTaintViolation,
	}
	for _, code := range codes {
		err := New(code, "test")
		if err.Code != code {
			t.Errorf("code mismatch: expected %s got %s", code, err.Code)
		}
	}
}

func TestSentinels_NotNil(t *testing.T) {
	sentinels := []error{
		ErrNotImplemented, ErrEmptyIndex, ErrResourceExhausted, ErrTimeout,
		ErrCancelled, ErrUnauthorized, ErrForbidden, ErrInvalidInput,
		ErrNotFound, ErrAlreadyExists, ErrInternal,
		ErrProviderExhausted, ErrNetworkUnavailable, ErrTaintViolation,
	}
	for _, s := range sentinels {
		if s == nil {
			t.Errorf("sentinel error should not be nil: %v", s)
		}
	}
}
