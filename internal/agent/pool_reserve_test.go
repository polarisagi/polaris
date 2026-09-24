package agent

import (
	"context"
	"testing"
	"time"
)

// TestPool_InteractiveReserve 复现 2026-09-25 实测：后台 headless 任务（等待上限
// 10 分钟）排满全部槽位，交互 Acquire（100ms）必然落空，用户只收到"系统当前负载
// 较高"。预留后，后台最多占 maxSize-reserve 个槽位，交互始终拿得到。
func TestPool_InteractiveReserve(t *testing.T) {
	pool := NewPool(newTestPoolAgent, 2).WithInteractiveReserve(1)

	_, bgRelease, err := pool.acquireInner(context.Background(), "bg-1", true)
	if err != nil {
		t.Fatalf("第一个后台任务应拿到槽位: %v", err)
	}
	defer bgRelease()

	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := pool.acquireInner(shortCtx, "bg-2", true); err == nil {
		t.Fatal("后台已用满其份额，第二个后台任务不得挤占交互预留槽位")
	}

	_, release, err := pool.Acquire(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("交互会话必须拿到预留槽位: %v", err)
	}
	release()
}

// TestPool_InteractiveReserve_SingleSlot 容量为 1 时后台仍至少可用 1 槽（下限），
// 否则单槽部署上所有后台任务永远无法执行。
func TestPool_InteractiveReserve_SingleSlot(t *testing.T) {
	pool := NewPool(newTestPoolAgent, 1).WithInteractiveReserve(1)
	_, release, err := pool.acquireInner(context.Background(), "bg-1", true)
	if err != nil {
		t.Fatalf("单槽部署后台至少可用 1 槽: %v", err)
	}
	release()
}
