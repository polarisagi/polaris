package guard

import (
	"fmt"
	"testing"
	"time"
)

// ADR-0105 决策十一（WP10 订正）：回合终态只"释放"不"清空"，同一会话的下一回合复用同一令牌。

func TestPIITokenVault_ReleaseTaskKeepsTokenAcrossTurns(t *testing.T) {
	v := NewPIITokenVault()
	first := v.TokenizeForTask("sess", "alice@example.com")
	v.ReleaseTask("sess") // 回合 1 终态
	second := v.TokenizeForTask("sess", "alice@example.com")
	if first != second {
		t.Fatalf("release 之后同一原文应得同一令牌（否则跨回合前缀缓存断开）：%s vs %s", first, second)
	}
	if got, err := v.ResolveForTask("sess", first); err != nil || got != "alice@example.com" {
		t.Fatalf("release 后映射仍应可还原：%q %v", got, err)
	}
}

func TestPIITokenVault_ReleaseTaskEvictsIdleNamespaces(t *testing.T) {
	v := NewPIITokenVault()
	clock := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	v.now = func() time.Time { return clock }
	tokOld := v.TokenizeForTask("old", "old@example.com")
	v.ReleaseTask("old")
	clock = clock.Add(piiVaultIdleTTL + time.Minute)
	tokNew := v.TokenizeForTask("new", "new@example.com")
	v.ReleaseTask("new") // 清扫：old 闲置超 TTL
	if _, err := v.ResolveForTask("old", tokOld); err == nil {
		t.Fatal("闲置超过 TTL 的命名空间应被回收")
	}
	if _, err := v.ResolveForTask("new", tokNew); err != nil {
		t.Fatalf("刚释放的命名空间不得被回收：%v", err)
	}
}

func TestPIITokenVault_ReleaseTaskBoundsNamespaceCount(t *testing.T) {
	v := NewPIITokenVault()
	clock := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	v.now = func() time.Time { return clock }
	for i := 0; i < piiVaultMaxTasks+10; i++ {
		clock = clock.Add(time.Second)
		id := fmt.Sprintf("s%04d", i)
		v.TokenizeForTask(id, id+"@example.com")
		v.ReleaseTask(id)
	}
	v.mu.RLock()
	n := len(v.tokens)
	_, oldestKept := v.tokens["s0000"]
	_, newestKept := v.tokens[fmt.Sprintf("s%04d", piiVaultMaxTasks+9)]
	v.mu.RUnlock()
	if n != piiVaultMaxTasks {
		t.Fatalf("命名空间数应被压到上限 %d，实际 %d", piiVaultMaxTasks, n)
	}
	if oldestKept || !newestKept {
		t.Fatalf("应按最久未用淘汰：oldest kept=%v newest kept=%v", oldestKept, newestKept)
	}
}
