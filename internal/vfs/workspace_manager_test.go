package vfs

import (
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// TestWorkspaceManager_ConcurrentAccess_Race 复核修复 GR-6-002/GR-6-003：
// manifests map 此前无锁保护，Create/RegisterFile/CheckQuota/GC 与后台 gcWorker
// 并发访问会触发 Go runtime 的 fatal "concurrent map read and map write"（无法
// recover）。本测试跑并发读写，配合 `go test -race` 验证加锁修复真实生效。
func TestWorkspaceManager_ConcurrentAccess_Race(t *testing.T) {
	root := t.TempDir()
	wm := NewWorkspaceManager(root, 1<<30) // 1GB，避免并发测试触发配额拒绝

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
