package cronadmin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	repoimpl "github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func newTestCronAdmin(t *testing.T) (*CronAdmin, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS automations (
			id TEXT PRIMARY KEY,
			name TEXT,
			prompt TEXT,
			trigger_type TEXT,
			cron_schedule TEXT,
			channel_id TEXT,
			working_dir TEXT,
			env_type TEXT,
			reasoning_effort TEXT,
			result_action TEXT,
			sandbox_level INTEGER,
			cedar_rules_json TEXT,
			enabled INTEGER,
			requires_hitl INTEGER,
			risk_level INTEGER,
			source TEXT DEFAULT 'user',
			last_run_at TEXT,
			next_run_at TEXT,
			run_count INTEGER,
			last_run_status TEXT,
			last_run_error TEXT,
			failure_count INTEGER,
			circuit_open INTEGER NOT NULL DEFAULT 0,
			circuit_opened_at TEXT,
			created_at TEXT,
			updated_at TEXT,
			event_filter TEXT
		);
		CREATE TABLE IF NOT EXISTS automation_runs (
			id TEXT PRIMARY KEY,
			automation_id TEXT,
			trigger TEXT,
			status TEXT,
			session_id TEXT,
			started_at TEXT,
			finished_at TEXT,
			error_msg TEXT,
			prompt_snapshot TEXT
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	ca := &CronAdmin{
		DB:             db,
		AutomationRepo: repoimpl.NewSQLiteAutomationRepository(db),
	}
	return ca, db
}

func TestAutomationService_Validation(t *testing.T) {
	ca, _ := newTestCronAdmin(t)
	ctx := context.Background()

	// 1. 缺少 prompt
	_, _, err := ca.SaveAutomation(ctx, AutomationInput{
		CronSchedule: "0 * * * *",
	})
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for missing prompt, got %v", err)
	}

	// 2. 非法 cron
	_, _, err = ca.SaveAutomation(ctx, AutomationInput{
		Prompt:       "test prompt",
		TriggerType:  "cron",
		CronSchedule: "invalid-cron-expr",
	})
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for invalid cron, got %v", err)
	}

	// 3. 非法 reasoning_effort
	_, _, err = ca.SaveAutomation(ctx, AutomationInput{
		Prompt:          "test prompt",
		TriggerType:     "manual",
		ReasoningEffort: "extreme_high",
	})
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for invalid reasoning_effort, got %v", err)
	}

	// 4. 非法 result_action
	_, _, err = ca.SaveAutomation(ctx, AutomationInput{
		Prompt:       "test prompt",
		TriggerType:  "manual",
		ResultAction: "invalid_action",
	})
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for invalid result_action, got %v", err)
	}

	// 5. 空 channel: 前缀
	_, _, err = ca.SaveAutomation(ctx, AutomationInput{
		Prompt:       "test prompt",
		TriggerType:  "manual",
		ResultAction: "channel:",
	})
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for empty channel: prefix, got %v", err)
	}

	// 6. 有效 channel:<id>
	id, _, err := ca.SaveAutomation(ctx, AutomationInput{
		Prompt:       "test prompt",
		TriggerType:  "manual",
		ResultAction: "channel:ch_slack_1",
	})
	if err != nil {
		t.Errorf("expected success for channel:ch_slack_1, got %v", err)
	}
	if id == "" {
		t.Errorf("expected non-empty id")
	}
}

