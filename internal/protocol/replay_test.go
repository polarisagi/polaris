package protocol

import (
	"testing"
)

func TestReplayMode(t *testing.T) {
	// Ensure default is false
	if IsReplaying() != false {
		t.Errorf("Expected IsReplaying() to be false initially")
	}

	// Set to true
	SetReplayMode(true)
	if IsReplaying() != true {
		t.Errorf("Expected IsReplaying() to be true after SetReplayMode(true)")
	}

	// Set to false
	SetReplayMode(false)
	if IsReplaying() != false {
		t.Errorf("Expected IsReplaying() to be false after SetReplayMode(false)")
	}
}
