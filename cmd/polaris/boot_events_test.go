package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol/repo"
)

func TestStoreEventWriter_WriteStateTransEvent(t *testing.T) {
	tr := newTestTrajectoryRepo(t)
	w := newStoreEventWriter(tr)

	w.WriteStateTransEvent("sess-1", "S_PLAN")

	rows, err := tr.ListBySession(context.Background(), "sess-1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d err=%v", len(rows), err)
	}
	r := rows[0]
	if r.EventType != "S_PLAN" || r.Seq != 1 || r.ToolName != "" || r.ToolOK != nil || r.LatencyMs != -1 {
		t.Errorf("状态迁移行不应带工具列: %+v", r)
	}
}

func TestStoreEventWriter_WriteLLMCallEvent(t *testing.T) {
	tr := newTestTrajectoryRepo(t)
	w := newStoreEventWriter(tr)

	w.WriteLLMCallEvent("sess-1", map[string]any{"prompt": "hello"}, map[string]any{"output": "world"})

	rows, _ := tr.ListBySession(context.Background(), "sess-1")
	if len(rows) != 1 || rows[0].EventType != repo.TrajectoryEventLLMCall {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	var val map[string]map[string]any
	if err := json.Unmarshal([]byte(rows[0].Payload), &val); err != nil {
		t.Fatal(err)
	}
	if val["request"]["prompt"] != "hello" || val["response"]["output"] != "world" {
		t.Errorf("payload 键应保持 request/response: %v", val)
	}
}

func TestStoreEventWriter_WriteToolCallEvent(t *testing.T) {
	tr := newTestTrajectoryRepo(t)
	w := newStoreEventWriter(tr)

	w.WriteToolCallEvent("sess-1", "read_file", map[string]any{"arg1": "a"}, map[string]any{"res1": "b"}, true, 17)
	w.WriteToolCallEvent("sess-1", "bash", nil, map[string]any{"error": "boom"}, false, 0)

	rows, _ := tr.ListBySession(context.Background(), "sess-1")
	if len(rows) != 2 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].ToolName != "read_file" || rows[0].ToolOK == nil || !*rows[0].ToolOK || rows[0].LatencyMs != 17 {
		t.Errorf("成功工具行列值不符: %+v", rows[0])
	}
	if rows[1].ToolOK == nil || *rows[1].ToolOK || rows[1].LatencyMs != 0 {
		t.Errorf("失败工具行 tool_ok 应为 false、耗时 0（非 NULL）: %+v", rows[1])
	}
	var val map[string]any
	_ = json.Unmarshal([]byte(rows[0].Payload), &val)
	if val["tool"] != "read_file" || val["args"] == nil || val["result"] == nil {
		t.Errorf("payload 键应保持 tool/args/result: %v", val)
	}
}

func TestStoreEventWriter_EmptySessionOrNilStoreIsNoop(t *testing.T) {
	tr := newTestTrajectoryRepo(t)
	newStoreEventWriter(tr).WriteStateTransEvent("", "S_PLAN")
	if rows, _ := tr.ListBySession(context.Background(), ""); len(rows) != 0 {
		t.Fatalf("空 sessionID 不应落库: %+v", rows)
	}
	newStoreEventWriter(nil).WriteStateTransEvent("s", "S_PLAN") // 不 panic
}

// 同一会话并发写入：seq 必须从 1 连续无重复，不再依赖纳秒时间戳排序。
func TestStoreEventWriter_ConcurrentSeqMonotonic(t *testing.T) {
	tr := newTestTrajectoryRepo(t)
	w := newStoreEventWriter(tr)
	const n = 40
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				w.WriteStateTransEvent("sess-c", "S_PLAN")
			} else {
				w.WriteToolCallEvent("sess-c", "t", nil, nil, true, 1)
			}
		}()
	}
	wg.Wait()
	rows, _ := tr.ListBySession(context.Background(), "sess-c")
	if len(rows) != n {
		t.Fatalf("丢事件: got %d want %d", len(rows), n)
	}
	for i, r := range rows {
		if r.Seq != int64(i+1) {
			t.Fatalf("seq 不连续: idx=%d seq=%d", i, r.Seq)
		}
	}
}
