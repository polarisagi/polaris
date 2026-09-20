package automation

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/polarisagi/polaris/internal/protocol/pb"

	_ "modernc.org/sqlite"
)

func TestParseInferencePayload(t *testing.T) {
	pbPayload := &pb.LLMCallPayload{
		Provider:     "deepseek",
		InputTokens:  1000,
		OutputTokens: 200,
		CostUsd:      0.324, // custom cost
	}
	payload, _ := proto.Marshal(pbPayload)
	tokens, provider, _, _, callType, costUSD := parseInferencePayload(payload, "topic", "actor", "evType")
	if tokens != 1200 {
		t.Errorf("Expected 1200, got %d", tokens)
	}
	if provider != "deepseek" || callType != "evType" {
		t.Errorf("Mismatch in parsed fields")
	}
	if costUSD != 0.324 {
		t.Errorf("Expected cost 0.324, got %v", costUSD)
	}

	// Missing provider -> falls back to actor
	pbPayload2 := &pb.LLMCallPayload{
		InputTokens: 100,
	}
	payload2, _ := proto.Marshal(pbPayload2)
	_, p2, _, _, _, _ := parseInferencePayload(payload2, "topic", "default_actor", "evType")
	if p2 != "default_actor" {
		t.Errorf("Expected default_actor, got %s", p2)
	}
}

func TestGenerateCostReport(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open db: %v", err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
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
	pbPayload := &pb.LLMCallPayload{
		Provider:    "deepseek",
		InputTokens: 1000000,
	}
	payload, _ := proto.Marshal(pbPayload)
	db.Exec(`INSERT INTO events (topic, actor, type, payload, created_at) VALUES (?, ?, ?, ?, ?)`,
		"llm.call.recorded", "actor", "type", payload, monthStart.UnixMilli()+1000)

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
	if !strings.Contains(string(content), "- type: $0.27") { // evType was passed as "type", so call_type="type"
		t.Errorf("Expected cost for type to be $0.27")
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
