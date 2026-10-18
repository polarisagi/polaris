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

func TestAgent_ResolveModelRoute_LocalOnlyMode(t *testing.T) {
	ctx := context.Background()
	a := NewAgentWithDefaults("sess-local-only")

	origCfg := config.Get()
	defer config.Update(origCfg)

	var newCfg config.Config
	if origCfg != nil {
		newCfg = *origCfg
	}
	newCfg.Security.LocalOnlyMode = true
	config.Update(&newCfg)

	// 非 local 模型被拦截，并回退到默认池
	a.SetModelLock("anthropic/claude-3-7-sonnet")
	ref, pool, _, err := a.resolveModelRoute(ctx, "default", "none")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref != "" || pool != "default" {
		t.Fatalf("expected non-local model to fallback to default pool, got ref=%q, pool=%q", ref, pool)
	}

	// 本地模型（如 ollama/llama3）应正常放行
	a.SetModelLock("ollama/llama3")
	ref, _, _, err = a.resolveModelRoute(ctx, "default", "none")
	if err != nil {
		t.Fatalf("unexpected error on local model: %v", err)
	}
	if ref != "ollama/llama3" {
		t.Fatalf("expected local model to be permitted, got ref=%q", ref)
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
		CREATE TABLE providers (id TEXT, name TEXT, type TEXT);
		CREATE TABLE provider_discovered_models (provider_id TEXT, model_id TEXT, output_price_per_mtok REAL);
		INSERT INTO providers VALUES ('prov_custom', 'custom', 'openai_compat');
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
