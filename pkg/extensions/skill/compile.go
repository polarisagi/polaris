package skill

// Logic Collapse 编译流水线。
// 架构文档: docs/arch/M06-Skill-Library.md §2.2
// 技能以 TypeScript 脚本形式生成，通过 npx tsx 直接运行，无需预编译。
// 顺序: freshnessCheck → taintCheck → compileGate → LLM 代码生成 → 静态分析 → 风险分级 → 签名 → 入库

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sync/atomic"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// ─── 常量与错误 ──────────────────────────────────────────────────────────────

const (
	MinSuccessCount     = 50  // 编译前安全闸门: 最少成功次数（对外导出，M9 复用）
	MinSemanticVariance = 0.1 // 最小语义方差（低于此 → 多样性不足 → 拒绝，对外导出）

	compileMinFreeMemMB = 80 // CompileGate: 最小剩余内存(MB)
)

var (
	ErrLogicCollapseDisabled    = perrors.New(perrors.CodeInternal, "logic collapse: feature gate disabled (Tier0 or insufficient memory)")
	ErrInsufficientSuccessCount = perrors.New(perrors.CodeInternal, "logic collapse: success_count < 50")
	ErrInsufficientDiversity    = perrors.New(perrors.CodeInternal, "logic collapse: semantic_variance < 0.1 — needs_more_diversity")
	ErrEvalGateNotPassed        = perrors.New(perrors.CodeInternal, "logic collapse: eval gate not passed")
	ErrStaleTrajectory          = perrors.New(perrors.CodeInternal, "logic collapse: stale trajectory — needs_adaptation")
	ErrCompileGateRejected      = perrors.New(perrors.CodeInternal, "logic collapse: compile gate rejected (memory or concurrency limit)")
	ErrCompileGateBusy          = perrors.New(perrors.CodeInternal, "logic collapse: compile gate busy")
	ErrTaintedTrajectory        = perrors.New(perrors.CodeInternal, "logic collapse: TaintMedium+ trajectory rejected — tainted_trajectory")
)

// ─── 核心类型 ─────────────────────────────────────────────────────────────────

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
	CompletedAt       int64            // unix seconds
	Entities          []CollapseEntity // 用于 Freshness Check
	SemanticClusterID string
	TaintLevel        int // 0=None, 1=Low, 2=Medium, 3+=High
}

// CollapseToolCall 工具调用类型签名（DataStripping 后无参数值）。
type CollapseToolCall struct {
	ToolName   string
	Args       map[string]string // key → 类型字符串
	OutputType string
	OrderIndex int
}

// CollapseEntity 轨迹中的实体（用于时效性检查）。
type CollapseEntity struct {
	Type  string
	Value string
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
	SkillMeta    protocol.SkillMeta
}

// ─── 接口 ─────────────────────────────────────────────────────────────────────

// LLMCodeGenerator LLM 代码生成接口（TypeScript 技能）。
type LLMCodeGenerator interface {
	GenerateImpl(ctx context.Context, traj *CollapseTrajectory) ([]byte, error)
}

// ─── CompileGate — 内存 + 并发准入 ───────────────────────────────────────────

// CompileGate 控制并发编译数量，防止内存超限。
type CompileGate struct {
	maxConcurrent int64
	inFlight      atomic.Int64
}

func NewCompileGate(tier observability.Tier) *CompileGate {
	max := int64(2)
	if tier >= observability.Tier1 {
		max = 4
	}
	return &CompileGate{maxConcurrent: max}
}

func (g *CompileGate) TryAcquire(freeMB int64) bool {
	if freeMB < compileMinFreeMemMB {
		return false
	}
	if g.inFlight.Load() >= g.maxConcurrent {
		return false
	}
	g.inFlight.Add(1)
	return true
}

func (g *CompileGate) Release() { g.inFlight.Add(-1) }

func (g *CompileGate) InFlight() int { return int(g.inFlight.Load()) }

// ─── LogicCollapseCompiler — 主编译器 ────────────────────────────────────────

