package agent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

type execRecord struct {
	name          string
	args          string
	start         time.Time
	end           time.Time
	activeAtStart int32
}

// concurrencyMockToolExecutor 模拟带有不同并发能力与延迟的工具执行器。
type concurrencyMockToolExecutor struct {
	mu           sync.Mutex
	tools        map[string]types.Tool
	activeCount  atomic.Int32
	history      []execRecord
	delay        time.Duration
	overlapCheck atomic.Bool
}

func newConcurrencyMockExecutor(delay time.Duration) *concurrencyMockToolExecutor {
	return &concurrencyMockToolExecutor{
		tools: make(map[string]types.Tool),
		delay: delay,
	}
}

func (m *concurrencyMockToolExecutor) registerTool(t types.Tool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools[t.Name] = t
}

func (m *concurrencyMockToolExecutor) Lookup(name string) (types.Tool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tools[name]
	if !ok {
		return types.Tool{}, fmt.Errorf("tool %q not found", name)
	}
	return t, nil
}

func (m *concurrencyMockToolExecutor) ExecuteWithTaint(_ context.Context, name string, args []byte, _ types.TaintLevel) (*types.ToolResult, error) {
	cur := m.activeCount.Add(1)
	defer m.activeCount.Add(-1)

	now := time.Now()
	rec := execRecord{
		name:          name,
		args:          string(args),
		start:         now,
		activeAtStart: cur,
	}

	if cur > 1 {
		m.overlapCheck.Store(true)
	}

	if m.delay > 0 {
		time.Sleep(m.delay)
	}

	rec.end = time.Now()

	m.mu.Lock()
	m.history = append(m.history, rec)
	m.mu.Unlock()

	return &types.ToolResult{
		Success: true,
		Output:  []byte(fmt.Sprintf(`{"result":"ok from %s"}`, name)),
	}, nil
}

func (m *concurrencyMockToolExecutor) getRecord(name string) (execRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.history {
		if r.name == name {
			return r, true
		}
	}
	return execRecord{}, false
}

