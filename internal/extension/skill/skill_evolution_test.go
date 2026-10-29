package skill

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store"
)

func newTestMonitor(t *testing.T) (*SkillEvolutionMonitor, *store.SQLiteStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "polaris.db")
	s, err := store.OpenSQLite(dbPath, schema.FS)
	if err != nil {
		t.Fatalf("store.OpenSQLite failed: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cfg := config.DefaultThresholds()
	m := NewSkillEvolutionMonitor(s.DB(), nil, nil, &cfg)
	return m, s
}

func seedToolCallEvent(t *testing.T, s *store.SQLiteStore, seq int, skillName string, success bool) {
	t.Helper()
	content := `{"tool_name":"` + skillName + `","success":` + boolStr(success) + `}`
	_, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO episodic_events (session_id, seq, timestamp, event_type, source, content, archived) VALUES (?, ?, ?, 'tool_call', 'agent', ?, 0)`,
		"test-session", seq, time.Now().UnixMilli(), content,
	)
	if err != nil {
		t.Fatalf("seed tool_call event failed: %v", err)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestGatherSkillStats_ReadsToolCallEvents 验证修复后的查询（event_type='tool_call'）
// 能正确读到真实写入的事件，而不是此前恒为空结果集的 'tool_result'。
func TestGatherSkillStats_ReadsToolCallEvents(t *testing.T) {
	m, s := newTestMonitor(t)

	for i := 0; i < 8; i++ {
		seedToolCallEvent(t, s, i, "skill:flaky", true)
	}
	for i := 8; i < 12; i++ {
		seedToolCallEvent(t, s, i, "skill:flaky", false)
	}

	stats, err := m.gatherSkillStats(context.Background())
	if err != nil {
		t.Fatalf("gatherSkillStats failed: %v", err)
	}
	entry, ok := stats["skill:flaky"]
	if !ok {
		t.Fatal("expected stats for skill:flaky, got none (query still not matching real event_type)")
	}
	if entry.Total != 12 || entry.Success != 8 {
		t.Errorf("expected Total=12 Success=8, got Total=%d Success=%d", entry.Total, entry.Success)
	}
}

// TestTriggerEvolutions_CooldownPreventsReTrigger 验证同一技能在冷却期内不会被重复标记触发。
func TestTriggerEvolutions_CooldownPreventsReTrigger(t *testing.T) {
	m, _ := newTestMonitor(t)

	stats := map[string]*skillStatEntry{
		"skill:bad": {Total: 20, Success: 2}, // rate=0.1，低于默认阈值 0.6
	}

	m.triggerEvolutions(context.Background(), stats, 0.6, 10)
	firstTs, ok := m.lastTriggeredAt["skill:bad"]
	if !ok {
		t.Fatal("expected skill:bad to be recorded in lastTriggeredAt after first trigger")
	}

	// 立即再次调用：应命中冷却期，不更新时间戳
	m.triggerEvolutions(context.Background(), stats, 0.6, 10)
	secondTs := m.lastTriggeredAt["skill:bad"]
	if !secondTs.Equal(firstTs) {
		t.Errorf("expected lastTriggeredAt unchanged within cooldown window, first=%v second=%v", firstTs, secondTs)
	}
}

// TestCheckAndEvolve_RunningGuardResetsAfterCompletion 验证 running 标志在调用完成后被正确复位，
// 不会永久阻塞后续扫描。
func TestCheckAndEvolve_RunningGuardResetsAfterCompletion(t *testing.T) {
	m, _ := newTestMonitor(t)

	if err := m.CheckAndEvolve(context.Background()); err != nil {
		t.Fatalf("CheckAndEvolve failed: %v", err)
	}
	m.mu.Lock()
	running := m.running
	m.mu.Unlock()
	if running {
		t.Error("expected running=false after CheckAndEvolve completes")
	}
}
