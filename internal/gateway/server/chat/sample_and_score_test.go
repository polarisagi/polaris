package chat

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/eval/analysis"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeLLMRegistry 记录 PickProvider 被以哪些 role 依次调用，验证
// SampleAndScoreReply 的 "default" → "general" 兜底链与
// system_prompt.go InjectSystemPrompt 保持同一 Provider 取用方式。
type fakeLLMRegistry struct {
	pickedRoles []string
	byRole      map[string]protocol.Provider
}

func (f *fakeLLMRegistry) PickProvider(role string) protocol.Provider {
	f.pickedRoles = append(f.pickedRoles, role)
	return f.byRole[role]
}
func (f *fakeLLMRegistry) PickProviderName(role string) string                                  { return "" }
func (f *fakeLLMRegistry) PickProviderByRecordID(mID string) protocol.Provider                  { return nil }
func (f *fakeLLMRegistry) UnregisterAll()                                                       {}
func (f *fakeLLMRegistry) RegisterWithRole(name, displayName, role string, p protocol.Provider) {}

type noopProvider struct{}

func (noopProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: "0.5"}, nil
}
func (noopProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
func (noopProvider) Capabilities() types.ProviderCapabilities { return types.ProviderCapabilities{} }
func (noopProvider) Tokenizer() protocol.TokenizerAdapter     { return nil }
func (noopProvider) ModelID() string                          { return "noop" }

// TestSampleAndScoreReply_NilSamplingMonitor_NoOp 2026-07-14：nil
// SamplingMonitor（未注入场景，与生产 boot 前/测试环境一致）必须安全跳过，
// 不 panic、不触碰 Registry。
func TestSampleAndScoreReply_NilSamplingMonitor_NoOp(t *testing.T) {
	reg := &fakeLLMRegistry{}
	h := &ChatHandler{Registry: reg, SamplingMonitor: nil}
	h.SampleAndScoreReply("session-1", "q", "r")
	if len(reg.pickedRoles) != 0 {
		t.Errorf("expected Registry untouched when SamplingMonitor is nil, got PickProvider calls: %v", reg.pickedRoles)
	}
}

// TestSampleAndScoreReply_NilRegistry_NoOp Registry 未注入时同样安全跳过。
func TestSampleAndScoreReply_NilRegistry_NoOp(t *testing.T) {
	h := &ChatHandler{Registry: nil, SamplingMonitor: analysis.NewContinuousSamplingMonitor(nil)}
	h.SampleAndScoreReply("session-1", "q", "r") // 不应 panic
}

// TestSampleAndScoreReply_FallsBackFromDefaultToGeneral 验证 Provider 取用
// 顺序与 system_prompt.go 一致：PickProvider("default") 为 nil 时兜底
// PickProvider("general")。
func TestSampleAndScoreReply_FallsBackFromDefaultToGeneral(t *testing.T) {
	reg := &fakeLLMRegistry{byRole: map[string]protocol.Provider{"general": noopProvider{}}}
	h := &ChatHandler{Registry: reg, SamplingMonitor: analysis.NewContinuousSamplingMonitor(nil)}
	h.SampleAndScoreReply("session-1", "q", "r")
	want := []string{"default", "general"}
	if len(reg.pickedRoles) != len(want) || reg.pickedRoles[0] != want[0] || reg.pickedRoles[1] != want[1] {
		t.Errorf("expected PickProvider role sequence %v, got %v", want, reg.pickedRoles)
	}
}

// TestSampleAndScoreReply_DefaultAvailable_SkipsGeneralFallback default 角色
// 有 Provider 时不应再兜底查询 general。
func TestSampleAndScoreReply_DefaultAvailable_SkipsGeneralFallback(t *testing.T) {
	reg := &fakeLLMRegistry{byRole: map[string]protocol.Provider{"default": noopProvider{}}}
	h := &ChatHandler{Registry: reg, SamplingMonitor: analysis.NewContinuousSamplingMonitor(nil)}
	h.SampleAndScoreReply("session-1", "q", "r")
	if len(reg.pickedRoles) != 1 || reg.pickedRoles[0] != "default" {
		t.Errorf("expected only PickProvider(\"default\") called, got %v", reg.pickedRoles)
	}
}