func TestAutomationService_PreserveSourceOnUpdate(t *testing.T) {
	ca, _ := newTestCronAdmin(t)
	ctx := context.Background()

	// 创建来源为 agent 的自动化
	id, _, err := ca.SaveAutomation(ctx, AutomationInput{
		Name:            "agent task",
		Prompt:          "hello agent",
		TriggerType:     "cron",
		CronSchedule:    "0 * * * *",
		Source:          "agent",
		ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 验证初始 source 为 agent
	row, err := ca.AutomationRepo.GetAutomation(ctx, id)
	if err != nil || row == nil {
		t.Fatalf("failed to get automation: %v", err)
	}
	if row.Source != "agent" {
		t.Errorf("expected source agent, got %q", row.Source)
	}

	// 更新自动化，传入不同的 source 试图篡改
	_, _, err = ca.SaveAutomation(ctx, AutomationInput{
		ID:           id,
		Prompt:       "updated prompt",
		TriggerType:  "cron",
		CronSchedule: "0 * * * *",
		Source:       "user", // 试图修改来源
	})
	if err != nil {
		t.Fatal(err)
	}

	// 验证更新后 source 仍保持创建时的 agent
	rowUpdated, err := ca.AutomationRepo.GetAutomation(ctx, id)
	if err != nil || rowUpdated == nil {
		t.Fatal(err)
	}
	if rowUpdated.Source != "agent" {
		t.Errorf("expected source to remain agent after update, got %q", rowUpdated.Source)
	}
}

func TestAutomationService_DeleteNotFound(t *testing.T) {
	ca, _ := newTestCronAdmin(t)
	ctx := context.Background()

	err := ca.DeleteAutomation(ctx, "non_existent_id")
	if err == nil || !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Errorf("expected CodeNotFound when deleting non-existent id, got %v", err)
	}
}

func TestAutomationService_TriggerDisabled(t *testing.T) {
	ca, _ := newTestCronAdmin(t)
	ctx := context.Background()

	disabled := false
	id, _, err := ca.SaveAutomation(ctx, AutomationInput{
		Prompt:      "prompt",
		TriggerType: "manual",
		Enabled:     &disabled,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = ca.TriggerAutomation(ctx, id)
	if err == nil || !apperr.IsCode(err, apperr.CodeConflict) {
		t.Errorf("expected CodeConflict when triggering disabled automation, got %v", err)
	}
}

func TestAutomationService_HTTPConsistency(t *testing.T) {
	ca, _ := newTestCronAdmin(t)

	// 非法 cron 请求体
	body := `{"prompt": "hello", "trigger_type": "cron", "cron_schedule": "not-valid"}`
	req := httptest.NewRequest("POST", "/api/v1/automations", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	ca.HandleCreateAutomation(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("expected HTTP 400 for invalid cron, got %d", w.Result().StatusCode)
	}

	// SaveAutomation 调用同一非法输入同样应返回 CodeInvalidInput
	var in AutomationInput
	json.Unmarshal([]byte(body), &in)
	_, _, err := ca.SaveAutomation(context.Background(), in)
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected SaveAutomation to return CodeInvalidInput, got %v", err)
	}
}

func TestMinIntervalMinutes(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 1. 每 15 分钟
	m15, err := MinIntervalMinutes("*/15 * * * *", from)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m15 != 15 {
		t.Errorf("expected 15 min, got %d", m15)
	}

	// 2. 每小时
	m60, err := MinIntervalMinutes("0 * * * *", from)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m60 != 60 {
		t.Errorf("expected 60 min, got %d", m60)
	}

	// 3. 语义别名 @hourly
	mHourly, err := MinIntervalMinutes("@hourly", from)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mHourly != 60 {
		t.Errorf("expected 60 min, got %d", mHourly)
	}

	// 4. 非法表达式
	_, err = MinIntervalMinutes("invalid cron", from)
	if err == nil {
		t.Errorf("expected error for invalid cron")
	}
}

func TestAutomationService_ListSummaries(t *testing.T) {
	ca, _ := newTestCronAdmin(t)
	ctx := context.Background()

	ca.SaveAutomation(ctx, AutomationInput{
		Name:         "Task 1",
		Prompt:       "Do task 1",
		TriggerType:  "cron",
		CronSchedule: "0 * * * *",
		Source:       "agent",
	})

	summaries, err := ca.ListAutomationSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	if summaries[0].Name != "Task 1" || summaries[0].Kind != "prompt" || summaries[0].Source != "agent" {
		t.Errorf("unexpected summary: %+v", summaries[0])
	}
}
