package sysadmin

import (
	"context"
	"database/sql"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"

	_ "github.com/mattn/go-sqlite3"
)

// TestNewSysAdminHandler_CronCallbacksWired 回归测试（2026-07-07）：
//
// 此前 NewSysAdminHandler 把 cronadmin.NewCronAdmin 的 buildToolSchemas/
// cronTickWorkflows 两个回调硬编码为 nil。cron_scheduler.go 的 cronTick()
// 无条件调用 ca.CronTickWorkflows(ctx)，cron_runner.go 的 executeAutomation()
// 无条件调用 ca.BuildToolSchemas()——一旦调度器被启动（另一半修复见
// server_core.go Start()），首次 tick / 首次执行 automation 就会因调用
// nil func 触发 panic，导致整个进程崩溃。
//
// 本测试验证：1) 两个回调经由 NewSysAdminHandler 正确接到 h 自身方法，均非
// nil；2) CronTickWorkflows 对着一张空 workflows 表实际执行一次不 panic。
func TestNewSysAdminHandler_CronCallbacksWired(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE workflows (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			trigger_type TEXT NOT NULL DEFAULT 'manual',
			cron_schedule TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			next_run_at TEXT NOT NULL DEFAULT '',
			last_run_status TEXT NOT NULL DEFAULT '',
			circuit_open INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		t.Fatal(err)
	}

	h := NewSysAdminHandler(Dependencies{DB: db})

	if h.Cron == nil {
		t.Fatal("h.Cron 未初始化")
	}
	if h.Cron.BuildToolSchemas == nil {
		t.Fatal("Cron.BuildToolSchemas 仍是 nil——调度器一启动就会在 executeAutomation 里 panic")
	}
	if h.Cron.CronTickWorkflows == nil {
		t.Fatal("Cron.CronTickWorkflows 仍是 nil——调度器一启动就会在 cronTick 里 panic")
	}

	// 直接调用回调本身（而非跑完整 cronTick），验证不 panic 且能正常查询空表。
	h.Cron.CronTickWorkflows(context.Background())
	if schemas := h.Cron.BuildToolSchemas(); schemas != nil {
		t.Fatalf("h.Catalog 未设置时 BuildToolSchemas 应返回 nil，实际: %v", schemas)
	}
}

// TestNewSysAdminHandler_HITLGatewayWired 回归测试（2026-07-07）：
// 此前 sysadmin.Dependencies 没有 HITLGateway 字段，server_core.go 里已经
// 拿到的真实 hitlGateway 实例从未传给 NewSysAdminHandler，导致
// RequiresHITL=true 的 automation 永远走 cron_runner.go 里的 nil 分支
// 静默跳过审批（有判空不 panic，但审批形同虚设）。
func TestNewSysAdminHandler_HITLGatewayWired(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	fake := fakeHITL{}
	h := NewSysAdminHandler(Dependencies{DB: db, HITLGateway: fake})

	if h.HITLGateway == nil {
		t.Fatal("h.HITLGateway 未从 Dependencies 接入")
	}
	if h.Cron.HITLGateway == nil {
		t.Fatal("h.Cron.HITLGateway 未从 Dependencies 接入")
	}
}

type fakeHITL struct{}

func (fakeHITL) Prompt(ctx context.Context, p types.HITLPrompt) (*types.HITLResponse, error) {
	return nil, nil
}
func (fakeHITL) Respond(ctx context.Context, checkpointID string, response types.HITLResponse) error {
	return nil
}
func (fakeHITL) Pending(ctx context.Context) ([]types.HITLPrompt, error) { return nil, nil }
