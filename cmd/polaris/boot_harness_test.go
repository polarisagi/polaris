package main

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/types"
)

type recordingProvider struct {
	lastMessages []types.Message
}

func (r *recordingProvider) Infer(_ context.Context, msgs []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	r.lastMessages = msgs
	return &types.ProviderResponse{Content: "[]"}, nil
}
func (r *recordingProvider) StreamInfer(_ context.Context, _ []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
func (r *recordingProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsStreaming: false}
}
func (r *recordingProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (r *recordingProvider) ModelID() string                      { return "recording" }

// TestV2_RouterInferCallerAdapter_PIITokenization 验证 sleep-time 等后台提案器
// 经由 routerInferCallerAdapter 调用 LLM 时，在 standard 档下经过 PII 令牌化脱敏（V2 / HE-2）。
func TestV2_RouterInferCallerAdapter_PIITokenization(t *testing.T) {
	reg := llm.NewProviderRegistry(config.M1RouterThresholds{})
	recProv := &recordingProvider{}
	reg.Register("default", "default", recProv)

	router := llm.NewInferenceRouter(reg, nil)
	vault := guard.NewPIITokenVault()
	pol := policy.ProfilePolicy(policy.ProfileStandard)

	// 提前在 vault 中记录真实 PII
	vault.TokenizeForTask("sleeptime", "secret-user-email@test.com")

	adapter := &routerInferCallerAdapter{
		router: router,
		vault:  vault,
		policy: pol,
	}

	req := types.InferRequest{
		Messages: []types.Message{
			{Role: "user", Content: "Weakness trace contains sensitive email: secret-user-email@test.com"},
		},
	}
	opts := types.InferOptions{
		Purpose: "self_harness_propose",
	}

	_, err := adapter.CallInfer(context.Background(), req, opts)
	if err != nil {
		t.Fatalf("CallInfer failed: %v", err)
	}

	if len(recProv.lastMessages) == 0 {
		t.Fatal("provider must receive messages")
	}

	gotContent := recProv.lastMessages[0].Content
	if strings.Contains(gotContent, "secret-user-email@test.com") {
		t.Fatalf("V2 缺陷：PII 明文未被令牌化，泄漏到 Provider！内容: %s", gotContent)
	}
	if !strings.Contains(gotContent, "⟦PII:") {
		t.Fatalf("期望包含 ⟦PII:xxxx⟧ 令牌，实际: %s", gotContent)
	}
}
