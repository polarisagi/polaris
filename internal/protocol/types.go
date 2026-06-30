package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// LLMInferFunc 跨模块统一的 LLM 简单推理函数签名。
// 单 prompt 输入，单 string 输出，options 透传 InferOption 链。
// 消费方：memory/consolidation、knowledge/connector、swarm/agents。
// @canonical: 此处为唯一定义，各消费包以 type alias 引用，禁止重复声明。
type LLMInferFunc func(ctx context.Context, prompt string, opts ...types.InferOption) (string, error)

// ApplyInferOptions 合并选项，返回最终参数。
func ApplyInferOptions(opts []types.InferOption) types.InferOptions {
	o := types.InferOptions{ThinkingMode: types.ThinkingDisabled}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

type TokenizerAdapter interface {
	CountTokens(text string) int
	CountTokensBatch(texts []string) []int
}

type MultimodalTokenizer interface {
	TokenizerAdapter
	// CountImageTokens 估算图片 token
	CountImageTokens(width, height int, detail string) int
	// CountVideoTokens 估算视频 token
	CountVideoTokens(durationSecs float64, fps float64) int
	// EstimateRequest 估算完整 InferRequest 的输入 token 数
	EstimateRequest(req *types.InferRequest) int
}

type Iterator interface {
	Next() bool
	Key() []byte
	Value() []byte
	Err() error
	Close() error
}

type Transaction interface {
	Get(key []byte) ([]byte, error)
	Put(key, value []byte) error
	Delete(key []byte) error
	Scan(prefix []byte) (Iterator, error)
}