// LogicCollapseCompiler 将轨迹蒸馏为 TypeScript 技能并注册。
type LogicCollapseCompiler struct {
	gate       *CompileGate
	codeGen    LLMCodeGenerator
	registry   protocol.SkillRegistry
	signingKey []byte
	workDir    string
}

// LogicCollapseConfig 编译器配置。
type LogicCollapseConfig struct {
	Gate       *CompileGate
	CodeGen    LLMCodeGenerator
	Registry   protocol.SkillRegistry
	SigningKey []byte
	WorkDir    string
}

func NewLogicCollapseCompiler(cfg LogicCollapseConfig) *LogicCollapseCompiler {
	return &LogicCollapseCompiler{
		gate:       cfg.Gate,
		codeGen:    cfg.CodeGen,
		registry:   cfg.Registry,
		signingKey: cfg.SigningKey,
		workDir:    cfg.WorkDir,
	}
}

// Compile 将轨迹蒸馏为 TypeScript 技能脚本。
func (c *LogicCollapseCompiler) Compile(ctx context.Context, req *CompileRequest) (*CompileResult, error) {
	if req.Trajectory == nil {
		return nil, perrors.New(perrors.CodeInternal, "compile: nil trajectory")
	}
	if req.Trajectory.TaintLevel >= 2 {
		return nil, ErrTaintedTrajectory
	}
	if !req.EvalGatePassed {
		return nil, ErrEvalGateNotPassed
	}

	if c.gate != nil {
		freeMB := int64(observability.ProbeAvailableMemoryMB())
		if !c.gate.TryAcquire(freeMB) {
			return nil, ErrCompileGateBusy
		}
		defer c.gate.Release()
	}

	// LLM 生成 TypeScript 脚本
	src, err := c.codeGen.GenerateImpl(ctx, req.Trajectory)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "compile: LLM code gen failed", err)
	}

	// 风险分级
	riskLevel, sandboxTier := assessScriptRisk(src, "llm_generated")

	// 签名
	sig, err := signScript(src, c.signingKey)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "compile: signing failed", err)
	}

	// 计算 hash
	sum := sha256.Sum256(src)
	scriptHash := hex.EncodeToString(sum[:])

	// 注册技能
	skillMeta := protocol.SkillMeta{
		Name:       req.Trajectory.SkillID,
		Version:    "1.0.0",
		Runtime:    "script",
		RiskLevel:  riskLevel,
		Sandbox:    sandboxTier,
		Trust:      protocol.TrustSystem,
		ExecMode:   "tool",
		ScriptPath: req.WorkDir + "/" + req.Trajectory.SkillID + "/src/index.ts",
	}
	if c.registry != nil {
		if err := c.registry.Register(ctx, skillMeta); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "compile: registry.Register failed", err)
		}
	}

	return &CompileResult{
		ScriptSource: src,
		ScriptHash:   scriptHash,
		Signature:    sig,
		RiskLevel:    riskLevel,
		SandboxTier:  sandboxTier,
		SkillMeta:    skillMeta,
	}, nil
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

// assessScriptRisk 基于 TypeScript 源码评估风险级别和推荐沙箱层级。
func assessScriptRisk(src []byte, source string) (riskLevel string, sandboxTier int) {
	s := string(src)
	switch {
	case contains(s, "exec(", "child_process", "spawnSync"):
		return "high", 3 // L3 Container
	case contains(s, "fetch(", "http.", "axios", "net."):
		return "medium", 3 // L3 Container（网络需最高隔离）
	case contains(s, "fs.write", "writeFile", "createWriteStream"):
		return "medium", 3 // L3 Container
	case source == "builtin" || source == "user_verified":
		return "low", 1 // L1 InProcess
	default:
		return "medium", 2 // L2 SandboxWasm
	}
}

func contains(s string, patterns ...string) bool {
	for _, p := range patterns {
		if len(s) > 0 && len(p) > 0 {
			for i := range s {
				if i+len(p) <= len(s) && s[i:i+len(p)] == p {
					return true
				}
			}
		}
	}
	return false
}

func signScript(src []byte, key []byte) (string, error) {
	if len(key) == 0 {
		return "", perrors.New(perrors.CodeInternal, "signing key not configured")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(src)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