// TestR2_06_IsConcurrencySafe 验证 isConcurrencySafe 判定的完整性与 fail-closed 契约。
func TestR2_06_IsConcurrencySafe(t *testing.T) {
	tests := []struct {
		name string
		tool types.Tool
		want bool
	}{
		{
			name: "trusted builtin readonly with SideNone",
			tool: types.Tool{
				Name:        "read_file",
				Source:      types.ToolBuiltin,
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			want: true,
		},
		{
			name: "trusted builtin readonly with empty SideEffects",
			tool: types.Tool{
				Name:        "grep",
				Source:      types.ToolBuiltin,
				Capability:  types.CapReadOnly,
				SideEffects: nil,
			},
			want: true,
		},
		{
			name: "capability higher than CapReadOnly",
			tool: types.Tool{
				Name:        "write_file",
				Source:      types.ToolBuiltin,
				Capability:  types.CapWriteLocal,
				SideEffects: []types.SideEffect{types.SideFileWrite},
			},
			want: false,
		},
		{
			name: "readonly capability but has file_write side effect",
			tool: types.Tool{
				Name:        "sneaky_tool",
				Source:      types.ToolBuiltin,
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideFileWrite},
			},
			want: false,
		},
		{
			name: "readonly capability but has process_spawn side effect",
			tool: types.Tool{
				Name:        "spawn_cmd",
				Source:      types.ToolBuiltin,
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideProcessSpawn},
			},
			want: false,
		},
		{
			name: "mcp source must fail-closed",
			tool: types.Tool{
				Name:        "mcp_read",
				Source:      types.ToolMCP,
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			want: false,
		},
		{
			name: "mcp prefix name must fail-closed",
			tool: types.Tool{
				Name:        "mcp__server__read",
				Source:      types.ToolBuiltin,
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			want: false,
		},
		{
			name: "skill source must fail-closed",
			tool: types.Tool{
				Name:        "skill__search",
				Source:      types.ToolSkill,
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			want: false,
		},
		{
			name: "code_act name must fail-closed",
			tool: types.Tool{
				Name:        "code_act:python",
				Source:      types.ToolBuiltin,
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			want: false,
		},
		{
			name: "delegation transfer_to_agent must fail-closed",
			tool: types.Tool{
				Name:        "transfer_to_agent",
				Source:      types.ToolBuiltin,
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			want: false,
		},
		{
			name: "delegation spawn_agents must fail-closed",
			tool: types.Tool{
				Name:        "spawn_agents",
				Source:      types.ToolBuiltin,
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isConcurrencySafe(tc.tool)
			if got != tc.want {
				t.Errorf("isConcurrencySafe() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestR2_06_ExecuteToolCallsConcurrently_MixedBatch 验证混合批次调用：
// 连续并发安全调用成组并行执行，不安全调用严格串行执行，结果按原顺序返回。
func TestR2_06_ExecuteToolCallsConcurrently_MixedBatch(t *testing.T) {
	agent := NewAgentWithDefaults("test-r2-06-mixed")
	mock := newConcurrencyMockExecutor(30 * time.Millisecond)

	// 注册 5 个工具：2 个安全，1 个不安全，2 个安全
	mock.registerTool(types.Tool{Name: "r1", Source: types.ToolBuiltin, Capability: types.CapReadOnly})
	mock.registerTool(types.Tool{Name: "r2", Source: types.ToolBuiltin, Capability: types.CapReadOnly})
	mock.registerTool(types.Tool{Name: "w3", Source: types.ToolBuiltin, Capability: types.CapWriteLocal, SideEffects: []types.SideEffect{types.SideFileWrite}})
	mock.registerTool(types.Tool{Name: "r4", Source: types.ToolBuiltin, Capability: types.CapReadOnly})
	mock.registerTool(types.Tool{Name: "r5", Source: types.ToolBuiltin, Capability: types.CapReadOnly})
	agent.InjectToolExecutor(mock)

	calls := []types.InferToolCall{
		{ID: "c1", Name: "r1", Input: []byte(`{}`)},
		{ID: "c2", Name: "r2", Input: []byte(`{}`)},
		{ID: "c3", Name: "w3", Input: []byte(`{}`)},
		{ID: "c4", Name: "r4", Input: []byte(`{}`)},
		{ID: "c5", Name: "r5", Input: []byte(`{}`)},
	}

	agent.recordValidatedPlan(toolCallsToDAGPlan(calls, types.TaintNone))

	results := agent.executeToolCallsConcurrently(context.Background(), calls)
	if len(results) != len(calls) {
		t.Fatalf("expected %d results, got %d", len(calls), len(results))
	}

	// 1. 结果严格按原顺序返回
	for i, id := range []string{"c1", "c2", "c3", "c4", "c5"} {
		toolUseID := ""
		if len(results[i].Parts) > 0 {
			if partMap, ok := results[i].Parts[0].(map[string]any); ok {
				if v, ok := partMap["tool_use_id"].(string); ok {
					toolUseID = v
				}
			}
		}
		if toolUseID != id {
			t.Errorf("result[%d] tool_use_id mismatch: got %q, want %q", i, toolUseID, id)
		}
	}

	// 2. 顺序断言：
	r1Rec, ok1 := mock.getRecord("r1")
	r2Rec, ok2 := mock.getRecord("r2")
	w3Rec, ok3 := mock.getRecord("w3")
	r4Rec, ok4 := mock.getRecord("r4")
	r5Rec, ok5 := mock.getRecord("r5")
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		t.Fatalf("missing execution record: ok1=%v ok2=%v ok3=%v ok4=%v ok5=%v", ok1, ok2, ok3, ok4, ok5)
	}

	// r1 & r2 并行执行（在 w3 开始前均已结束）
	if !r1Rec.end.Before(w3Rec.start) && !r1Rec.end.Equal(w3Rec.start) {
		t.Errorf("r1 should finish before or at w3 starts: r1End=%v, w3Start=%v", r1Rec.end, w3Rec.start)
	}
	if !r2Rec.end.Before(w3Rec.start) && !r2Rec.end.Equal(w3Rec.start) {
		t.Errorf("r2 should finish before or at w3 starts: r2End=%v, w3Start=%v", r2Rec.end, w3Rec.start)
	}

	// w3 串行执行，并发数必须为 1
	if w3Rec.activeAtStart > 1 {
		t.Errorf("w3 is unsafe and must execute serially alone, but activeAtStart was %d", w3Rec.activeAtStart)
	}

	// w3 在 r4, r5 开始前必须已结束
	if !w3Rec.end.Before(r4Rec.start) && !w3Rec.end.Equal(r4Rec.start) {
		t.Errorf("w3 should finish before r4 starts: w3End=%v, r4Start=%v", w3Rec.end, r4Rec.start)
	}
	if !w3Rec.end.Before(r5Rec.start) && !w3Rec.end.Equal(r5Rec.start) {
		t.Errorf("w3 should finish before r5 starts: w3End=%v, r5Start=%v", w3Rec.end, r5Rec.start)
	}

	// r1, r2 或 r4, r5 应当发生过并发
	if !mock.overlapCheck.Load() {
		t.Errorf("concurrency-safe tools expected to execute concurrently, but no overlap detected")
	}
}

// TestR2_06_ExecuteToolCallsConcurrently_TwoWritesSameFile_Serial 验证写同一文件的多个调用必须严格串行。
func TestR2_06_ExecuteToolCallsConcurrently_TwoWritesSameFile_Serial(t *testing.T) {
	agent := NewAgentWithDefaults("test-r2-06-serial-writes")
	mock := newConcurrencyMockExecutor(25 * time.Millisecond)

	mock.registerTool(types.Tool{
		Name:        "write_file",
		Source:      types.ToolBuiltin,
		Capability:  types.CapWriteLocal,
		SideEffects: []types.SideEffect{types.SideFileWrite},
	})
	agent.InjectToolExecutor(mock)

	calls := []types.InferToolCall{
		{ID: "w1", Name: "write_file", Input: []byte(`{"path":"test.txt","content":"hello"}`)},
		{ID: "w2", Name: "write_file", Input: []byte(`{"path":"test.txt","content":"world"}`)},
	}

	agent.recordValidatedPlan(toolCallsToDAGPlan(calls, types.TaintNone))

	results := agent.executeToolCallsConcurrently(context.Background(), calls)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	mock.mu.Lock()
	records := append([]execRecord(nil), mock.history...)
	mock.mu.Unlock()

	if len(records) != 2 {
		t.Fatalf("expected 2 execution records, got %d", len(records))
	}

	// 如果两个写调用同时运行，并发数必定达到 2
	if mock.overlapCheck.Load() {
		t.Fatalf("two write_file calls executed concurrently! overlap detected")
	}

	for i, r := range records {
		if r.activeAtStart > 1 {
			t.Fatalf("record[%d] activeAtStart=%d, want 1", i, r.activeAtStart)
		}
	}

	// 记录 0 必须在 记录 1 开始前或同时结束
	if !records[0].end.Before(records[1].start) && !records[0].end.Equal(records[1].start) {
		t.Fatalf("first write did not finish before second write: rec0.end=%v, rec1.start=%v", records[0].end, records[1].start)
	}
}
