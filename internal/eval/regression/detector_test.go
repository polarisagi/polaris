package regression

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store"
)

// openTestDB 创建临时 SQLite 库并加载 schema，返回可直接用于 SQLQuerier 的 *sql.DB。
func openTestDB(t *testing.T) *store.SQLiteStore {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "polaris.db")
	s, err := store.OpenSQLite(dbPath, schema.FS)
	if err != nil {
		t.Fatalf("store.OpenSQLite failed: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedEvents 直接向 events 表写入 n 条同类型事件（绕过 MutationBus，仅用于测试查询层）。
func seedEvents(t *testing.T, s *store.SQLiteStore, n int, evType, topic string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, err := s.DB().ExecContext(ctx,
			`INSERT INTO events (id, topic, actor, type, payload, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			randID(t), topic, "system:test", evType, []byte("{}"), 1720000000000+int64(i),
		)
		if err != nil {
			t.Fatalf("seed event failed: %v", err)
		}
	}
}

var idCounter int

func randID(t *testing.T) string {
	t.Helper()
	idCounter++
	return fmt.Sprintf("01TEST-%d", idCounter)
}

func TestDetectRegression_NilDB(t *testing.T) {
	d := NewLightweightRegressionDetector(nil)
	if _, err := d.DetectRegression(context.Background(), "test-module"); err == nil {
		t.Fatal("expected error for nil db, got nil")
	}
}

func TestDetectRegression_InsufficientData(t *testing.T) {
	s := openTestDB(t)
	seedEvents(t, s, 10, "tool_call", "tool.result")

	d := NewLightweightRegressionDetector(s.DB())
	report, err := d.DetectRegression(context.Background(), "test-module")
	if err != nil {
		t.Fatalf("DetectRegression failed: %v", err)
	}
	if report.Severity != "Warning" {
		t.Errorf("expected Warning severity for insufficient data, got %s", report.Severity)
	}
	if !strings.Contains(report.Markdown, "数据不足") {
		t.Errorf("expected report to mention insufficient data, got: %s", report.Markdown)
	}
}

func TestDetectRegression_StableBaseline(t *testing.T) {
	s := openTestDB(t)
	// 基线窗口 + 近期窗口：均为 200 条 tool_call，无错误事件、无新类型 → Pass
	seedEvents(t, s, regressionWindowSize, "tool_call", "tool.result")
	seedEvents(t, s, regressionWindowSize, "tool_call", "tool.result")

	d := NewLightweightRegressionDetector(s.DB())
	report, err := d.DetectRegression(context.Background(), "test-module")
	if err != nil {
		t.Fatalf("DetectRegression failed: %v", err)
	}
	if report.Severity != "Pass" {
		t.Errorf("expected Pass severity for stable distribution, got %s: %s", report.Severity, report.Markdown)
	}
}

func TestDetectRegression_NewErrorSpike(t *testing.T) {
	s := openTestDB(t)
	// 基线窗口：干净
	seedEvents(t, s, regressionWindowSize, "tool_call", "tool.result")
	// 近期窗口：混入大量错误类事件
	seedEvents(t, s, regressionWindowSize-10, "tool_call", "tool.result")
	seedEvents(t, s, 10, "tool_call", "tool.result.error")

	d := NewLightweightRegressionDetector(s.DB())
	report, err := d.DetectRegression(context.Background(), "test-module")
	if err != nil {
		t.Fatalf("DetectRegression failed: %v", err)
	}
	if report.Severity == "Pass" {
		t.Errorf("expected non-Pass severity when error-topic events spike, got Pass: %s", report.Markdown)
	}
}
