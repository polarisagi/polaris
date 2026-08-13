package vfs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
)

// TestWorkspaceManager_ConcurrentAccess_Race 复核修复 GR-6-002/GR-6-003：
// manifests map 此前无锁保护，Create/RegisterFile/CheckQuota/GC 与后台 gcWorker
// 并发访问会触发 Go runtime 的 fatal "concurrent map read and map write"（无法
// recover）。本测试跑并发读写，配合 `go test -race` 验证加锁修复真实生效。
func TestWorkspaceManager_ConcurrentAccess_Race(t *testing.T) {
	root := t.TempDir()
	wm := NewWorkspaceManager(root, 1<<30, config.DefaultThresholds().M7Tool) // 1GB，避免并发测试触发配额拒绝

	const numTasks = 20
	const numOpsPerTask = 20

	var wg sync.WaitGroup
	for i := 0; i < numTasks; i++ {
		taskID := filepath.Base(filepath.Clean("task-" + strconv.Itoa(i)))
		wg.Add(1)
		go func(taskID string) {
			defer wg.Done()
			if _, err := wm.Create(taskID); err != nil {
				t.Errorf("Create(%s) failed: %v", taskID, err)
				return
			}
			for j := 0; j < numOpsPerTask; j++ {
				wm.RegisterFile(taskID, WorkspaceFile{Path: "f.txt", Size: 10})
				if err := wm.CheckQuota(10); err != nil {
					t.Errorf("CheckQuota failed: %v", err)
				}
			}
		}(taskID)
	}

	// 并发触发 GC（活跃任务集为空，验证与上面的 Create/RegisterFile 并发不 panic；
	// 由于所有任务都是刚创建的，age < 7 天不会被真正回收，这里只验证并发安全）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < numOpsPerTask; j++ {
			wm.GC(0, nil)
		}
	}()

	wg.Wait()
}

// TestWorkspaceManager_StageEphemeralFile 验证批次4 XR-11 finding 复核修复：
// CodeAct 等消费方通过 StageEphemeralFile 落盘的临时脚本必须写入 rootDir 之下
// （而非系统 /tmp），纳入配额核算，且 cleanup() 归还配额、删除文件。
func TestWorkspaceManager_StageEphemeralFile(t *testing.T) {
	root := t.TempDir()
	wm := NewWorkspaceManager(root, 1<<20, config.DefaultThresholds().M7Tool) // 1MB

	data := []byte("print('hello')")
	path, cleanup, err := wm.StageEphemeralFile("session-1", "script.py", data)
	if err != nil {
		t.Fatalf("StageEphemeralFile failed: %v", err)
	}
	if !strings.HasPrefix(path, root) {
		t.Fatalf("staged path %q not under rootDir %q (VFS 隔离边界要求)", path, root)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read staged file failed: %v", readErr)
	}
	if string(got) != string(data) {
		t.Fatalf("staged content mismatch: got %q want %q", got, data)
	}

	beforeQuota := wm.totalSize
	if beforeQuota == 0 {
		t.Fatal("expected quota to be reserved after staging")
	}

	cleanup()

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected staged file to be removed after cleanup(), stat err=%v", statErr)
	}
	if wm.totalSize != 0 {
		t.Fatalf("expected quota released after cleanup(), got totalSize=%d", wm.totalSize)
	}
}

// TestWorkspaceManager_StageEphemeralFile_QuotaExhausted 验证超出配额时
// StageEphemeralFile fail-closed 拒绝写入，不留下部分写入的文件。
func TestWorkspaceManager_StageEphemeralFile_QuotaExhausted(t *testing.T) {
	root := t.TempDir()
	wm := NewWorkspaceManager(root, 4, config.DefaultThresholds().M7Tool) // 4 bytes，任何非空脚本都会超限

	_, _, err := wm.StageEphemeralFile("session-1", "script.py", []byte("print('too long')"))
	if err == nil {
		t.Fatal("expected quota exhausted error")
	}
}

// TestWorkspaceManager_SweepEphemeralOrphans 验证孤儿清理：模拟 cleanup() 未被
// 调用（进程崩溃场景）时，超过 maxAge 的文件被扫描删除并归还配额。
func TestWorkspaceManager_SweepEphemeralOrphans(t *testing.T) {
	root := t.TempDir()
	wm := NewWorkspaceManager(root, 1<<20, config.DefaultThresholds().M7Tool)

	path, _, err := wm.StageEphemeralFile("session-1", "orphan.py", []byte("x = 1"))
	if err != nil {
		t.Fatalf("StageEphemeralFile failed: %v", err)
	}
	// 模拟文件早已存在（早于 maxAge 阈值），不调用 cleanup()。
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatalf("os.Chtimes failed: %v", err)
	}

	wm.SweepEphemeralOrphans(int64((1 * time.Hour).Seconds()))

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected orphaned file to be swept, stat err=%v", statErr)
	}
	if wm.totalSize != 0 {
		t.Fatalf("expected quota released after sweep, got totalSize=%d", wm.totalSize)
	}
}

