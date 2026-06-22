package sysadmin

import (
	"testing"
)

func TestHookRunnerError(t *testing.T) {
	err := hookNotFoundError{}
	if err.Error() != "hook script not found" {
		t.Errorf("expected hook script not found")
	}
}
