package provider

import (
	"testing"
)

func TestBoolToInt(t *testing.T) {
	if boolToInt(true) != 1 {
		t.Errorf("expected 1")
	}
	if boolToInt(false) != 0 {
		t.Errorf("expected 0")
	}
}
