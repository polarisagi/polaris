package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGitStatusParsesNULPathsAndUnbornBranch(t *testing.T) {
	root := t.TempDir()
	setGitTestEnv(t, root)
	runGitStatusTest(t, root, "init", "-b", "feature/status")
	items := []struct {
		path string
		data string
	}{{"space name.txt", "x"}, {"中文 name.txt", "y"}}
	if runtime.GOOS != "windows" {
		items = append(items, struct {
			path string
			data string
		}{"中文\nname.txt", "z"})
	}
	for _, item := range items {
		if err := os.WriteFile(filepath.Join(root, item.path), []byte(item.data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fn := MakeGitStatusFn([]string{root}, false, "")
	out, err := fn(context.Background(), mustJSON(t, map[string]string{"path": root}))
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	var result struct {
		Branch string          `json:"branch"`
		Files  []gitStatusFile `json:"files"`
		Clean  bool            `json:"clean"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.Branch != "feature/status" || result.Clean || len(result.Files) != len(items) {
		t.Fatalf("unexpected unborn status: %+v", result)
	}
	seen := map[string]bool{}
	for _, file := range result.Files {
		seen[file.Path] = file.IndexStatus == "?" && file.WorktreeStatus == "?"
	}
	if !seen["space name.txt"] || !seen["中文 name.txt"] || (runtime.GOOS != "windows" && !seen["中文\nname.txt"]) {
		t.Fatalf("NUL-separated paths were not preserved: %+v", result.Files)
	}
}

func TestGitStatusReportsRenameAndNonRepository(t *testing.T) {
	root := t.TempDir()
	setGitTestEnv(t, root)
	runGitStatusTest(t, root, "init", "-b", "main")
	runGitStatusTest(t, root, "config", "user.email", "test@example.invalid")
	runGitStatusTest(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitStatusTest(t, root, "add", "old.txt")
	runGitStatusTest(t, root, "commit", "-m", "initial")
	runGitStatusTest(t, root, "mv", "old.txt", "new name.txt")
	fn := MakeGitStatusFn([]string{root}, false, "")
	out, err := fn(context.Background(), mustJSON(t, map[string]string{"path": root}))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Files []gitStatusFile `json:"files"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "new name.txt" || result.Files[0].OriginalPath != "old.txt" {
		t.Fatalf("rename not preserved: %+v", result.Files)
	}
	nonRepo := t.TempDir()
	nonRepoFn := MakeGitStatusFn([]string{nonRepo}, false, "")
	if _, err := nonRepoFn(context.Background(), mustJSON(t, map[string]string{"path": nonRepo})); err == nil {
		t.Fatal("non-repository status should fail")
	}
}

func TestParseGitStatusPorcelainCleanIsEmptySlice(t *testing.T) {
	branch, files, err := parseGitStatusPorcelain([]byte("## main\x00"))
	if err != nil || branch != "main" || files == nil || len(files) != 0 {
		t.Fatalf("unexpected clean parse: branch=%q files=%#v err=%v", branch, files, err)
	}
	_, files, err = parseGitStatusPorcelain([]byte("## main\x00?? line\nbreak.txt\x00"))
	if err != nil || len(files) != 1 || files[0].Path != "line\nbreak.txt" {
		t.Fatalf("NUL parser must preserve embedded newlines: files=%+v err=%v", files, err)
	}
}

func runGitStatusTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func setGitTestEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "empty-gitconfig"))
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
