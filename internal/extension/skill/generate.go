package skill

// Logic Collapse 编译流水线。
// 架构文档: docs/arch/M06-Skill-Library.md §2.2
// 技能以 Python 脚本形式生成（src/skill.py），通过 ContainerSandbox L3 执行（ADR-0026）。
// 顺序: freshnessCheck → taintCheck → evalGate → compileGate → DataStripping → LLM 代码生成 → ValidatePython → 风险分级 → 沙箱探针 → 签名 → 入库

import (
	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── 常量与错误 ──────────────────────────────────────────────────────────────

const (
	// MinSuccessCount / MinSemanticVariance 权威定义已上移至 protocol.MinSkillSuccessCount /
	// protocol.MinSkillSemanticVariance（M04 §B2，供 extension/skill 与 learning 共享）。
	// 此处保留同名常量作为包内简写，值恒等于 protocol 定义。
	MinSuccessCount     = protocol.MinSkillSuccessCount
	MinSemanticVariance = protocol.MinSkillSemanticVariance

	compileMinFreeMemMB = 80 // CompileGate: 最小剩余内存(MB)
)

var (
	ErrLogicCollapseDisabled    = apperr.New(apperr.CodeInternal, "logic collapse: feature gate disabled (Tier0 or insufficient memory)")
	ErrInsufficientSuccessCount = apperr.New(apperr.CodeInternal, "logic collapse: success_count < 50")
	ErrInsufficientDiversity    = apperr.New(apperr.CodeInternal, "logic collapse: semantic_variance < 0.1 — needs_more_diversity")
	ErrEvalGateNotPassed        = apperr.New(apperr.CodeInternal, "logic collapse: eval gate not passed")
	ErrStaleTrajectory          = apperr.New(apperr.CodeInternal, "logic collapse: stale trajectory — needs_adaptation")
	ErrCompileGateRejected      = apperr.New(apperr.CodeInternal, "logic collapse: compile gate rejected (memory or concurrency limit)")
	ErrCompileGateBusy          = apperr.New(apperr.CodeInternal, "logic collapse: compile gate busy")
	ErrTaintedTrajectory        = apperr.New(apperr.CodeInternal, "logic collapse: TaintMedium+ trajectory rejected — tainted_trajectory")
	ErrHighRiskTaintedScript    = apperr.New(apperr.CodeForbidden, "logic collapse: high-risk APIs in tainted trajectory — compile blocked")
	ErrSandboxProbeFailed       = apperr.New(apperr.CodeInternal, "logic collapse: sandbox probe failed")
)

// staleTrajectoryDays 轨迹超过此天数未更新视为陈腐（对应 state.yaml §m6_skill.freshness_threshold_days）。
const staleTrajectoryDays = 30

// ─── 核心类型 ─────────────────────────────────────────────────────────────────
//
// CollapseTrajectory / CollapseToolCall / CollapseEntity / CompileRequest /
// CompileResult / LLMCodeGenerator 权威定义已上移至 internal/protocol/skill_compile.go
// （M04 §B2：跨模块共享类型须在 internal/protocol/ 定义，internal/learning 消费方
// 不再直接 import 本包）。此处仅保留类型别名，包内代码与外部既有引用不受影响。

type CollapseTrajectory = protocol.CollapseTrajectory
type CollapseToolCall = protocol.CollapseToolCall
type CollapseEntity = protocol.CollapseEntity
type CompileRequest = protocol.CompileRequest
type CompileResult = protocol.CompileResult

// ─── 接口 ─────────────────────────────────────────────────────────────────────

// LLMCodeGenerator LLM 代码生成接口（TypeScript 技能）。
type LLMCodeGenerator = protocol.LLMCodeGenerator

// FreshnessChecker 检查轨迹实体新鲜度。
// 实现方应查询语义记忆，判断轨迹引用的实体是否仍然有效。
type FreshnessChecker interface {
	// IsFresh 返回 false 表示实体已陈腐（需要重新采样后再编译）。
	IsFresh(ctx context.Context, traj *CollapseTrajectory) bool
}

// ─── CompileGate — 内存 + 并发准入 ───────────────────────────────────────────

// CompileGate 控制并发编译数量，防止内存超限。
type CompileGate struct {
	maxConcurrent int64
	inFlight      atomic.Int64
}

func NewCompileGate(tier probe.Tier) *CompileGate {
	max := int64(2)
	if tier >= probe.Tier1 {
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
	gate             *CompileGate
	codeGen          LLMCodeGenerator
	registry         protocol.SkillRegistry
	freshnessChecker FreshnessChecker // nil 时使用时间戳兜底检查
	signingKey       []byte
	workDir          string
	containerSandbox sandbox.SandboxProvider
}

// LogicCollapseConfig 编译器配置。
type LogicCollapseConfig struct {
	Gate             *CompileGate
	CodeGen          LLMCodeGenerator
	Registry         protocol.SkillRegistry
	FreshnessChecker FreshnessChecker // 可选，nil 时用时间戳兜底
	SigningKey       []byte
	WorkDir          string
	ContainerSandbox sandbox.SandboxProvider
}

func NewLogicCollapseCompiler(cfg LogicCollapseConfig) *LogicCollapseCompiler {
	return &LogicCollapseCompiler{
		gate:             cfg.Gate,
		codeGen:          cfg.CodeGen,
		registry:         cfg.Registry,
		freshnessChecker: cfg.FreshnessChecker,
		signingKey:       cfg.SigningKey,
		workDir:          cfg.WorkDir,
		containerSandbox: cfg.ContainerSandbox,
	}
}

// Compile 将轨迹蒸馏为 TypeScript 技能脚本。
//
//nolint:gocyclo,nestif
func (c *LogicCollapseCompiler) Compile(ctx context.Context, req *CompileRequest) (*CompileResult, error) {
	if req.Trajectory == nil {
		return nil, apperr.New(apperr.CodeInternal, "compile: nil trajectory")
	}

	// Step 0: FreshnessCheck — 时效性校验（M06 §2.2 约定：freshnessCheck 为第一步）
	if c.freshnessChecker != nil {
		if !c.freshnessChecker.IsFresh(ctx, req.Trajectory) {
			return nil, ErrStaleTrajectory
		}
	} else {
		// 无注入检查器时，用 CompletedAt 时间戳兜底（超过 staleTrajectoryDays 天视为陈腐）
		if req.Trajectory.CompletedAt > 0 {
			age := time.Since(time.Unix(req.Trajectory.CompletedAt, 0))
			if age > time.Duration(staleTrajectoryDays)*24*time.Hour {
				return nil, ErrStaleTrajectory
			}
		}
	}

	if req.Trajectory.TaintLevel >= 2 {
		return nil, ErrTaintedTrajectory
	}
	if !req.EvalGatePassed {
		return nil, ErrEvalGateNotPassed
	}

	if c.gate != nil {
		freeMB := int64(probe.ProbeAvailableMemoryMB())
		if !c.gate.TryAcquire(freeMB) {
			return nil, ErrCompileGateBusy
		}
		defer c.gate.Release()
	}

	// Redact PII in schema before generating prompt
	req.Trajectory.InputSchema = redactPIIFields(req.Trajectory.InputSchema)
	req.Trajectory.OutputSchema = redactPIIFields(req.Trajectory.OutputSchema)

	// LLM 生成 Python 脚本
	src, err := c.codeGen.GenerateImpl(ctx, req.Trajectory)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "compile: LLM code gen failed", err)
	}

	// Python 静态安全检查（禁止 os/subprocess/socket/eval/exec）
	if err := ValidatePython(string(src)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "compile: Python static analysis failed", err)
	}

	// 风险分级
	riskLevel, sandboxTier := assessScriptRisk(src, "llm_generated")
	if riskLevel == "high" && types.TaintLevel(req.Trajectory.TaintLevel) >= types.TaintMedium {
		return nil, ErrHighRiskTaintedScript
	}

	// 沙箱探针
	if err := runSandboxProbe(ctx, c.containerSandbox, src, req.WorkDir); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "LogicCollapseCompiler.Compile", err)
	}

	// 签名
	sig, err := signScript(src, c.signingKey)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "compile: signing failed", err)
	}

	// 计算 hash
	sum := sha256.Sum256(src)
	scriptHash := hex.EncodeToString(sum[:])

	// 注册技能
	skillMeta := types.SkillMeta{
		Name:       req.Trajectory.SkillID,
		Version:    "1.0.0",
		Runtime:    "script",
		RiskLevel:  riskLevel,
		Sandbox:    sandboxTier,
		Trust:      types.TrustSystem,
		ExecMode:   "tool",
		ScriptPath: req.WorkDir + "/" + req.Trajectory.SkillID + "/src/skill.py",
	}
	if c.registry != nil {
		if err := c.registry.Register(ctx, skillMeta); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "compile: registry.Register failed", err)
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

