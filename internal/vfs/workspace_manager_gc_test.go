package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
)

// GR-6.1-009：重启重建清单时，只有 Create() 写过标记的目录才是任务工作区；
// episodic 溢出载荷目录（logs/）、临时脚本目录与墓碑目录都不能进入清单，
// 否则 7 天 GC 会把持久数据整目录删除。
func TestRebuildManifests_SkipsNonTaskDirs(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultThresholds().M7Tool

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wm := NewWorkspaceManagerWithContext(ctx, root, 1<<30, cfg)
	if _, err := wm.Create("task-1"); err != nil {
		t.Fatal(err)
	}
	if err := wm.WriteFile(filepath.Join("logs", "events", "e1.bin"), []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ephemeralScriptsSubdir), 0o700); err != nil {
		t.Fatal(err)
	}
	tomb := filepath.Join(root, "old-task"+tombstoneInfix+"1")
	if err := os.MkdirAll(tomb, 0o700); err != nil {
		t.Fatal(err)
	}

	wm2 := NewWorkspaceManagerWithContext(ctx, root, 1<<30, cfg)
	wm2.mu.RLock()
	keys := make([]string, 0, len(wm2.manifests))
	for k := range wm2.manifests {
		keys = append(keys, k)
	}
	wm2.mu.RUnlock()
	if len(keys) != 1 || keys[0] != "task-1" {
		t.Fatalf("manifests = %v, want only task-1", keys)
	}

	// 超期 GC 不得触碰 logs/ 持久载荷
	wm2.GC(time.Now().Unix()+int64(cfg.WorkspaceMaxAgeSeconds)+10, nil)
	if _, err := os.Stat(filepath.Join(root, "logs", "events", "e1.bin")); err != nil {
		t.Fatalf("episodic payload deleted by GC: %v", err)
	}
	// 遗留墓碑被重新入队回收
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(tomb); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stale tombstone not reclaimed after restart")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// GR-6.1-004：活跃集合查询失败时本轮必须跳过（fail-closed），不得删除工作区。
func TestRunGCOnce_ProviderErrorSkips(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultThresholds().M7Tool
	cfg.WorkspaceMaxAgeSeconds = -1 // 任何工作区都视为过期
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wm := NewWorkspaceManagerWithContext(ctx, root, 1<<30, cfg)
	dir, err := wm.Create("task-x")
	if err != nil {
		t.Fatal(err)
	}
	wm.runGCOnce(ctx, func(context.Context) ([]string, error) { return nil, errors.New("db down") })
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("workspace removed despite provider error: %v", err)
	}
	wm.runGCOnce(ctx, func(context.Context) ([]string, error) { return []string{"task-x"}, nil })
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("active workspace removed: %v", err)
	}
	wm.runGCOnce(ctx, func(context.Context) ([]string, error) { return nil, nil })
	wm.mu.RLock()
	_, still := wm.manifests["task-x"]
	wm.mu.RUnlock()
	if still {
		t.Fatal("expired inactive workspace not collected")
	}
}

// GR-6.1-003：ctx 取消后 gcWorker 必须退出。
func TestGCWorker_ExitsOnCancel(t *testing.T) {
	wm := &WorkspaceManager{gcCh: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { wm.gcWorker(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gcWorker did not exit after ctx cancel")
	}
}
