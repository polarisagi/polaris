package llm

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestCrossPoolFallback_RoleLessProviderStillServed 守护 Tier-0 最常见形态：
// 只注册了一个 role 为空串的本地 Provider（boot_substrate.go 的 reg.Register），
// 请求却指定了 ModelPool="reasoning"。
//
// 若把 Pool 当硬约束，这里会在健康 Provider 就在眼前时直接 CodeResourceExhausted
// 拒绝服务——比 GD-13-005 原本要修的"单池耗尽即断链"更糟。降级链尾部的
// "不限 role 全局兜底"就是为此存在，删掉它即回归。
func TestCrossPoolFallback_RoleLessProviderStillServed(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	local := &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 1.0}}
	reg.Register("ollama-local", "Local LLM", local) // role == ""

	router := NewInferenceRouter(reg, nil)
	resp, err := router.Infer(context.Background(),
		[]types.Message{{Role: "user", Content: "hi"}},
		types.WithModelPool("reasoning"),
	)
	if err != nil {
		t.Fatalf("role-less local provider must still serve a pooled request, got err: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.DegradedFromPool != "reasoning" {
		t.Fatalf("expected DegradedFromPool='reasoning' to surface the downgrade, got %q", resp.DegradedFromPool)
	}
}

// TestCrossPoolFallback_EmptyPoolTriggersFallback 目标 Pool 一个 Provider 都没
// 注册时也必须走降级链。此前初始选择返回 nil 就直接报错，导致降级机制在
// "该 Pool 压根没配置"这一最常见触发场景下完全不会被调用。
func TestCrossPoolFallback_EmptyPoolTriggersFallback(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	// 只有 general Pool，reasoning Pool 为空
	reg.RegisterWithRole("general-p1", "GeneralP1", "general",
		&mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 1.0}})

	router := NewInferenceRouter(reg, nil)
	resp, err := router.Infer(context.Background(),
		[]types.Message{{Role: "user", Content: "hi"}},
		types.WithModelPool("reasoning"),
	)
	if err != nil {
		t.Fatalf("empty target pool must fall back, got err: %v", err)
	}
	if resp.DegradedFromPool != "reasoning" {
		t.Fatalf("expected DegradedFromPool='reasoning', got %q", resp.DegradedFromPool)
	}
}

// TestStreamCrossPoolFallback_EmitsNotice 流式路径同样要支持跨 Pool 降级，
// 并在流首插入一条 StreamSystemNotice 告知用户已切换模型。
// Task 07 只给非流式 Infer 实现了降级，而交互式对话走的恰恰是 StreamInfer——
// 等于该特性在主用户路径上不生效。
func TestStreamCrossPoolFallback_EmitsNotice(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	reg.RegisterWithRole("general-p1", "GeneralP1", "general",
		&mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 1.0}})

	router := NewInferenceRouter(reg, nil)
	ch, err := router.StreamInfer(context.Background(),
		[]types.Message{{Role: "user", Content: "hi"}},
		types.WithModelPool("reasoning"),
	)
	if err != nil {
		t.Fatalf("stream cross-pool fallback must succeed, got err: %v", err)
	}

	var sawNotice bool
	var firstType types.StreamEventType
	first := true
	for ev := range ch {
		if first {
			firstType = ev.Type
			first = false
		}
		if ev.Type == types.StreamSystemNotice {
			sawNotice = true
			if ev.Content == "" {
				t.Fatal("degrade notice must carry user-facing text")
			}
		}
	}
	if !sawNotice {
		t.Fatal("expected a StreamSystemNotice announcing the cross-pool downgrade")
	}
	if firstType != types.StreamSystemNotice {
		t.Fatalf("notice must be the first event so the UI can show it up front, got %v", firstType)
	}
}

// TestNoPoolSpecified_KeepsGlobalBest 未指定 Pool 时行为完全不变（全局 best），
// 不得因引入降级链而改变既有路由语义。
func TestNoPoolSpecified_KeepsGlobalBest(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	reg.RegisterWithRole("reasoning-p1", "ReasoningP1", "reasoning",
		&mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 1.0}})

	router := NewInferenceRouter(reg, nil)
	resp, err := router.Infer(context.Background(), []types.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unpooled request must use global best: %v", err)
	}
	if resp.DegradedFromPool != "" {
		t.Fatalf("unpooled request must not be marked as degraded, got %q", resp.DegradedFromPool)
	}
}
