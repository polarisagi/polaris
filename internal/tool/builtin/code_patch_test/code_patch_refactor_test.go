package codepatchtest

import (
	"strings"
	"testing"
)

func TestPatchPaths_ValidatesDiffAndFileHeaders(t *testing.T) {
	patch := "diff --git a/pkg/value.go b/pkg/value.go\n--- a/pkg/value.go\n+++ b/pkg/value.go\n@@ -1 +1 @@\n-old\n+new\n"
	paths, err := patchPaths(patch)
	if err != nil {
		t.Fatalf("valid patch rejected: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("unexpected paths: %v", paths)
	}
	if _, ok := paths["pkg/value.go"]; !ok {
		t.Fatalf("parsed target missing: %v", paths)
	}

	for _, invalid := range []string{
		"diff --git a/one.go b/two.go\n--- a/other.go\n+++ b/two.go\n",
		"diff --git a/one.go b/one.go\n--- a/one.go\n+++ b/other.go\n",
		"diff --git a/one.go b/one.go\nrename from one.go\n",
		"diff --git a/one.go b/one.go\ncopy to two.go\n",
	} {
		if _, err := patchPaths(invalid); err == nil {
			t.Errorf("invalid patch accepted: %q", invalid)
		}
	}
}

func TestPatchPaths_RejectsInputBeyondParserBound(t *testing.T) {
	if _, err := patchPaths(strings.Repeat("x", maxPatchBytes+1)); err == nil {
		t.Fatal("oversized patch accepted")
	}
}
