package diff_text

import (
	"testing"
)

func TestComputeUnifiedDiff(t *testing.T) {
	oldLines := []string{"line1", "line2", "line3"}
	newLines := []string{"line1", "line2_changed", "line3", "line4"}
	diff := computeUnifiedDiff(oldLines, newLines)
	if diff == "" {
		t.Fatalf("expected diff, got empty")
	}
}
