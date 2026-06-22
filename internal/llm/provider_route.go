package llm

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// Provider Router — 三层递进路由。
// 架构文档: docs/arch/01-Inference-Runtime-深度选型.md §4

func Route(ctx context.Context, req *types.InferRequest, providers []protocol.Provider) (protocol.Provider, error) {
	for _, p := range providers {
		if p.Capabilities().MaxContextTokens < req.MaxTokens {
			continue
		}
		if !p.Capabilities().SupportsTools && len(req.Tools) > 0 {
			continue
		}
		return p, nil
	}

	tier := determineComplexity(req)
	for _, p := range providers {
		if matchProviderTier(p, tier) {
			return p, nil
		}
	}

	return nil, apperr.New(apperr.CodeInternal, "all providers exhausted")
}

func determineComplexity(req *types.InferRequest) int {
	outputEstimate := estimateOutputTokens(req)
	if len(req.Tools) > 5 || outputEstimate > 4096 {
		return 3
	}
	if len(req.Tools) > 1 || outputEstimate > 1024 {
		return 2
	}
	return 1
}

// estimateOutputTokens 基于消息长度估算输出 tokens（近似：输入长度的 1/3，最小 512）。
func estimateOutputTokens(req *types.InferRequest) int {
	inputLen := 0
	for _, m := range req.Messages {
		inputLen += len(m.Content)
	}
	// 简单比例估算：每 4 字节约 1 token，输出 ≈ 输入 token 的 1/3
	estimated := inputLen / 12
	if estimated < 512 {
		return 512
	}
	return estimated
}

// matchProviderTier 按复杂度层级选择 Provider：
// tier 3 → 要求支持工具调用且上下文 ≥ 32K；
// tier 2 → 要求支持工具调用；
// tier 1 → 无特殊要求，首个可用 Provider。
func matchProviderTier(p protocol.Provider, tier int) bool {
	caps := p.Capabilities()
	switch tier {
	case 3:
		return caps.SupportsTools && caps.MaxContextTokens >= 32000
	case 2:
		return caps.SupportsTools
	default:
		return true
	}
}
