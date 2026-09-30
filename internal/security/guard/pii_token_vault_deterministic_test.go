package guard

import (
	"fmt"
	"sync"
	"testing"
)

// ADR-0105 决策十一：确定性会话内令牌。

func TestPIITokenVault_DeterministicWithinTask(t *testing.T) {
	v := NewPIITokenVault()
	a1 := v.TokenizeForTask("t1", "alice@example.com")
	a2 := v.TokenizeForTask("t1", "alice@example.com")
	if a1 != a2 {
		t.Fatalf("same task+original must reuse token: %s vs %s", a1, a2)
	}
	b := v.TokenizeForTask("t1", "bob@example.com")
	if b == a1 {
		t.Fatalf("different originals must get different tokens: %s", b)
	}
	// 大小写不同视为不同原文（有意不规范化，避免合并不同实体）。
	c := v.TokenizeForTask("t1", "Alice@example.com")
	if c == a1 {
		t.Fatalf("no normalization: case variant must not merge with %s", a1)
	}
	for tok, want := range map[string]string{a1: "alice@example.com", b: "bob@example.com", c: "Alice@example.com"} {
		got, err := v.ResolveForTask("t1", tok)
		if err != nil || got != want {
			t.Fatalf("resolve %s = %q,%v want %q", tok, got, err, want)
		}
	}
	// 重复令牌化不得增长正向映射（此前每次调用新增一条，会话内无界增长）。
	v.mu.RLock()
	n := len(v.tokens["t1"])
	v.mu.RUnlock()
	if n != 3 {
		t.Fatalf("forward map should hold 3 entries, got %d", n)
	}
}

func TestPIITokenVault_TaskIsolationUnlinkable(t *testing.T) {
	v := NewPIITokenVault()
	// 2^32 空间内两次独立随机相等的概率 ~2.3e-10，多 task 重复断言仍可忽略。
	for i := 0; i < 50; i++ {
		x := v.TokenizeForTask(fmt.Sprintf("ta-%d", i), "alice@example.com")
		y := v.TokenizeForTask(fmt.Sprintf("tb-%d", i), "alice@example.com")
		if x == y {
			t.Fatalf("different tasks must not share token for same original (iter %d): %s", i, x)
		}
		if _, err := v.ResolveForTask(fmt.Sprintf("tb-%d", i), x); err == nil {
			t.Fatalf("token of task ta must not resolve in task tb (iter %d)", i)
		}
	}
}

func TestPIITokenVault_ClearTaskClearsReverse(t *testing.T) {
	v := NewPIITokenVault()
	tok := v.TokenizeForTask("t1", "alice@example.com")
	v.TokenizeForTask("t2", "alice@example.com")
	v.ClearTask("t1")

	v.mu.RLock()
	_, fwd := v.tokens["t1"]
	_, rev := v.reverse["t1"]
	_, rev2 := v.reverse["t2"]
	v.mu.RUnlock()
	if fwd || rev {
		t.Fatalf("ClearTask must drop both maps: fwd=%v rev=%v", fwd, rev)
	}
	if !rev2 {
		t.Fatal("ClearTask must not touch other tasks")
	}
	if _, err := v.ResolveForTask("t1", tok); err == nil {
		t.Fatal("cleared token must fail-closed")
	}
	// 清理后同原文重新生成（旧令牌已不可解析，不得复活）。
	if got := v.TokenizeForTask("t1", "alice@example.com"); got == "" {
		t.Fatal("re-tokenize after clear must work")
	}
	if got := v.TokenizeForTask("t1", "alice@example.com"); got == tok {
		// 2^-32 概率，视为失败即可察觉反向映射残留。
		t.Fatal("reverse mapping leaked across ClearTask")
	}
}

func TestPIITokenVault_CollisionRetry(t *testing.T) {
	v := NewPIITokenVault()
	seq := []string{"aaaaaaaa", "aaaaaaaa", "aaaaaaaa", "bbbbbbbb"}
	i := 0
	v.randHex = func(int) string { s := seq[i]; i++; return s }

	t1 := v.TokenizeForTask("t", "first")  // 消费 seq[0]
	t2 := v.TokenizeForTask("t", "second") // seq[1]、seq[2] 碰撞，seq[3] 成功
	if t1 == t2 {
		t.Fatalf("collision must be retried, got duplicate %s", t1)
	}
	if got, _ := v.ResolveForTask("t", t1); got != "first" {
		t.Fatalf("first mapping overwritten: %q", got)
	}
	if got, _ := v.ResolveForTask("t", t2); got != "second" {
		t.Fatalf("second mapping wrong: %q", got)
	}
}

func TestPIITokenVault_TokenizeKnownValuesUsesReverse(t *testing.T) {
	v := NewPIITokenVault()
	tok := v.TokenizeForTask("t", "alice@example.com")
	got := v.TokenizeKnownValues("t", "echo alice@example.com done")
	want := "echo " + tok + " done"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPIITokenVault_ConcurrentDeterministic(t *testing.T) {
	v := NewPIITokenVault()
	const workers, values = 16, 20
	results := make([][values]string, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < values; i++ {
				results[w][i] = v.TokenizeForTask("t", fmt.Sprintf("user%d@example.com", i))
				if i%7 == 0 {
					v.TokenizeKnownValues("t", "x")
				}
			}
		}(w)
	}
	wg.Wait()
	for i := 0; i < values; i++ {
		for w := 1; w < workers; w++ {
			if results[w][i] != results[0][i] {
				t.Fatalf("value %d: worker %d got %s, worker 0 got %s", i, w, results[w][i], results[0][i])
			}
		}
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if len(v.tokens["t"]) != values || len(v.reverse["t"]) != values {
		t.Fatalf("maps diverged: fwd=%d rev=%d want %d", len(v.tokens["t"]), len(v.reverse["t"]), values)
	}
}
