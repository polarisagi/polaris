package apperr

import (
	"errors"
	"testing"
)

func TestNewSentinel_IdentityMatch(t *testing.T) {
	s := NewSentinel(CodeResourceExhausted, "replan exhausted")
	other := New(CodeResourceExhausted, "rate limited")
	if errors.Is(other, s) {
		t.Fatal("same-code non-sentinel must not match sentinel")
	}
	if !errors.Is(Wrap(CodeInternal, "ctx", s), s) {
		t.Fatal("wrapped sentinel must match")
	}
	if !errors.Is(s, &Error{Code: CodeResourceExhausted}) {
		t.Fatal("code-based matching against non-sentinel target must still work")
	}
}
