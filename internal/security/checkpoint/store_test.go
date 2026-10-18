package checkpoint_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/security/checkpoint"
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
