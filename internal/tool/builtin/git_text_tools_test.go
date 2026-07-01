package builtin

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitDiff(t *testing.T) {
	tmpDir := t.TempDir()

	// Init git repo
	exec.Command("git", "-C", tmpDir, "init").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "test").Run()

	file := filepath.Join(tmpDir, "test.txt")
	exec.Command("sh", "-c", "echo 'hello' > "+file).Run()
	exec.Command("git", "-C", tmpDir, "add", "test.txt").Run()
	exec.Command("git", "-C", tmpDir, "commit", "-m", "init").Run()

	exec.Command("sh", "-c", "echo 'hello world' > "+file).Run()

	diffFn := makeGitDiffFn([]string{tmpDir}, false, "")
	ctx := context.Background()

	// Invalid json
	_, err := diffFn(ctx, []byte(`{invalid`))
	if err == nil {
		t.Fatal("expected err")
	}

	args := `{"path": "` + tmpDir + `"}`
	out, err := diffFn(ctx, []byte(args))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid json")
	}
	if res["raw_diff"] == "" {
		t.Fatalf("expected raw diff")
	}
}

func TestGitCommit(t *testing.T) {
	tmpDir := t.TempDir()
	exec.Command("git", "-C", tmpDir, "init").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "test").Run()

	file := filepath.Join(tmpDir, "test.txt")
	exec.Command("sh", "-c", "echo 'hello' > "+file).Run()

	commitFn := makeGitCommitFn([]string{tmpDir}, false, "")
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
