package surprise

import (
	"fmt"
	"testing"
)

// submitAndWait 提交一次计算并等待结果落桶（ResultCh 在落桶之后才写入）。
func submitAndWait(t *testing.T, c *SurpriseCalculator, taskID string, seq []string) float64 {
	t.Helper()
	req := &CalcRequest{TaskID: taskID, ToolSeq: seq, ResultCh: make(chan float64, 1)}
	if !c.Submit(req) {
		t.Fatal("submit should succeed")
	}
	return <-req.ResultCh
}

func TestCurrentSurprise_IsolatedPerTask(t *testing.T) {
	c := NewSurpriseCalculator(nil)
	defer c.Close()

	if got := c.CurrentSurprise("never-seen"); got != 0.5 {
		t.Fatalf("无历史任务应返回中性 0.5，got %f", got)
	}
	// bash/computer_use 走 Tier-0 启发式加权，惊异更高；read 序列更低——两任务结果必须各归各桶。
	high := submitAndWait(t, c, "task-high", []string{"bash", "computer_use"})
	low := submitAndWait(t, c, "task-low", []string{"read", "read"})
	if high == low {
		t.Fatalf("测试前提失败：两序列结果应不同 high=%f low=%f", high, low)
	}
	if got := c.CurrentSurprise("task-high"); got != high {
		t.Errorf("task-high 应读到自己的结果 %f，got %f", high, got)
	}
	if got := c.CurrentSurprise("task-low"); got != low {
		t.Errorf("task-low 应读到自己的结果 %f，got %f", low, got)
	}
	if got := c.CurrentSurprise(""); got != 0.5 {
		t.Errorf("空 taskID 不落桶、读中性值，got %f", got)
	}
}

func TestCurrentSurprise_EWMAWithinTask(t *testing.T) {
	c := NewSurpriseCalculator(nil)
	defer c.Close()
	r1 := submitAndWait(t, c, "t", []string{"bash", "computer_use"})
	r2 := submitAndWait(t, c, "t", []string{"read", "read"})
	want := 0.8*r1 + 0.2*r2
	if got := c.CurrentSurprise("t"); diff(got, want) > 1e-9 {
		t.Fatalf("同任务应按 EWMA(0.2) 合并：want %f got %f", want, got)
	}
}

func diff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

func TestTaskBuckets_BoundedLRU(t *testing.T) {
	b := newTaskBuckets(3)
	for i := range 5 {
		b.observe(fmt.Sprintf("t%d", i), float64(i)/10)
	}
	if b.size() != 3 {
		t.Fatalf("桶数应封顶 3，got %d", b.size())
	}
	if _, ok := b.get("t0"); ok {
		t.Error("最旧的 t0 应被淘汰")
	}
	if _, ok := b.get("t1"); ok {
		t.Error("次旧的 t1 应被淘汰")
	}
	if v, ok := b.get("t4"); !ok || v != 0.4 {
		t.Errorf("最新桶应保留：%v %v", v, ok)
	}
	// 更新 t2 使其成为最新，再插入新桶时淘汰的应是 t3 而不是 t2。
	b.observe("t2", 0.9)
	b.observe("t5", 0.5)
	if _, ok := b.get("t2"); !ok {
		t.Error("刚更新过的 t2 不应被淘汰")
	}
	if _, ok := b.get("t3"); ok {
		t.Error("最久未更新的 t3 应被淘汰")
	}
	if newTaskBuckets(0).limit != DefaultTaskBucketCap {
		t.Error("cap<=0 应回退默认上限")
	}
}

func TestCalculatorBucketsBounded(t *testing.T) {
	c := NewSurpriseCalculator(nil)
	defer c.Close()
	c.mu.Lock()
	c.buckets = newTaskBuckets(4)
	c.mu.Unlock()
	for i := range 10 {
		submitAndWait(t, c, fmt.Sprintf("task-%d", i), []string{"a", "b"})
	}
	c.mu.Lock()
	n := c.buckets.size()
	c.mu.Unlock()
	if n != 4 {
		t.Fatalf("计算器桶应有界，got %d", n)
	}
}

func TestMarkovWarmStart_FromSequences(t *testing.T) {
	seqs := [][]string{
		{"read", "edit", "bash"}, // 2 次转移
		{"read", "edit"},         // 1 次转移
		{"solo"},                 // 不足 2 个，无转移
	}
	m := NewMarkovMatrixFromSequences(seqs)
	if m.TotalTransitions() != 3 {
		t.Fatalf("TotalTransitions want 3 got %f", m.TotalTransitions())
	}
	if m.counts["read"]["edit"] != 2 {
		t.Errorf("read->edit want 2 got %f", m.counts["read"]["edit"])
	}
	// 会话之间不连边：edit(会话1末尾 bash) 与下一会话 read 之间不应有 bash->read。
	if m.counts["bash"]["read"] != 0 {
		t.Error("跨会话不应连边")
	}

	c := NewSurpriseCalculator(nil)
	defer c.Close()
	c.WithMarkovMatrix(m)
	c.mu.Lock()
	got := c.markov.TotalTransitions()
	c.mu.Unlock()
	if got <= 0 {
		t.Fatal("warm-start 后 TotalTransitions 应 > 0")
	}
}

func TestSurpriseCalculator_CloseStopsWorkers(t *testing.T) {
	c := NewSurpriseCalculator(nil)
	c.Close()
	c.Close() // 幂等：cancel 可重复调用
}
