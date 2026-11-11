package codepatchtest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/sandbox"
	toolSandbox "github.com/polarisagi/polaris/internal/tool/sandbox"
)

func TestCodePatchTestFn_RealGoAndJavaScriptFixtures(t *testing.T) {
	runner := toolSandbox.NewBoundedCmdRunner()
	for _, fixture := range []string{"go", "javascript"} {
		t.Run(fixture, func(t *testing.T) {
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			scratchParent := t.TempDir()
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			candidate, testPatch, f2p, ptp := fixtureData(fixture, repo)
			writeFixture(t, repo, fixture)
			if !probePhysicalRunner(t, runner, repo) {
				t.Skip("Rust bwrap/Seatbelt isolation is unavailable")
			}
			if !probeFixtureToolchain(t, runner, fixture, repo) {
				t.Skip(fixture + " runtime is not installed in the physical sandbox")
			}
			initFixtureGit(t, repo)
			baseCommit := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
			if len(baseCommit) != 40 && len(baseCommit) != 64 {
				t.Fatalf("unexpected fixture commit hash %q", baseCommit)
			}
			before := gitFixtureOutput(t, repo, "status", "--porcelain")
			input, err := json.Marshal(request{Path: repo, BaseCommit: baseCommit, CandidatePatch: candidate, TestPatch: testPatch,
				FailToPassCommands: []string{f2p}, PassToPassCommands: []string{ptp}})
			if err != nil {
				t.Fatal(err)
			}
			canonicalScratch, err := filepath.EvalSymlinks(scratchParent)
			if err != nil {
				t.Fatal(err)
			}
			fn := MakeCodePatchTestFn(Deps{CmdRunner: runner, ScratchRoot: canonicalScratch, AllowedPaths: []string{base, scratchParent}})
			output, err := fn(context.Background(), input)
			if err != nil {
				t.Fatalf("patch eval failed: %v", err)
			}
			var got result
			if err := json.Unmarshal(output, &got); err != nil {
				t.Fatal(err)
			}
			if !got.Passed || got.Phase != "candidate" {
				t.Fatalf("candidate did not pass verified baseline/candidate flow: %+v", got)
			}
			after := gitFixtureOutput(t, repo, "status", "--porcelain")
			if before != after || after != "" {
				t.Fatalf("source worktree/index changed: before=%q after=%q", before, after)
			}
		})
	}
}

func writeFixture(t *testing.T, repo, fixture string) {
	t.Helper()
	files := map[string]string{"guard_fixture.txt": "base\n"}
	if fixture == "go" {
		files["main.go"] = "package main\n\nfunc answer() int { return 0 }\n"
		files["main_test.go"] = "package main\n\nimport (\"os\"; \"testing\")\nfunc TestFix(t *testing.T) { if answer() != 42 { t.Fatal(\"bug\") } }\nfunc TestProtectedTarget(t *testing.T) { b, e := os.ReadFile(\"guard_fixture.txt\"); if e != nil || string(b) != \"trusted\\n\" { t.Fatal(\"trusted test target changed\") } }\n"
	} else {
		files["index.js"] = "function answer() { return 0; }\nmodule.exports = { answer };\n"
		files["test.js"] = "const fs=require('fs'); const {answer}=require('./index'); if (process.argv[2]==='fix' && answer()!==42) throw Error('bug'); if (process.argv[2]==='stable' && fs.readFileSync('guard_fixture.txt','utf8')!=='trusted\\n') throw Error('trusted test target changed');\n"
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func fixtureData(fixture, sourceRepo string) (candidate, testPatch, f2p, ptp string) {
	testPatch = "diff --git a/guard_fixture.txt b/guard_fixture.txt\n--- a/guard_fixture.txt\n+++ b/guard_fixture.txt\n@@ -1 +1 @@\n-base\n+trusted\n"
	probeSourceWrite := "if [ -f .candidate-stage ]; then if printf tampered > " + shellQuote(filepath.Join(sourceRepo, "guard_fixture.txt")) + " 2>/dev/null; then exit 9; fi; fi; "
	if fixture == "go" {
		candidate = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,5 @@\n package main\n \n-func answer() int { return 0 }\n+import \"os\"\n+func init() { _ = os.WriteFile(\"guard_fixture.txt\", []byte(\"tampered\\n\"), 0600) }\n+func answer() int { return 42 }\n"
		candidate += "diff --git a/.candidate-stage b/.candidate-stage\nnew file mode 100644\n--- /dev/null\n+++ b/.candidate-stage\n@@ -0,0 +1 @@\n+candidate\n"
		return candidate, testPatch, "GO111MODULE=off go test -run TestFix -count=1", probeSourceWrite + "GO111MODULE=off go test -run TestProtectedTarget -count=1"
	}
	candidate = "diff --git a/index.js b/index.js\n--- a/index.js\n+++ b/index.js\n@@ -1,2 +1,4 @@\n-function answer() { return 0; }\n+const fs=require('fs');\n+try { fs.writeFileSync('guard_fixture.txt','tampered\\n'); } catch (e) {}\n+function answer() { return 42; }\n module.exports = { answer };\n"
	candidate += "diff --git a/.candidate-stage b/.candidate-stage\nnew file mode 100644\n--- /dev/null\n+++ b/.candidate-stage\n@@ -0,0 +1 @@\n+candidate\n"
	return candidate, testPatch, "node test.js fix", probeSourceWrite + "node test.js stable"
}

func probePhysicalRunner(t *testing.T, runner sandbox.CmdRunner, dir string) bool {
	t.Helper()
	_, code, method, err := runner.RunCmd(context.Background(), sandbox.CmdRunnerCfg{CallerType: "builtin", Command: "true", WorkDir: dir, AllowedPaths: []string{dir}, NetworkBlock: true, TimeoutMs: 30000})
	if method != "bwrap" && method != "seatbelt" {
		return false
	}
	if err != nil || code != 0 {
		t.Fatalf("physical sandbox probe failed: method=%s code=%d err=%v", method, code, err)
	}
	return true
}

func probeFixtureToolchain(t *testing.T, runner sandbox.CmdRunner, fixture, dir string) bool {
	t.Helper()
	command := "go version"
	if fixture == "javascript" {
		command = "node --version"
	}
	_, code, method, err := runner.RunCmd(context.Background(), sandbox.CmdRunnerCfg{CallerType: "builtin", Command: command, WorkDir: dir, AllowedPaths: []string{dir}, NetworkBlock: true, TimeoutMs: 30000})
	if method != "bwrap" && method != "seatbelt" {
		t.Fatalf("sandbox method changed after a successful physical probe: %s", method)
	}
	if err != nil {
		t.Fatalf("toolchain probe sandbox failed: command=%q err=%v", command, err)
	}
	if code == 127 {
		return false
	}
	if code != 0 {
		t.Fatalf("toolchain probe failed unexpectedly: command=%q code=%d err=%v", command, code, err)
	}
	return true
}

func initFixtureGit(t *testing.T, repo string) {
	t.Helper()
	gitFixtureCommand(t, repo, "init", "-q")
	gitFixtureCommand(t, repo, "config", "user.email", "fixture@example.invalid")
	gitFixtureCommand(t, repo, "config", "user.name", "fixture")
	gitFixtureCommand(t, repo, "add", ".")
	gitFixtureCommand(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "fixture base")
}

func gitFixtureOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("fixture git %v failed: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}

func gitFixtureCommand(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture git %v failed: %v: %s", args, err, output)
	}
}
