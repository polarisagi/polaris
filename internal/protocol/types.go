package protocol

import (
	"context"
	"time"

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

// CatalogEntry 表示工具目录中的单个工具元数据。
type CatalogEntry struct {
	Name        string
	Description string
	Parameters  any              // JSON Schema
	Source      types.ToolSource // builtin / mcp / skill / native
	Capability  types.CapabilityLevel
	TrustTier   types.TrustTier
	TaintLevel  types.TaintLevel
	Timeout     time.Duration
	// 执行路由所需元数据
	MCPServerID string // Source==ToolMCP 时有效
	MCPToolName string // MCP 协议原始工具名（非 LLM 调用名）
	SkillName   string // Source==ToolSkill 时有效（"skill:xxx" 格式）
}

// MockResponse 表示 032_mock_response_cache 表中的一条记录。
type MockResponse struct {
	OperationHash string
	PlanSessionID string
	Method        string
	URLPattern    string
	StatusCode    int
	ResponseBody  string
	HitCount      int
	CreatedAt     int64
	ExpiresAt     *int64 // 可能为 nil，表示随 planner_session 生命周期过期
}
