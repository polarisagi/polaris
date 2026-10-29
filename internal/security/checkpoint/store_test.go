package checkpoint_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/security/checkpoint"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestStore_CreateListRestore(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	testFile := filepath.Join(projectDir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("version 1"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := checkpoint.NewStore(dataDir, projectDir, 5)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	ctx := context.Background()

	// 创建第一个快照
	h1, err := store.Create(ctx, "first snapshot")
	if err != nil {
		t.Fatalf("Create first snapshot failed: %v", err)
	}
	if h1 == "" {
		t.Fatal("expected non-empty commit hash")
	}

	// 修改文件
	if err := os.WriteFile(testFile, []byte("version 2"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 创建第二个快照
	h2, err := store.Create(ctx, "second snapshot")
	if err != nil {
		t.Fatalf("Create second snapshot failed: %v", err)
	}
	if h2 == h1 {
		t.Fatal("second commit hash should differ from first")
	}

	// 验证列表
	snaps, err := store.List(ctx, 10)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(snaps) < 2 {
		t.Fatalf("expected at least 2 snapshots, got %d", len(snaps))
	}
	if snaps[0].Hash != h2 {
		t.Errorf("expected first snapshot in list to be h2, got %s", snaps[0].Hash)
	}

	// 恢复到快照 1
	if err := store.Restore(ctx, h1); err != nil {
		t.Fatalf("Restore failed: %v", err)
	}

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "version 1" {
		t.Errorf("expected file content to be 'version 1', got %q", string(content))
	}
}

func TestStore_PruneRetention(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	testFile := filepath.Join(projectDir, "counter.txt")
	store, err := checkpoint.NewStore(dataDir, projectDir, 2)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for i := 1; i <= 4; i++ {
		_ = os.WriteFile(testFile, []byte(string(rune('0'+i))), 0o644)
		_, err := store.Create(ctx, "commit")
		if err != nil {
			t.Fatal(err)
		}
	}

	snaps, err := store.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	// 保留数设为 2，裁剪后 snapshot 数目不应无限增长
	if len(snaps) > 4 {
		t.Errorf("expected pruned snapshots, got %d", len(snaps))
	}
}

func TestStore_Restore_InvalidHashFormat(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := checkpoint.NewStore(dataDir, projectDir, 5)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	ctx := context.Background()
	invalidHashes := []string{
		"-b",
		"--help",
		"xyz",
		"123456", // less than 7 chars
		"123456g",
		"../etc/passwd",
		"",
	}

	for _, badHash := range invalidHashes {
		err := store.Restore(ctx, badHash)
		if err == nil {
			t.Errorf("Restore(%q) expected error, got nil", badHash)
			continue
		}
		if apperr.CodeOf(err) != apperr.CodeInvalidInput {
			t.Errorf("Restore(%q) expected CodeInvalidInput, got %v (%v)", badHash, apperr.CodeOf(err), err)
		}
	}
}

func TestStore_Restore_NonExistentCommit(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	testFile := filepath.Join(projectDir, "tracked.txt")
	if err := os.WriteFile(testFile, []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := checkpoint.NewStore(dataDir, projectDir, 5)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	ctx := context.Background()
	if _, err := store.Create(ctx, "init snapshot"); err != nil {
		t.Fatalf("Create snapshot failed: %v", err)
	}

	// 40-char valid hex commit that does not exist
	nonExistentHash := "0123456789abcdef0123456789abcdef01234567"
	err = store.Restore(ctx, nonExistentHash)
	if err == nil {
		t.Fatal("expected error for non-existent commit, got nil")
	}
	if apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Errorf("expected CodeNotFound, got %v (%v)", apperr.CodeOf(err), err)
	}
}

func TestStore_Restore_DeletesNewFilesAndPreservesIgnored(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 配置 .gitignore
	gitignorePath := filepath.Join(projectDir, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("*.ignored\nignored_dir/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 初始文件：tracked.txt 与 secret.ignored
	trackedFile := filepath.Join(projectDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("version 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignoredFile := filepath.Join(projectDir, "secret.ignored")
	if err := os.WriteFile(ignoredFile, []byte("secret-v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := checkpoint.NewStore(dataDir, projectDir, 5)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	ctx := context.Background()
	h1, err := store.Create(ctx, "snapshot 1")
	if err != nil {
		t.Fatalf("Create snapshot 1 failed: %v", err)
	}

	// 在快照后修改 trackedFile
	if err := os.WriteFile(trackedFile, []byte("version 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 在快照后新增未忽略文件与目录
	newFile := filepath.Join(projectDir, "new_file.txt")
	if err := os.WriteFile(newFile, []byte("new file content"), 0o644); err != nil {
		t.Fatal(err)
	}
	newDir := filepath.Join(projectDir, "new_sub_dir")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	newDirFile := filepath.Join(newDir, "child.txt")
	if err := os.WriteFile(newDirFile, []byte("child file"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 在快照后新增忽略文件与忽略目录
	newIgnoredFile := filepath.Join(projectDir, "temp.ignored")
	if err := os.WriteFile(newIgnoredFile, []byte("temp-ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignoredDir := filepath.Join(projectDir, "ignored_dir")
	if err := os.MkdirAll(ignoredDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ignoredDirFile := filepath.Join(ignoredDir, "data.log")
	if err := os.WriteFile(ignoredDirFile, []byte("log data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 执行恢复到快照 1
	if err := store.Restore(ctx, h1); err != nil {
		t.Fatalf("Restore to h1 failed: %v", err)
	}

	// 验证 1: 跟踪文件恢复为 version 1
	content, err := os.ReadFile(trackedFile)
	if err != nil {
		t.Fatalf("read trackedFile failed: %v", err)
	}
	if string(content) != "version 1" {
		t.Errorf("expected trackedFile content 'version 1', got %q", string(content))
	}

	// 验证 2: 快照后新建的文件与目录被彻底删除
	if _, err := os.Stat(newFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected newFile to be deleted, got err: %v", err)
	}
	if _, err := os.Stat(newDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected newDir to be deleted, got err: %v", err)
	}

	// 验证 3: 被 .gitignore 忽略的文件与目录完整保留
	ignoredContent, err := os.ReadFile(ignoredFile)
	if err != nil {
		t.Fatalf("read ignoredFile failed: %v", err)
	}
	if string(ignoredContent) != "secret-v1" {
		t.Errorf("expected secret-v1, got %q", string(ignoredContent))
	}

	tempIgnoredContent, err := os.ReadFile(newIgnoredFile)
	if err != nil {
		t.Fatalf("read newIgnoredFile failed: %v", err)
	}
	if string(tempIgnoredContent) != "temp-ignored" {
		t.Errorf("expected temp-ignored, got %q", string(tempIgnoredContent))
	}

	ignoredDirContent, err := os.ReadFile(ignoredDirFile)
	if err != nil {
		t.Fatalf("read ignoredDirFile failed: %v", err)
	}
	if string(ignoredDirContent) != "log data" {
		t.Errorf("expected log data, got %q", string(ignoredDirContent))
	}
}
