package skill

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestRegistry_RegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()

	meta := protocol.SkillMeta{
		Name:         "skill:code_review",
		Version:      "1.0.0",
		Runtime:      "script",
		RiskLevel:    "low",
		Sandbox:      1,
		Capabilities: []string{"read_file", "analyze"},
		Trust:        protocol.TrustSystem,
	}

	if err := reg.Register(ctx, meta); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Get by name
	got, err := reg.Get(ctx, "skill:code_review", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "skill:code_review" {
		t.Errorf("名称不匹配: %s", got.Name)
	}
	if got.Version != "1.0.0" {
		t.Errorf("版本不匹配: %s", got.Version)
	}

	// Get by version
	_, err = reg.Get(ctx, "skill:code_review", "2.0.0")
	if err == nil {
		t.Error("版本不匹配应返回错误")
	}
}

func TestRegistry_RegisterRejectInvalidName(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()

	meta := protocol.SkillMeta{
		Name:    "code_review", // 缺少 "skill:" 前缀
		Version: "1.0.0",
		Trust:   protocol.TrustSystem,
	}

	err := reg.Register(ctx, meta)
	if err == nil {
		t.Error("缺少 skill: 前缀应被拒绝")
	}
}

func TestRegistry_RegisterRejectUnsigned(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()

	meta := protocol.SkillMeta{
		Name:    "skill:unsigned",
		Version: "1.0.0",
		Trust:   protocol.TrustUntrusted,
	}

	err := reg.Register(ctx, meta)
	if err == nil {
		t.Error("未签名技能应被拒绝")
	}
}

func TestRegistry_ListWithFilter(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()

	reg.Register(ctx, protocol.SkillMeta{
		Name: "skill:a", Version: "1.0", RiskLevel: "low",
		Capabilities: []string{"read"}, Trust: protocol.TrustSystem,
	})
	reg.Register(ctx, protocol.SkillMeta{
		Name: "skill:b", Version: "1.0", RiskLevel: "high",
		Capabilities: []string{"write"}, Trust: protocol.TrustSystem,
	})
	reg.Register(ctx, protocol.SkillMeta{
		Name: "skill:c", Version: "1.0", RiskLevel: "medium",
		Capabilities: []string{"read", "write"}, Trust: protocol.TrustSystem,
	})
	// 废弃一个
	reg.Deprecate(ctx, "skill:b", "", "测试")

	// 不包含废弃技能
	all, _ := reg.List(ctx, protocol.SkillFilter{IncludeDeprecated: false})
	if len(all) != 2 {
		t.Errorf("不应包含废弃技能, 期望 2 实际 %d", len(all))
	}

	// 包含废弃技能
	all, _ = reg.List(ctx, protocol.SkillFilter{IncludeDeprecated: true})
	if len(all) != 3 {
		t.Errorf("应包含废弃技能, 期望 3 实际 %d", len(all))
	}

	// 按 RiskLevel 过滤
	all, _ = reg.List(ctx, protocol.SkillFilter{RiskLevelMax: "low", IncludeDeprecated: true})
	if len(all) != 1 {
		t.Errorf("RiskLevelMax=low 应 1 个, 实际 %d", len(all))
	}

	// 按 Capability 过滤
	all, _ = reg.List(ctx, protocol.SkillFilter{
		Capabilities:      []string{"read", "write"},
		IncludeDeprecated: true,
	})
	if len(all) != 1 {
		t.Errorf("双能力匹配应 1 个, 实际 %d", len(all))
	}
}

func TestRegistry_Deprecate(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()

	reg.Register(ctx, protocol.SkillMeta{
		Name: "skill:to_deprecate", Version: "1.0",
		Trust: protocol.TrustSystem,
	})

	if err := reg.Deprecate(ctx, "skill:to_deprecate", "", "不再需要"); err != nil {
		t.Fatalf("Deprecate: %v", err)
	}

	_, err := reg.Get(ctx, "skill:to_deprecate", "")
	if err != nil {
		t.Fatalf("Get 废弃技能应成功: %v", err)
	}

	log := reg.AuditLog()
	if len(log) != 1 {
		t.Errorf("审计日志应有 1 条, 实际 %d", len(log))
	}
}

func TestSelector_HeuristicScore(t *testing.T) {
	reg := NewRegistry()
	sel := NewSelector(reg)
	ctx := context.Background()

	// 注册一个高分技能
	reg.Register(ctx, protocol.SkillMeta{
		Name: "skill:best_match", Version: "1.0",
		RiskLevel:    "high",
		Capabilities: []string{"read", "analyze", "write"},
		Trust:        protocol.TrustSystem,
		Benchmarks: protocol.SkillBenchmarks{
			PassRate:     0.95,
			AvgLatencyMs: 500,
			AvgTokens:    200,
		},
	})

	// 注册一个低分技能
	reg.Register(ctx, protocol.SkillMeta{
		Name: "skill:worst_match", Version: "1.0",
		RiskLevel:    "low",
		Capabilities: []string{"read"},
		Trust:        protocol.TrustSystem,
		Benchmarks: protocol.SkillBenchmarks{
			PassRate:     0.5,
			AvgLatencyMs: 6000,
			AvgTokens:    1000,
		},
	})

	hint := protocol.TaskHint{
		TaskType:           "code_review",
		CapabilitiesNeeded: []string{"read", "analyze", "write"},
		ComplexityScore:    0.9,
	}

	results, err := sel.Select(ctx, hint)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("应返回至少 1 个结果")
	}
	if results[0].Name != "skill:best_match" {
		t.Errorf("最高分应为 best_match, 实际 %s", results[0].Name)
	}
}

func TestSelector_SelectEmptyRegistry(t *testing.T) {
	reg := NewRegistry()
	sel := NewSelector(reg)

	results, _ := sel.Select(context.Background(), protocol.TaskHint{
		TaskType: "unknown",
	})
	if len(results) != 0 {
		t.Errorf("空 Registry 应返回 0 结果, 实际 %d", len(results))
	}
}
