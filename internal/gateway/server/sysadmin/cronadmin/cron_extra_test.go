package cronadmin

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func TestCronNewRunID(t *testing.T) {
	id := newRunID()
	if id == "" {
		t.Errorf("expected non-empty run ID")
	}
}

func TestUpdateAutomationStats(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE automations (id TEXT PRIMARY KEY, run_count INTEGER, success_count INTEGER, failure_count INTEGER, last_run_status TEXT, last_run_error TEXT, circuit_open INTEGER, circuit_opened_at DATETIME, next_run_at DATETIME, updated_at DATETIME)")
	if err != nil {
		t.Fatal(err)
	}

	ca := &CronAdmin{
		DB:             db,
		AutomationRepo: repo.NewSQLiteAutomationRepository(db),
	}
	ca.updateAutomationStats("test", "success", "error", "100")
}

func TestExecuteAutomation(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()
	// 按真实 DDL 建表：此前手写的 automation_runs 用了 error/completed_at 等
	// 不存在于 017 的列，恰好掩盖了 UpdateRunStatus 列名与 DDL 不符的缺陷。
	ddl, err := schema.FS.ReadFile("017_automations.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}

	ca := &CronAdmin{
		DB:             db,
		AutomationRepo: repo.NewSQLiteAutomationRepository(db),
		Registry:       llm.NewProviderRegistry(config.M1RouterThresholds{}),
	}
	ca.executeAutomation(context.Background(), &automation{ID: "test"}, "manual")

	// Wait a bit for the async goroutine to fail/finish gracefully
	time.Sleep(50 * time.Millisecond)
}

// SessionOrch 未注入时后台 worker 不得在 RunTurn 处 nil panic（CI 日志实测被
// SafeGo 吞掉后 automation 永远停在 running），须按 error 收尾写回统计。
func TestExecuteAutomation_NilSessionOrchFinishesAsError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	ddl, err := schema.FS.ReadFile("017_automations.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO automations (id, name, prompt) VALUES ('a1', 'n', 'p')`); err != nil {
		t.Fatal(err)
	}

	ca := &CronAdmin{DB: db, AutomationRepo: repo.NewSQLiteAutomationRepository(db)}
	ca.executeAutomation(context.Background(), &automation{ID: "a1", Name: "n", Prompt: "p"}, "manual")

	var status, errMsg string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.QueryRow(`SELECT last_run_status, last_run_error FROM automations WHERE id='a1'`).Scan(&status, &errMsg); err != nil {
			t.Fatal(err)
		}
		if status == "error" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != "error" || errMsg != "session orchestrator not configured" {
		t.Fatalf("want error/session orchestrator not configured, got %q/%q", status, errMsg)
	}

	// run 记录须按真实 DDL 插入并收尾：此前 CreateRun 漏传 trigger 违反 CHECK、
	// UpdateRunStatus 写不存在的列，两步都静默失败，automation_runs 始终为空。
	var trigger, runStatus, runErr, finishedAt string
	if err := db.QueryRow(`SELECT trigger, status, error_msg, finished_at FROM automation_runs WHERE automation_id='a1'`).
		Scan(&trigger, &runStatus, &runErr, &finishedAt); err != nil {
		t.Fatalf("run record missing: %v", err)
	}
	if trigger != "manual" || runStatus != "error" || runErr != "session orchestrator not configured" || finishedAt == "" {
		t.Fatalf("bad run record: trigger=%q status=%q error_msg=%q finished_at=%q", trigger, runStatus, runErr, finishedAt)
	}
	var sessionID, promptSnapshot string
	if err := db.QueryRow(`SELECT session_id, prompt_snapshot FROM automation_runs WHERE automation_id='a1'`).
		Scan(&sessionID, &promptSnapshot); err != nil {
		t.Fatal(err)
	}
	if sessionID == "" || promptSnapshot != "p" {
		t.Fatalf("run record must carry session_id and prompt_snapshot, got %q/%q", sessionID, promptSnapshot)
	}
}
