package automation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo 创建一个带初始提交的临时 git 仓库，返回其路径。
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("init\n"), 0644); err != nil {
		t.Fatalf("write README failed: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	return dir
}

func TestWorktreeManager_CommitChanges_SignsAuthor(t *testing.T) {
	baseDir := initTestRepo(t)
	tmpDir := t.TempDir()
	wm := NewWorktreeManager(baseDir, tmpDir)

	ctx := context.Background()
	wtDir, branchName, err := wm.PrepareWorktree(ctx, "test-run-1")
	if err != nil {
		t.Fatalf("PrepareWorktree failed: %v", err)
	}
	defer wm.Cleanup(wtDir)

	if err := os.WriteFile(filepath.Join(wtDir, "new.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	hasChanges, diffSummary, err := wm.CommitChanges(ctx, wtDir, branchName)
	if err != nil {
		t.Fatalf("CommitChanges failed: %v", err)
	}
	if !hasChanges {
		t.Fatal("expected hasChanges=true")
	}
	if !strings.Contains(diffSummary, "new.txt") {
		t.Errorf("expected diff summary to mention new.txt, got: %s", diffSummary)
	}

	// 验证提交署名符合 CLAUDE.md 强制规则
	cmd := exec.CommandContext(ctx, "git", "log", "-1", "--format=%an <%ae>")
	cmd.Dir = wtDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	got := strings.TrimSpace(string(out))
	if got != commitAuthor {
		t.Errorf("expected commit author %q, got %q", commitAuthor, got)
	}
}

func TestWorktreeManager_CommitChanges_NoChanges(t *testing.T) {
	baseDir := initTestRepo(t)
	tmpDir := t.TempDir()
	wm := NewWorktreeManager(baseDir, tmpDir)

	ctx := context.Background()
	wtDir, branchName, err := wm.PrepareWorktree(ctx, "test-run-2")
	if err != nil {
		t.Fatalf("PrepareWorktree failed: %v", err)
	}
	defer wm.Cleanup(wtDir)

	hasChanges, _, err := wm.CommitChanges(ctx, wtDir, branchName)
	if err != nil {
		t.Fatalf("CommitChanges failed: %v", err)
	}
	if hasChanges {
		t.Error("expected hasChanges=false when worktree is clean")
	}
}

func TestWorktreeManager_CreatePullRequest_SkipsWithoutToken(t *testing.T) {
	baseDir := initTestRepo(t)
	tmpDir := t.TempDir()
	wm := NewWorktreeManager(baseDir, tmpDir)

	t.Setenv("GITHUB_TOKEN", "")
	if err := wm.CreatePullRequest(context.Background(), "some-branch", "title", "body"); err != nil {
		t.Errorf("expected nil error (fail-open) when GITHUB_TOKEN unset, got: %v", err)
	}
}
