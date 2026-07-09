package automation

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestExtractJSONString(t *testing.T) {
	data := []byte(`{"provider":"deepseek", "other": "value"}`)
	if extractJSONString(data, "provider") != "deepseek" {
		t.Errorf("Expected deepseek")
	}
	if extractJSONString(data, "missing") != "" {
		t.Errorf("Expected empty")
	}
}

func TestExtractJSONInt(t *testing.T) {
	data := []byte(`{"input_tokens":1000, "other": "value"}`)
	if extractJSONInt(data, "input_tokens") != 1000 {
		t.Errorf("Expected 1000")
	}
	if extractJSONInt(data, "missing") != 0 {
		t.Errorf("Expected 0")
	}
}

func TestParseInferencePayload(t *testing.T) {
	payload := []byte(`{"provider":"deepseek","task_type":"agent.task","session_id":"s1","call_type":"llm","input_tokens":1000,"output_tokens":200}`)
	tokens, provider, taskType, sessionID, callType := parseInferencePayload(payload, "topic", "actor", "evType")
	if tokens != 1200 {
		t.Errorf("Expected 1200, got %d", tokens)
	}
	if provider != "deepseek" || taskType != "agent.task" || sessionID != "s1" || callType != "llm" {
		t.Errorf("Mismatch in parsed fields")
	}

	// Missing provider -> falls back to actor
	payload2 := []byte(`{"input_tokens":100}`)
	_, p2, _, _, _ := parseInferencePayload(payload2, "topic", "default_actor", "evType")
	if p2 != "default_actor" {
		t.Errorf("Expected default_actor, got %s", p2)
	}
}

func TestGenerateCostReport(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE events (
		topic TEXT,
		actor TEXT,
		type TEXT,
		payload BLOB,
		created_at INTEGER
	)`)
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}

	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	// Insert dummy event inside the range
	payload := `{"provider":"deepseek","task_type":"t1","session_id":"s1","call_type":"c1","input_tokens":1000000,"output_tokens":0}`
	db.Exec(`INSERT INTO events (topic, actor, type, payload, created_at) VALUES (?, ?, ?, ?, ?)`,
		"llm.test", "actor", "type", payload, monthStart.UnixMicro()+1000)

	reporter := NewCostReporter()
	tmpDir := t.TempDir()

	err = reporter.generateCostReport(context.Background(), tmpDir, db)
	if err != nil {
		t.Fatalf("generateCostReport failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, "monthly_cost_report.md"))
	if err != nil {
		t.Fatalf("Failed to read report: %v", err)
	}

	// 1000000 tokens for deepseek = $0.27
	if !strings.Contains(string(content), "- deepseek: $0.27") {
		t.Errorf("Expected cost for deepseek to be $0.27, got:\n%s", string(content))
	}
	if !strings.Contains(string(content), "- t1: $0.27") {
		t.Errorf("Expected cost for t1 to be $0.27")
	}
	if !strings.Contains(string(content), "- s1: $0.27") {
		t.Errorf("Expected cost for s1 to be $0.27")
	}
	if !strings.Contains(string(content), "- c1: $0.27") {
		t.Errorf("Expected cost for c1 to be $0.27")
	}
}

func TestStartMonthlyCostReport_Cancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tmpDir := t.TempDir()

	StartMonthlyCostReport(ctx, tmpDir, nil)

	// Cancel immediately to check if it exits without panic
	cancel()
	time.Sleep(100 * time.Millisecond) // Give goroutine time to exit
}
