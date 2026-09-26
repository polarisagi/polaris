package plugin

import (
	"testing"
)

func TestCond(t *testing.T) {
	if cond(true, "a", "b") != "a" {
		t.Errorf("expected a")
	}
	if cond(false, "a", "b") != "b" {
		t.Errorf("expected b")
	}
}
