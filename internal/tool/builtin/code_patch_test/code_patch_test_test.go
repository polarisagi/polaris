package codepatchtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/sandbox"
	toolmeta "github.com/polarisagi/polaris/internal/tool"
)

func TestCodePatchTestFn_FailsClosedWithoutPhysicalRunner(t *testing.T) {
	input, _ := json.Marshal(request{Path: "/tmp/repo", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CandidatePatch: "diff --git a/a b/a", FailToPassCommands: []string{"go test"}, PassToPassCommands: []string{"go test"}})
	for name, runner := range map[string]sandbox.CmdRunner{"nil": nil, "nop": sandbox.NopCmdRunner{}} {
		t.Run(name, func(t *testing.T) {
			if _, err := MakeCodePatchTestFn(Deps{CmdRunner: runner, ScratchRoot: "/tmp"})(context.Background(), input); err == nil {
				t.Fatal("runner without verifiable physical isolation must fail closed")
			}
		})
	}
}

func TestPatchPathsRejectsUnparsedAndUnsafeTargets(t *testing.T) {
	for _, patch := range []string{
		"--- a/test.go\n+++ b/test.go\n@@ -1 +1 @@",
		"diff --git a/../outside b/../outside\n--- a/../outside\n+++ b/../outside\n",
		"diff --git a/.git/config b/.git/config\n--- a/.git/config\n+++ b/.git/config\n",
	} {
		if _, err := patchPaths(patch); err == nil {
			t.Fatalf("unsafe/unparsed patch accepted: %q", patch)
		}
	}
}

func TestCandidatePatchCannotChangeTrustedTestTarget(t *testing.T) {
	patch := "diff --git a/tests/fix_test.go b/tests/fix_test.go\n--- a/tests/fix_test.go\n+++ b/tests/fix_test.go\n@@ -1 +1 @@\n-old\n+candidate\n"
	if _, err := applyPatch(context.Background(), nil, t.TempDir(), t.TempDir(), "candidate.patch", patch, t.TempDir(), "/source", map[string]struct{}{"tests/fix_test.go": {}}); err == nil {
		t.Fatal("candidate patch changing a trusted test target must be rejected")
	}
}

func TestCodePatchValidationRejectsEmptyPatchAndTestGroups(t *testing.T) {
	base := request{Path: "/tmp/repo", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CandidatePatch: " ", FailToPassCommands: []string{"go test"}, PassToPassCommands: []string{"go test"}}
	if err := validateRequest(base, Deps{ScratchRoot: "/tmp/scratch", AllowedPaths: []string{"/tmp"}}); err == nil {
		t.Fatal("empty candidate patch must be rejected")
	}
	base.CandidatePatch = "diff --git a/main.go b/main.go\n"
	base.FailToPassCommands = nil
	if err := validateRequest(base, Deps{ScratchRoot: "/tmp/scratch", AllowedPaths: []string{"/tmp"}}); err == nil {
		t.Fatal("missing fail_to_pass commands must be rejected")
	}
}

func TestBaselineEvidenceRejectsMissingDependencyAndTimeout(t *testing.T) {
	if allCommandsFail([]commandResult{{ExitCode: -1}}) || allCommandsFail([]commandResult{{ExitCode: 127}}) {
		t.Fatal("executor/dependency failures must not count as expected baseline failures")
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := executionContextError(ctx); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired execution deadline must report timeout, got %v", err)
	}
}

func TestEvaluatorEvidenceRequiresAllFourCommandGroups(t *testing.T) {
	valid := result{Passed: true, Phase: "candidate", BaselineF2P: []commandResult{{ExitCode: 1}}, BaselinePTP: []commandResult{{ExitCode: 0}}, CandidateF2P: []commandResult{{ExitCode: 0}}, CandidatePTP: []commandResult{{ExitCode: 0}}}
	if passed, ok := validateResult(valid); !ok || !passed {
		t.Fatal("complete verified evidence was rejected")
	}
	valid.BaselineF2P[0].ExitCode = -1
	if passed, ok := validateResult(valid); ok || passed {
		t.Fatal("executor error must not satisfy baseline failure evidence")
	}
}

func TestToolMetadata_LoadsBoundedProcessTool(t *testing.T) {
	meta, err := toolmeta.GetBuiltinToolMeta("code_patch_test")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "code_patch_test" || meta.Capability == 0 || len(meta.SideEffects) != 2 {
		t.Fatalf("incomplete tool metadata: %+v", meta)
	}
	if _, ok := meta.InputSchema.(map[string]any); !ok {
		t.Fatal("input schema missing")
	}
}
