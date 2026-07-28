package metrics

import (
	"fmt"
	"sync"
	"testing"
)

// TestCardinalityLimits — inv_M3_06 的 CI 校验手段（M03-Observability.md §0-ter/§2.1
// 显式声明 `go test -run TestCardinalityLimits` 为该不变量的强制门控）。
// 验证 cardinalityGuard 的 cap 截断语义：满 cap 后新值一律映射为 "<overflow>" 桶，
// 已入桶的值不受影响，防止高基数标签（session_id/task_id/trace_id）撑爆 Prometheus 内存。
func TestCardinalityLimits(t *testing.T) {
	t.Run("cap 内的值原样放行且可重复命中", func(t *testing.T) {
		g := newCardinalityGuard(3)
		for _, v := range []string{"a", "b", "c"} {
			if got := g.Allow(v); got != v {
				t.Fatalf("Allow(%q) = %q, want %q（未满 cap 不应截断）", v, got, v)
			}
		}
		// 重复命中已入桶的值不消耗额外容量
		if got := g.Allow("a"); got != "a" {
			t.Fatalf("Allow(\"a\") 二次调用 = %q, want \"a\"", got)
		}
	})

	t.Run("超过 cap 的新值映射为 <overflow>", func(t *testing.T) {
		g := newCardinalityGuard(3)
		for _, v := range []string{"a", "b", "c"} {
			g.Allow(v)
		}
		if got := g.Allow("d"); got != "<overflow>" {
			t.Fatalf("Allow(\"d\") = %q, want \"<overflow>\"（cap=3 已满）", got)
		}
		// 溢出不得挤掉既有值：已入桶的值仍原样返回
		if got := g.Allow("b"); got != "b" {
			t.Fatalf("溢出后 Allow(\"b\") = %q, want \"b\"（既有值不应被逐出）", got)
		}
		// 溢出也不得让容量泄漏：后续新值仍然溢出
		if got := g.Allow("e"); got != "<overflow>" {
			t.Fatalf("Allow(\"e\") = %q, want \"<overflow>\"", got)
		}
	})

	t.Run("进程级单例 cap=500（inv_M3_06 硬上限）", func(t *testing.T) {
		g := newCardinalityGuard(500)
		for i := range 500 {
			v := fmt.Sprintf("v%d", i)
			if got := g.Allow(v); got != v {
				t.Fatalf("第 %d 个值被提前截断: Allow(%q) = %q", i, v, got)
			}
		}
		if got := g.Allow("v500"); got != "<overflow>" {
			t.Fatalf("第 501 个值 = %q, want \"<overflow>\"（cap=500 是硬上限）", got)
		}
		// GetCardinalityGuard 单例的 cap 必须同为 500
		if singleton := GetCardinalityGuard(); singleton.cap != 500 {
			t.Fatalf("GetCardinalityGuard().cap = %d, want 500", singleton.cap)
		}
	})

	t.Run("并发 Allow 不超发容量", func(t *testing.T) {
		const cap = 50
		g := newCardinalityGuard(cap)
		var wg sync.WaitGroup
		for i := range 200 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				g.Allow(fmt.Sprintf("k%d", i))
			}(i)
		}
		wg.Wait()
		g.mu.Lock()
		n := len(g.order)
		g.mu.Unlock()
		if n != cap {
			t.Fatalf("并发写入后已入桶数 = %d, want %d（不得超发容量）", n, cap)
		}
	})
}

// TestToolCategory — inv_M3_06 受控映射：tool_name 必须先经 ToolCategory 归并为
// 低基数 label（builtin/mcp/skill），禁止把原始工具名直接当标签用。
func TestToolCategory(t *testing.T) {
	cases := map[string]string{
		"mcp:github":     "mcp",
		"mcp_filesystem": "mcp",
		"skill:review":   "skill",
		"sk_summarize":   "skill",
		"read_file":      "builtin",
		"":               "builtin",
		"mcp":            "builtin", // 长度不足 4，不匹配前缀规则
	}
	for name, want := range cases {
		if got := ToolCategory(name); got != want {
			t.Errorf("ToolCategory(%q) = %q, want %q", name, got, want)
		}
	}
}