// detectObfuscatedRisk 检测混淆绕过模式（正则多模式）。
// 覆盖：eval/Function构造器调用、动态属性拼接访问、base64解码执行、import()动态模块。
var (
	riskPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\beval\s*\(`),
		regexp.MustCompile(`\bexec\s*\(`),
		regexp.MustCompile(`import\s+os\b`),
		regexp.MustCompile(`import\s+subprocess\b`),
		regexp.MustCompile(`__import__\s*\(`),
		regexp.MustCompile(`getattr\s*\(`),
	}
)

func detectObfuscatedRisk(src string) bool {
	for _, re := range riskPatterns {
		if re.MatchString(src) {
			return true
		}
	}
	return false
}

// assessScriptRisk 基于 TypeScript 源码评估风险级别和推荐沙箱层级。
func assessScriptRisk(src []byte, source string) (riskLevel string, sandboxTier int) {
	s := string(src)
	// 混淆绕过检测（优先于朴素字符串匹配）
	if detectObfuscatedRisk(s) {
		return "high", 3
	}
	switch {
	case contains(s, "subprocess", "os.system", "os.popen"):
		return "high", 3 // L3 Container
	case contains(s, "requests.", "urllib", "http.", "socket"):
		return "medium", 3 // L3 Container（网络需最高隔离）
	case contains(s, "open(", "os.write"):
		return "medium", 3 // L3 Container
	case source == "builtin" || source == "user_verified":
		return "low", 1 // L1 InProcess
	default:
		return "medium", 3 // L3 Container (since it's Python, run in container)
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
		return "", apperr.New(apperr.CodeInternal, "signing key not configured")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(src)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

var getPIIRegex = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)(email|phone|password|token|secret|key|ssn)`)
})

func redactPIIFields(schema map[string]string) map[string]string {
	if schema == nil {
		return nil
	}
	res := make(map[string]string, len(schema))
	piiRegex := getPIIRegex()
	for k, v := range schema {
		if piiRegex.MatchString(k) && !isTypeScriptType(v) {
			res[k] = "<REDACTED>"
		} else {
			res[k] = v
		}
	}
	return res
}

func isTypeScriptType(s string) bool {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	switch lower {
	case "string", "number", "boolean", "any", "void", "object", "unknown", "never":
		return true
	}
	if strings.Contains(lower, "[]") || strings.HasPrefix(lower, "array") || strings.HasPrefix(lower, "record") || strings.HasPrefix(s, "{") {
		return true
	}
	return false
}

func runSandboxProbe(ctx context.Context, sb sandbox.SandboxProvider, src []byte, workDir string) error {
	if workDir == "" {
		return nil
	}
	if sb == nil {
		// 如果 L3 未启用，跳过沙箱测试，仅做静态检查
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	res, err := sb.Run(probeCtx, sandbox.SandboxSpec{
		ToolName:    "probe",
		ScriptBytes: src,
		Input:       []byte("{}"),
	})
	if err != nil {
		return ErrSandboxProbeFailed
	}
	if !res.Success {
		return ErrSandboxProbeFailed
	}
	return nil
}
