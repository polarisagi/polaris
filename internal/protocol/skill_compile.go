package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// Logic Collapse 编译流水线跨模块契约（M06 §2.2, M09 §1.1）。
//
// producer: internal/extension/skill（LogicCollapseCompiler 具体实现）
// consumer: internal/learning（LogicCollapseMonitor 触发编译）
//
// 这些类型此前分别以 internal/extension/skill.CollapseTrajectory 等具体类型
// 由 internal/learning 直接 import 消费，违反 M04 §B2（跨模块共享类型须在
// internal/protocol/ 定义）。现收敛至此，extension/skill 与 learning 均通过
// 类型别名/直接引用本文件的定义，不再互相 import 对方的具体实现包。

// MinSkillSuccessCount 编译前安全闸门：最少成功次数。
const MinSkillSuccessCount = 50

// MinSkillSemanticVariance 最小语义方差（低于此 → 多样性不足 → 拒绝）。
const MinSkillSemanticVariance = 0.1

// CollapseEntity 轨迹中的实体（用于时效性检查）。
type CollapseEntity struct {
	Type  string
	Value string
}

// CollapseToolCall 工具调用类型签名（DataStripping 后无参数值）。
type CollapseToolCall struct {
	ToolName   string
	Args       map[string]string // key → 类型字符串
	OutputType string
	OrderIndex int
}

// CollapseTrajectory 传递给编译器的轨迹数据。
type CollapseTrajectory struct {
	SkillID           string
	GoalDescription   string
	ToolCalls         []CollapseToolCall
	InputSchema       map[string]string // param_name → TypeScript 类型
	OutputSchema      map[string]string
	RiskLevel         string // low / medium / high
	SuccessCount      int
	SemanticVariance  float64
	CompletedAt       int64 // unix seconds
	Entities          []CollapseEntity
	SemanticClusterID string
	TaintLevel        int // 0=None, 1=Low, 2=Medium, 3+=High
}

// CompileRequest 编译请求。
type CompileRequest struct {
	Trajectory     *CollapseTrajectory
	EvalGatePassed bool
	SigningKey     []byte
	WorkDir        string
}

// CompileResult 编译结果（TypeScript 脚本）。
type CompileResult struct {
	ScriptSource []byte // TypeScript 源码
	ScriptHash   string // SHA-256 hex
	Signature    string
	RiskLevel    string
	SandboxTier  int
	SkillMeta    types.SkillMeta
}

// LLMCodeGenerator LLM 代码生成接口（TypeScript/Python 技能）。
// 实现：internal/learning.defaultLLMCodeGenerator（基于 protocol.Provider）
type LLMCodeGenerator interface {
	GenerateImpl(ctx context.Context, traj *CollapseTrajectory) ([]byte, error)
}
