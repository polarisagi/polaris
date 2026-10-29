package prompt_test

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/prompt"
)

func TestNewRandomBoundary(t *testing.T) {
	s1, e1 := prompt.NewRandomBoundary()
	s2, _ := prompt.NewRandomBoundary()

	if s1 == s2 {
		t.Fatalf("expected different boundaries, got same: %s", s1)
	}

	if !strings.HasPrefix(s1, "[CODE_START_") || !strings.HasSuffix(s1, "]") {
		t.Fatalf("unexpected start format: %s", s1)
	}
	if !strings.HasPrefix(e1, "[CODE_END_") || !strings.HasSuffix(e1, "]") {
		t.Fatalf("unexpected end format: %s", e1)
	}
}
