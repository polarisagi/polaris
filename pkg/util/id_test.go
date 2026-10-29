package util

import (
	"strings"
	"testing"
)

func TestDeterministicID(t *testing.T) {
	a := DeterministicID("ext", "Polaris Browser Bridge", "polarisagi/official/polaris-browser-bridge")
	if a != DeterministicID("ext", "Polaris Browser Bridge", "polarisagi/official/polaris-browser-bridge") {
		t.Fatal("same key must yield same ID")
	}
	if !strings.HasPrefix(a, "ext_polaris_browser_brid_") || len(a) != len("ext_polaris_browser_brid_")+12 {
		t.Fatalf("unexpected shape: %s", a)
	}
	if a == DeterministicID("ext", "Polaris Browser Bridge", "other/polaris-browser-bridge") {
		t.Fatal("different key must yield different ID")
	}
	if r1, r2 := GenerateHumanReadableID("ext", "x"), GenerateHumanReadableID("ext", "x"); r1 == r2 {
		t.Fatal("random IDs must differ")
	}
}
