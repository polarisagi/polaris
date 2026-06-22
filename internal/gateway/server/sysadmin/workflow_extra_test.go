package sysadmin

import (
	"strings"
	"testing"
)

func TestWorkflowIDs(t *testing.T) {
	wID := newWorkflowID()
	if !strings.HasPrefix(wID, "wf_") {
		t.Errorf("invalid workflow ID prefix: %s", wID)
	}
	if len(wID) != 19 {
		t.Errorf("invalid workflow ID length: %d", len(wID))
	}

	wsID := newWorkflowStepID()
	if !strings.HasPrefix(wsID, "ws_") {
		t.Errorf("invalid workflow step ID prefix: %s", wsID)
	}

	wfrID := newWorkflowRunID()
	if !strings.HasPrefix(wfrID, "wfr_") {
		t.Errorf("invalid workflow run ID prefix: %s", wfrID)
	}
}
