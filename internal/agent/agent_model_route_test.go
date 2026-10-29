package agent

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestAgent_ResolveModelRoute_PriorityMatrix(t *testing.T) {
	ctx := context.Background()

	// 1. 默认兜底：无任何指定时返回默认池
	a := NewAgentWithDefaults("sess-route-1")
	ref, pool, thinking, err := a.resolveModelRoute(ctx, "default", "none")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref != "" || pool != "default" || thinking != "none" {
		t.Fatalf("expected ref='', pool='default', thinking='none', got ref=%q, pool=%q, thinking=%q", ref, pool, thinking)
	}

	// 2. 循环自选 (NextRound) 覆盖默认池
	a.sCtx.NextRoundModelRef = "openai/gpt-4o"
	a.sCtx.NextRoundThinking = "high"
	ref, pool, thinking, err = a.resolveModelRoute(ctx, "default", "none")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref != "openai/gpt-4o" || pool != "default" || thinking != "high" {
		t.Fatalf("expected ref='openai/gpt-4o', pool='default', thinking='high', got ref=%q, pool=%q, thinking=%q", ref, pool, thinking)
	}
	// 消费后 NextRound 字段自动清空
	if a.sCtx.NextRoundModelRef != "" {
		t.Fatalf("expected NextRoundModelRef to be consumed and cleared")
	}

	// 3. 角色指定 (AgentProfile) 优先于循环自选
	a.sCtx.NextRoundModelRef = "openai/gpt-4o-mini"
	a.sCtx.AgentProfile = &types.AgentProfileSpec{
		Model:    "anthropic/claude-3-7-sonnet",
		Thinking: "medium",
	}
	ref, pool, thinking, err = a.resolveModelRoute(ctx, "default", "none")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref != "anthropic/claude-3-7-sonnet" || pool != "default" || thinking != "medium" {
		t.Fatalf("expected agent_profile model to win, got ref=%q, pool=%q, thinking=%q", ref, pool, thinking)
	}

	// 4. 用户手选 (UserLockedModel) 拥有最高优先级，覆盖角色与自选
	a.SetModelLock("google/gemini-2.5-pro")
	ref, pool, thinking, err = a.resolveModelRoute(ctx, "default", "none")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref != "google/gemini-2.5-pro" || pool != "default" || thinking != "none" {
		t.Fatalf("expected user lock to win over all, got ref=%q, pool=%q, thinking=%q", ref, pool, thinking)
	}

	// 5. 引用指定为已知 ModelPool 时直接转化为 pool
	a.SetModelLock("reasoning")
	ref, pool, thinking, err = a.resolveModelRoute(ctx, "default", "none")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref != "" || pool != "reasoning" || thinking != "none" {
		t.Fatalf("expected ref='', pool='reasoning', thinking='none', got ref=%q, pool=%q, thinking=%q", ref, pool, thinking)
	}
}

func setupTestRouteDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
		CREATE TABLE providers (id TEXT PRIMARY KEY, name TEXT, type TEXT, is_local INTEGER DEFAULT 0);
		CREATE TABLE provider_discovered_models (provider_id TEXT, model_id TEXT, output_price_per_mtok REAL);
		INSERT INTO providers (id, name, type, is_local) VALUES ('ollama', 'Ollama', 'ollama', 1);
		INSERT INTO providers (id, name, type, is_local) VALUES ('anthropic', 'Anthropic', 'anthropic', 0);
		INSERT INTO providers (id, name, type, is_local) VALUES ('openai', 'OpenAI', 'openai_compat', 0);
	`)
	if err != nil {
		t.Fatalf("failed to setup test tables: %v", err)
	}
	return db
}

func TestAgent_ResolveModelRoute_LocalOnlyMode(t *testing.T) {
	ctx := context.Background()
	a := NewAgentWithDefaults("sess-local-only")
	a.extQuerier = setupTestRouteDB(t)

	origCfg := config.Get()
	defer config.Update(origCfg)

	var newCfg config.Config
	if origCfg != nil {
		newCfg = *origCfg
	}
	newCfg.Security.LocalOnlyMode = true
	config.Update(&newCfg)

	// 非 local 模型在 local_only_mode 下被硬拦截，拒绝执行（V3 / 隐私保护）
	a.SetModelLock("anthropic/claude-3-7-sonnet")
	_, _, _, err := a.resolveModelRoute(ctx, "default", "none")
	if err == nil {
		t.Fatal("V3 缺陷：local_only_mode 必须拒绝云端模型引用，不能静默放行或回退")
	}
	if !strings.Contains(err.Error(), "local_only_mode") {
		t.Fatalf("期望 local_only_mode 错误，实际: %v", err)
	}

	// 本地模型（如 ollama/llama3）应正常放行
	a.SetModelLock("ollama/llama3")
	ref, _, _, err := a.resolveModelRoute(ctx, "default", "none")
	if err != nil {
		t.Fatalf("unexpected error on local model: %v", err)
	}
	if ref != "ollama/llama3" {
		t.Fatalf("expected local model to be permitted, got ref=%q", ref)
	}
}

func TestAgent_SwitchModel_LocalOnlyMode(t *testing.T) {
	a := NewAgentWithDefaults("sess-switch-local")
	a.extQuerier = setupTestRouteDB(t)

	origCfg := config.Get()
	defer config.Update(origCfg)

	var newCfg config.Config
	if origCfg != nil {
		newCfg = *origCfg
	}
	newCfg.Security.LocalOnlyMode = true
	config.Update(&newCfg)

	// 1. 尝试切换到云端模型：必须被拦截
	_, err := a.SwitchModel("openai/gpt-4o", "", "")
	if err == nil {
		t.Fatal("V3 缺陷：SwitchModel 在 local_only_mode 下应拒绝切换到云端模型")
	}
	if !strings.Contains(err.Error(), "local_only_mode") {
		t.Fatalf("期望包含 local_only_mode 错误，实际: %v", err)
	}

	// 2. 切换到本地模型：正常通过
	msg, err := a.SwitchModel("ollama/llama3", "", "none")
	if err != nil {
		t.Fatalf("unexpected error switching to local model: %v", err)
	}
	if !strings.Contains(msg, "ollama/llama3") {
		t.Fatalf("expected switch confirmation, got %s", msg)
	}
}

func TestAgent_ResolveModelRoute_CostLimit(t *testing.T) {
	ctx := context.Background()
	a := NewAgentWithDefaults("sess-cost-limit")

	origCfg := config.Get()
	defer config.Update(origCfg)

	var newCfg config.Config
	if origCfg != nil {
		newCfg = *origCfg
	} else {
		newCfg.Thresholds = config.DefaultThresholds()
	}
	newCfg.Thresholds.M1Router.ModelSelectMaxOutputPricePerMtok = 5.0
	config.Update(&newCfg)

	// 使用内存 sqlite 数据库测试真实 SQL 查询
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(ctx, `
		CREATE TABLE providers (id TEXT, name TEXT, type TEXT, is_local INTEGER DEFAULT 0);
		CREATE TABLE provider_discovered_models (provider_id TEXT, model_id TEXT, output_price_per_mtok REAL);
		INSERT INTO providers VALUES ('prov_custom', 'custom', 'openai_compat', 0);
		INSERT INTO provider_discovered_models VALUES ('prov_custom', 'expensive-model', 15.0);
	`)
	if err != nil {
		t.Fatalf("failed to setup test tables: %v", err)
	}

	a.extQuerier = db
	a.SetModelLock("prov_custom/expensive-model")

	_, _, _, err = a.resolveModelRoute(ctx, "default", "none")
	if err == nil {
		t.Fatal("expected error due to max_output_price_per_mtok violation")
	}
	if !strings.Contains(err.Error(), "max_output_price_per_mtok") {
		t.Fatalf("expected error containing 'max_output_price_per_mtok', got: %v", err)
	}
}
