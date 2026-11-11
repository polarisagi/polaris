package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestGitDiff(t *testing.T) {
	tmpDir := t.TempDir()
	setGitTestEnv(t, tmpDir)

	// Init git repo
	runGitStatusTest(t, tmpDir, "init", "-b", "main")
	runGitStatusTest(t, tmpDir, "config", "user.email", "test@test.com")
	runGitStatusTest(t, tmpDir, "config", "user.name", "test")

	file := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(file, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitStatusTest(t, tmpDir, "add", "test.txt")
	runGitStatusTest(t, tmpDir, "commit", "-m", "init")
	if err := os.WriteFile(file, []byte("hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	diffFn := MakeGitDiffFn([]string{tmpDir}, false, "")
	ctx := context.Background()

	// Invalid json
	_, err := diffFn(ctx, []byte(`{invalid`))
	if err == nil {
		t.Fatal("expected err")
	}

	out, err := diffFn(ctx, mustJSON(t, map[string]string{"path": tmpDir}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid json")
	}
	if raw, ok := res["raw"].(string); !ok || !strings.Contains(raw, "+hello world") {
		t.Fatalf("expected raw diff to include changed line: %#v", res["raw"])
	}
	if len(res["files"].([]any)) != 1 || res["lines_added"].(float64) != 1 || res["lines_removed"].(float64) != 1 {
		t.Fatalf("unexpected diff stats: %#v", res)
	}
	refOut, err := diffFn(ctx, mustJSON(t, map[string]string{"path": tmpDir, "ref1": "HEAD"}))
	if err != nil || !strings.Contains(string(refOut), "hello") {
		t.Fatalf("ref1-only diff should be accepted, out=%s err=%v", refOut, err)
	}
	fileOut, err := diffFn(ctx, mustJSON(t, map[string]any{"path": tmpDir, "file": "test.txt"}))
	if err != nil {
		t.Fatalf("file-filtered diff: %v", err)
	}
	var filtered map[string]any
	if err := json.Unmarshal(fileOut, &filtered); err != nil || len(filtered["files"].([]any)) != 1 || filtered["lines_added"].(float64) != 1 {
		t.Fatalf("file filter lost stats: %#v err=%v", filtered, err)
	}
}

func TestGitDiffRejectsOptionAndEscapingInputs(t *testing.T) {
	tmpDir := t.TempDir()
	setGitTestEnv(t, tmpDir)
	runGitStatusTest(t, tmpDir, "init", "-b", "main")
	fn := MakeGitDiffFn([]string{tmpDir}, false, "")
	for _, input := range []map[string]any{
		{"path": tmpDir, "ref1": "--output=/tmp/pwned"},
		{"path": tmpDir, "ref2": "HEAD"},
		{"path": tmpDir, "file": "/etc/passwd"},
		{"path": tmpDir, "file": "../escape"},
	} {
		if _, err := fn(context.Background(), mustJSON(t, input)); err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
			t.Errorf("expected rejection for %#v", input)
		}
	}
	if !validGitDiffFilePath("name..with..dots.go") || !validGitDiffFilePath("src/..cache/file.go") {
		t.Fatal("legal names containing '..' must remain accepted")
	}
}

func TestGitDiffDisablesExternalDiff(t *testing.T) {
	root := t.TempDir()
	setGitTestEnv(t, root)
	runGitStatusTest(t, root, "init", "-b", "main")
	runGitStatusTest(t, root, "config", "user.email", "test@example.invalid")
	runGitStatusTest(t, root, "config", "user.name", "test")
	marker := filepath.Join(root, "external-ran")
	helper := filepath.Join(root, "external-diff")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf ran > \""+marker+"\"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitStatusTest(t, root, "config", "diff.external", helper)
	runGitStatusTest(t, root, "config", "diff.custom.textconv", helper)
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.txt diff=custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitStatusTest(t, root, "add", ".gitattributes", "file.txt")
	runGitStatusTest(t, root, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fn := MakeGitDiffFn([]string{root}, false, "")
	if _, err := fn(context.Background(), mustJSON(t, map[string]string{"path": root})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("external diff or textconv helper ran: stat err=%v", err)
	}
}

func TestGitDiffLiteralPathspecAndBinaryNumstat(t *testing.T) {
	root := t.TempDir()
	setGitTestEnv(t, root)
	runGitStatusTest(t, root, "init", "-b", "main")
	runGitStatusTest(t, root, "config", "user.email", "test@example.invalid")
	runGitStatusTest(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	runGitStatusTest(t, root, "add", "outside.txt", "binary.bin")
	runGitStatusTest(t, root, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0, 1, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	fn := MakeGitDiffFn([]string{root}, false, "")
	magic, err := fn(context.Background(), mustJSON(t, map[string]string{"path": root, "file": ":(top)outside.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	var magicResult struct {
		Files []gitDiffStat `json:"files"`
	}
	if err := json.Unmarshal(magic, &magicResult); err != nil || len(magicResult.Files) != 0 {
		t.Fatalf("magic pathspec must remain a literal filename: %+v err=%v", magicResult, err)
	}
	binary, err := fn(context.Background(), mustJSON(t, map[string]string{"path": root, "file": "binary.bin"}))
	if err != nil {
		t.Fatalf("binary numstat must be accepted: %v", err)
	}
	var binaryResult struct {
		Files []gitDiffStat `json:"files"`
	}
	if err := json.Unmarshal(binary, &binaryResult); err != nil || len(binaryResult.Files) != 1 || binaryResult.Files[0].Path != "binary.bin" || binaryResult.Files[0].Added != 0 || binaryResult.Files[0].Removed != 0 {
		t.Fatalf("unexpected binary numstat: %+v err=%v", binaryResult, err)
	}
}

func TestGitCommit(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	exec.Command("git", "-C", tmpDir, "init").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "test").Run()

	file := filepath.Join(tmpDir, "test.txt")
	exec.Command("sh", "-c", "echo 'hello' > "+file).Run()

	commitFn := MakeGitCommitFn([]string{tmpDir}, false, "")
	ctx := context.Background()

	args := `{"path": "` + tmpDir + `", "message": "init", "files": ["test.txt"]}`
	out, err := commitFn(ctx, []byte(args))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid json")
	}
	if res["hash"] == "" {
		t.Fatalf("expected hash")
	}
}
