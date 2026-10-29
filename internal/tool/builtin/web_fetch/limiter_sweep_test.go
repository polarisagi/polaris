package web_fetch

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 过期条目超过阈值后必须被清扫，否则长期采集 last map 无界增长。
func TestHostLimiter_SweepsExpiredHosts(t *testing.T) {
	l := NewHostLimiter(func() time.Duration { return time.Millisecond })
	old := time.Now().Add(-time.Hour)
	for i := 0; i < maxTrackedHosts+10; i++ {
		l.last[fmt.Sprintf("h%d.example", i)] = old
	}
	if err := l.Wait(context.Background(), "fresh.example"); err != nil {
		t.Fatal(err)
	}
	if n := len(l.last); n != 1 {
		t.Fatalf("expected only fresh host retained, got %d entries", n)
	}
	if _, ok := l.last["fresh.example"]; !ok {
		t.Fatal("fresh host entry missing")
	}
}
