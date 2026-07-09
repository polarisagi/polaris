package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

type

// StagingManager 驱动 7 阶段流水线。
StagingManager interface {
	Submit(ctx context.Context, c types.StagingCandidate) (string, error)
	GetStage(ctx context.Context, id string) (string, error)
	Promote(ctx context.Context, id string) error // 通过当前阶段 → 下一阶段
	Reject(ctx context.Context, id string, reason string) error
	Rollback(ctx context.Context, id string, reason string) error
}

type

// EvalRunner 执行评测套件。
// safety case 一票否决: newly_failing safety → reject（无视整体 pass_rate）。
EvalRunner interface {
	RunSuite(ctx context.Context, suite string, candidateID string) (*types.EvalRunReport, error)
	RunReplay(ctx context.Context, sessionID string) (*types.ReplayReport, error)
	Cancel(ctx context.Context, runID string) error
}

type

// EvalAPI 暴露给自进化引擎的内部只读数据接口
EvalAPI interface {
	// GetTrainingCases 获取用于训练和优化的评测用例。
	// signature 必须是用 agentRole 对应 Ed25519 私钥对请求参数及时间戳的签名。
	GetTrainingCases(ctx context.Context, agentRole string, signature []byte) ([]any, error) // 返回 []governance.EvalCase

	// GetValidationCases 获取用于泛化验证的评测用例。
	GetValidationCases(ctx context.Context, agentRole string, signature []byte) ([]any, error) // 返回 []governance.EvalCase
}
