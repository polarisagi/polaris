package mcp

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestMCPManager_CallToolAsync_And_GetAsyncTaskResult 验证 GD-08-001 tasks_cache
// 基本生命周期：CallToolAsync 立即返回 task_id（status=pending），dummy client
// 必然调用失败，后台 goroutine 应最终把状态更新为 AsyncTaskFailed 并记录错误信息。
func TestMCPManager_CallToolAsync_And_GetAsyncTaskResult(t *testing.T) {
	mgr := NewMCPManager(nil, http.DefaultClient, &mockPolicyGate{})

	testClient := NewMCPClient(MCPClientConfig{Trusted: true}, nil)
	mgr.mu.Lock()
	mgr.entries["fake-1"] = &mcpEntry{name: "fake-1", client: testClient}
	mgr.mu.Unlock()

	taskID, err := mgr.CallToolAsync(context.Background(), "fake-1", "tool1", nil)
	if err != nil {
		t.Fatalf("unexpected error from CallToolAsync: %v", err)
	}
	if taskID == "" {
		t.Fatalf("expected non-empty task_id")
	}

	result, ok := mgr.GetAsyncTaskResult(taskID)
	if !ok {
		t.Fatalf("expected task to be immediately queryable after CallToolAsync")
	}
	if result.Status != AsyncTaskPending && result.Status != AsyncTaskFailed {
		t.Fatalf("unexpected initial status: %v", result.Status)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		result, _ = mgr.GetAsyncTaskResult(taskID)
		if result.Status != AsyncTaskPending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if result.Status != AsyncTaskFailed {
		t.Errorf("expected AsyncTaskFailed after dummy client call, got %v", result.Status)
	}
	if result.Error == "" {
		t.Errorf("expected non-empty error message on failed task")
	}
}

func TestMCPManager_CallToolAsync_ServerNotFound(t *testing.T) {
	mgr := NewMCPManager(nil, http.DefaultClient, &mockPolicyGate{})
	if _, err := mgr.CallToolAsync(context.Background(), "non-existent", "tool1", nil); err == nil {
		t.Fatalf("expected error for non-existent server")
	}
}

func TestMCPManager_GetAsyncTaskResult_NotFound(t *testing.T) {
	mgr := NewMCPManager(nil, http.DefaultClient, &mockPolicyGate{})
	if _, ok := mgr.GetAsyncTaskResult("no-such-task"); ok {
		t.Errorf("expected ok=false for unknown task_id")
	}
}

// TestAsyncTaskCache_ExpiryAndSweep 验证 TTL 惰性淘汰（get 命中过期条目视为不存在）
// 与后台 sweep 的物理删除均按预期工作。
func TestAsyncTaskCache_ExpiryAndSweep(t *testing.T) {
	c := newAsyncTaskCache()

	c.put(&AsyncTaskResult{TaskID: "t1", Status: AsyncTaskDone, ExpiresAt: time.Now().Add(-time.Second)})
	if _, ok := c.get("t1"); ok {
		t.Errorf("expected expired task to be treated as not found by get()")
	}

	c.put(&AsyncTaskResult{TaskID: "t2", Status: AsyncTaskDone, ExpiresAt: time.Now().Add(-time.Second)})
	c.sweep()
	c.mu.RLock()
	_, exists := c.tasks["t2"]
	c.mu.RUnlock()
	if exists {
		t.Errorf("expected sweep() to physically remove expired task")
	}

	c.put(&AsyncTaskResult{TaskID: "t3", Status: AsyncTaskDone, ExpiresAt: time.Now().Add(time.Minute)})
	if _, ok := c.get("t3"); !ok {
		t.Errorf("expected non-expired task to be found")
	}
}