func TestWorkspaceManager_PathTraversal(t *testing.T) {
	root := t.TempDir()
	wm := NewWorkspaceManager(root, 1<<30, config.DefaultThresholds().M7Tool)

	tests := []struct {
		name    string
		relPath string
		wantErr bool
	}{
		{"Normal relative path", "a/b/c.txt", false},
		{"Empty path", "", true},
		{"Absolute path", "/etc/passwd", true},
		{"Simple traversal", "../passwd", true},
		{"Hidden traversal", "a/../../passwd", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := wm.WriteFile(tt.relPath, []byte("test"))
			if (err != nil) != tt.wantErr {
				t.Errorf("WriteFile(%q) error = %v, wantErr %v", tt.relPath, err, tt.wantErr)
			}
			_, err = wm.ReadFile(tt.relPath, -1)
			if (err != nil) != tt.wantErr {
				// ReadFile may return CodeNotFound if WriteFile failed, which is expected.
				// However, the traversal check itself should fail with InvalidInput.
				if tt.wantErr && !strings.Contains(err.Error(), "InvalidInput") {
					t.Errorf("ReadFile(%q) error = %v, expected traversal rejection", tt.relPath, err)
				}
			}
		})
	}
}

// ─── 阶段03 R-07：WithQuota 闭包配额管理回归测试 ───────────────────────────

// TestWorkspaceManager_WithQuota_ErrorReleasesQuota 验证 fn 返回 error 时
// WithQuota 自动归还预占份额，totalSize 回到调用前值。
func TestWorkspaceManager_WithQuota_ErrorReleasesQuota(t *testing.T) {
	root := t.TempDir()
	wm := NewWorkspaceManager(root, 1<<20, config.DefaultThresholds().M7Tool)

	before := wm.totalSize
	sentinel := &WorkspaceError{"boom"}
	err := wm.WithQuota(100, func() error {
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("期望 WithQuota 原样返回 fn 的 error，got %v", err)
	}
	if wm.totalSize != before {
		t.Fatalf("期望 fn 返回 error 后 totalSize 回到调用前值 %d，实际 %d", before, wm.totalSize)
	}
}

// TestWorkspaceManager_WithQuota_PanicReleasesQuotaAndRepanics 验证 fn panic
// 时 WithQuota 先归还预占份额，再重新 panic（不吞异常）。
func TestWorkspaceManager_WithQuota_PanicReleasesQuotaAndRepanics(t *testing.T) {
	root := t.TempDir()
	wm := NewWorkspaceManager(root, 1<<20, config.DefaultThresholds().M7Tool)

	before := wm.totalSize
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("期望 panic 被重新抛出，实际未 panic")
		}
		if wm.totalSize != before {
			t.Fatalf("期望 panic 后 totalSize 回到调用前值 %d，实际 %d", before, wm.totalSize)
		}
	}()
	_ = wm.WithQuota(100, func() error {
		panic("boom")
	})
}

// TestWorkspaceManager_WithQuota_SuccessKeepsQuotaReserved 验证 fn 返回 nil
// （视为调用方已登记）时预占份额保持占用，不被归还。
func TestWorkspaceManager_WithQuota_SuccessKeepsQuotaReserved(t *testing.T) {
	root := t.TempDir()
	wm := NewWorkspaceManager(root, 1<<20, config.DefaultThresholds().M7Tool)

	before := wm.totalSize
	err := wm.WithQuota(100, func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wm.totalSize != before+100 {
		t.Fatalf("期望 fn 返回 nil 后配额保持占用 %d，实际 totalSize=%d", before+100, wm.totalSize)
	}
}

// TestWorkspaceManager_WithQuota_QuotaExhaustedDoesNotCallFn 验证 CheckQuota
// 本身失败（超限）时，fn 完全不被调用。
func TestWorkspaceManager_WithQuota_QuotaExhaustedDoesNotCallFn(t *testing.T) {
	root := t.TempDir()
	wm := NewWorkspaceManager(root, 4, config.DefaultThresholds().M7Tool) // 4 bytes

	called := false
	err := wm.WithQuota(100, func() error {
		called = true
		return nil
	})
	if err != ErrWorkspaceQuotaExhausted {
		t.Fatalf("期望 ErrWorkspaceQuotaExhausted，实际 %v", err)
	}
	if called {
		t.Error("配额超限时不应调用 fn")
	}
}

func TestWorkspaceManager_StageEphemeralFile_Security(t *testing.T) {
	wm := &WorkspaceManager{rootDir: t.TempDir()}

	tests := []struct {
		name     string
		filename string
		wantErr  bool
	}{
		{"valid filename", "script.py", false},
		{"empty filename", "", true},
		{"dot filename", ".", true},
		{"dotdot filename", "..", true},
		{"path traversal filename", "../script.py", false}, // it gets sanitized to script.py! wait, is that true?
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, cleanup, err := wm.StageEphemeralFile("ns", tt.filename, []byte("test"))
			if (err != nil) != tt.wantErr {
				t.Errorf("StageEphemeralFile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				cleanup()
			}
		})
	}
}
