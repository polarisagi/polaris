package search

import (
	"fmt"
	"testing"
	"time"
)

func TestQueryEmbedCache_HitCopyAndSkipEmpty(t *testing.T) {
	c := newQueryEmbedCache()
	c.put("q", []float32{1, 2, 3})
	got, ok := c.get("q")
	if !ok || len(got) != 3 {
		t.Fatalf("应命中：%v %v", got, ok)
	}
	got[0] = 99 // 调用方就地修改（如归一化）不得污染缓存
	again, _ := c.get("q")
	if again[0] != 1 {
		t.Fatalf("缓存必须返回副本：%v", again)
	}
	c.put("empty", nil)
	if _, ok := c.get("empty"); ok {
		t.Fatal("空向量（嵌入失败/放弃）不得被缓存")
	}
}

func TestQueryEmbedCache_Bounded(t *testing.T) {
	c := newQueryEmbedCache()
	for i := 0; i < embedCacheMax*3; i++ {
		c.put(fmt.Sprintf("q%d", i), []float32{float32(i)})
	}
	if len(c.items) > embedCacheMax || len(c.order) > embedCacheMax {
		t.Fatalf("超出上限：items=%d order=%d", len(c.items), len(c.order))
	}
	if _, ok := c.get("q0"); ok {
		t.Fatal("最旧条目应已被淘汰")
	}
	if _, ok := c.get(fmt.Sprintf("q%d", embedCacheMax*3-1)); !ok {
		t.Fatal("最新条目应保留")
	}
}

func TestQueryEmbedCache_TTL(t *testing.T) {
	c := newQueryEmbedCache()
	now := time.Now()
	c.now = func() time.Time { return now }
	c.put("q", []float32{1})
	now = now.Add(embedCacheTTL - time.Second)
	if _, ok := c.get("q"); !ok {
		t.Fatal("TTL 内应命中")
	}
	now = now.Add(2 * time.Second)
	if _, ok := c.get("q"); ok {
		t.Fatal("TTL 后应失效（嵌入模型在线切换的陈旧窗口有界）")
	}
	if len(c.items) != 0 || len(c.order) != 0 {
		t.Fatalf("过期条目应同步清出 items 与淘汰队列：%d %d", len(c.items), len(c.order))
	}
}
