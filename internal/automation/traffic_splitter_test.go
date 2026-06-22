package automation

import (
	"testing"
)

func TestTrafficSplitter(t *testing.T) {
	ts := NewTrafficSplitter("base", "cand")

	// Default percent is 0
	if ts.Route("session1") != "base" {
		t.Errorf("Expected base")
	}

	// Set 100
	ts.SetPercent(100)
	if ts.Route("session1") != "cand" {
		t.Errorf("Expected cand")
	}

	// Out of bounds
	ts.SetPercent(150)
	if ts.Route("session1") != "cand" {
		t.Errorf("Expected cand, got %s", ts.Route("session1"))
	}

	ts.SetPercent(-50)
	if ts.Route("session1") != "base" {
		t.Errorf("Expected base")
	}

	// Middle percent
	ts.SetPercent(50)
	// We just test fnvHash predictability. "session1" -> hash % 100
	h := int(fnvHash("session1") % 100)
	if h < 50 {
		if ts.Route("session1") != "cand" {
			t.Errorf("Expected cand due to hash")
		}
	} else {
		if ts.Route("session1") != "base" {
			t.Errorf("Expected base due to hash")
		}
	}

	// Rollback
	ts.Rollback()
	if ts.Route("session1") != "base" {
		t.Errorf("Expected base after rollback")
	}
}
