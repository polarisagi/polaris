package cognition

import (
	"testing"
)

// ─── ComputeLayoutFingerprint ──────────────────────────────────────────────────

func TestFingerprint_EmptyLayout(t *testing.T) {
	layout := BuildContext(nil, 1000)
	fp := ComputeLayoutFingerprint(layout)
	if fp == "" {
		t.Error("fingerprint should not be empty")
	}
}

func TestFingerprint_SameLayout_SameFingerprint(t *testing.T) {
	a := BuildContext(nil, 1000)
	b := BuildContext(nil, 1000)
	fpA := ComputeLayoutFingerprint(a)
	fpB := ComputeLayoutFingerprint(b)
	if fpA != fpB {
		t.Error("identical layouts should have identical fingerprints")
	}
}

func TestFingerprint_DifferentTokens_DifferentFingerprint(t *testing.T) {
	a := BuildContext(nil, 1000)
	b := BuildContext(nil, 2000)
	fpA := ComputeLayoutFingerprint(a)
	fpB := ComputeLayoutFingerprint(b)
	if fpA == fpB {
		t.Error("different maxTokens should produce different fingerprints")
	}
}

func TestFingerprint_DifferentContent_Different(t *testing.T) {
	a := BuildContext(nil, 1000)
	b := BuildContext(nil, 1000)
	a.zones[0].Content = "hello"
	b.zones[0].Content = "world"
	fpA := ComputeLayoutFingerprint(a)
	fpB := ComputeLayoutFingerprint(b)
	if fpA == fpB {
		t.Error("different content should produce different fingerprints")
	}
}

// ─── EpochTracker ──────────────────────────────────────────────────────────────

func TestEpochTracker_StartsAtOne(t *testing.T) {
	tr := NewEpochTracker()
	if tr.CurrentEpoch() != 1 {
		t.Errorf("initial epoch: want 1, got %d", tr.CurrentEpoch())
	}
}

func TestEpochTracker_SameFingerprint_SameEpoch(t *testing.T) {
	tr := NewEpochTracker()
	fp := ContextFingerprint("abc123")

	e1 := tr.Check(fp)
	e2 := tr.Check(fp)
	if e1 != e2 {
		t.Errorf("same fp: epochs should match: %d vs %d", e1, e2)
	}
}

func TestEpochTracker_DifferentFingerprint_Increments(t *testing.T) {
	tr := NewEpochTracker()

	e1 := tr.Check(ContextFingerprint("abc"))
	e2 := tr.Check(ContextFingerprint("def"))
	if e2 != e1+1 {
		t.Errorf("different fp: epoch should increment: %d -> %d", e1, e2)
	}
}

func TestEpochTracker_MultipleChanges(t *testing.T) {
	tr := NewEpochTracker()

	_ = tr.Check(ContextFingerprint("a"))
	e2 := tr.Check(ContextFingerprint("b"))
	e3 := tr.Check(ContextFingerprint("c"))
	if e3 != e2+1 {
		t.Errorf("epochs should be sequential: %d -> %d", e2, e3)
	}
}

func TestEpochTracker_ReuseOldFingerprint_AfterChange(t *testing.T) {
	tr := NewEpochTracker()

	_ = tr.Check(ContextFingerprint("a"))
	e2 := tr.Check(ContextFingerprint("b"))
	e3 := tr.Check(ContextFingerprint("a"))
	if e3 != e2+1 {
		t.Errorf("revisiting old fp after diff: want %d, got %d", e2+1, e3)
	}
}

func TestEpochTracker_CurrentEpoch_NoMutation(t *testing.T) {
	tr := NewEpochTracker()
	tr.Check(ContextFingerprint("abc"))
	tr.Check(ContextFingerprint("def"))
	if tr.CurrentEpoch() != 3 {
		t.Errorf("after 2 distinct fp checks: want 3, got %d", tr.CurrentEpoch())
	}
}

func TestEpochTracker_ConcurrentSafe(t *testing.T) {
	tr := NewEpochTracker()
	done := make(chan struct{})
	go func() {
		tr.Check(ContextFingerprint("from-goroutine"))
		close(done)
	}()
	tr.Check(ContextFingerprint("from-main"))
	<-done
}
