package skill

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ScriptExecutor consumer-side 接口（在 cognition 包内定义）
type ScriptExecutor interface {
	ExecuteTest(ctx context.Context, scriptBytes []byte, input []byte) ([]byte, error)
}

// 技能验证管线 + 演化引擎。
// 架构文档: docs/arch/M06-Skill-Library.md §2.3, §4

// SkillValidationPipeline LLM 生成技能的四步验证。
// Step 0: Taint-Check → Step 1: 静态分析 → Step 2: 行为测试 → Step 3: 风险分级 → Step 4: 签名入库.
type SkillValidationPipeline struct {
	taintChecker   *TaintChecker
	staticAnalyzer *StaticAnalyzer
	scriptTester   *ScriptTester
	riskAssessor   *RiskAssessor
	signer         *Signer
}

// NewSkillValidationPipeline 创建完整验证管线。
func NewSkillValidationPipeline(signingKey []byte, executor ScriptExecutor) *SkillValidationPipeline {
	return &SkillValidationPipeline{
		taintChecker:   &TaintChecker{},
		staticAnalyzer: &StaticAnalyzer{},
		scriptTester:   &ScriptTester{runtime: executor},
		riskAssessor:   &RiskAssessor{},
		signer:         &Signer{privateKey: signingKey},
	}
}

// Validate 执行完整四步验证。返回最终风险分级和签名，任一步骤失败立即终止。
func (p *SkillValidationPipeline) Validate(code []byte, taintLevel int) (*ValidateResult, error) {
	p.scriptTester.scriptBytes = code

	// Step 0: Taint 检查
	if err := p.taintChecker.Check(taintLevel); err != nil {
		return nil, fmt.Errorf("SkillValidationPipeline.Validate: %w", err)
	}

	// Step 1: 静态分析
	ar, err := p.staticAnalyzer.Analyze(code)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "静态分析失败", err)
	}
	if !ar.Passed {
		return nil, &SkillPipelineError{fmt.Sprintf("static analysis failed: %v", ar.Violations)}
	}

	// Step 2: 脚本行为测试
	if err := p.scriptTester.Run(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Wasm 行为测试失败", err)
	}

	// Step 3: 风险分级
	riskLevel, sandboxTier := p.riskAssessor.Assess(code)

	// Step 4: 签名
	sig, err := p.signer.Sign(code)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "签名生成失败", err)
	}

	return &ValidateResult{
		Passed:      true,
		RiskLevel:   riskLevel,
		SandboxTier: sandboxTier,
		Signature:   sig,
	}, nil
}

type ValidateResult struct {
	Passed      bool
	RiskLevel   int
	SandboxTier int
	Signature   string
}

// TaintChecker Step 0 — 污点检查。
// 放行: TaintLow (用户输入) 或 TaintNone (系统编译期) → 编译
// 拒绝: TaintMedium+ 轨迹严禁编译 → 进入 MEMF, 标记 tainted_trajectory
// 原则: 污点永不静默消除。编译产物保持输入 TaintLevel 感知并传播到输出.
type TaintChecker struct{}

// Check 检查轨迹 Taint 合法性。
func (tc *TaintChecker) Check(taintLevel int) error {
	if taintLevel >= 2 { // TaintMedium+
		return ErrPipelineTaintedTrajectory
	}
	return nil
}

// StaticAnalyzer Step 1 — AST 系统调用审计。
// 禁止: import "os/exec", "net/http" (RiskLevel=high 除外), unsafe 包, CGO.
// 函数签名必须匹配 schema.json 定义.
type StaticAnalyzer struct{}

// Analyze 静态分析 impl.go。
// 基于文本模式匹配扫描禁止的导入和包引用（MVP 简化版，Tier 1+ 升级为 go/ast 完整分析）。
func (sa *StaticAnalyzer) Analyze(code []byte) (*AnalyzeResult, error) {
	result := &AnalyzeResult{Passed: true}
	codeStr := string(code)

	// 禁止的导入模式
	forbiddenImports := []string{
		`"os/exec"`,
		`"net/http"`,
		`"unsafe"`,
		`"C"`, // CGO
		`"syscall"`,
	}
	for _, fi := range forbiddenImports {
		if strings.Contains(codeStr, fi) {
			result.Violations = append(result.Violations, fmt.Sprintf("禁止导入: %s", fi))
		}
	}

	// 禁止的包调用模式（即使用别名导入也检测）
	forbiddenCalls := []string{
		"exec.Command",
		"http.Get",
		"http.Post",
		"unsafe.Pointer",
	}
	for _, fc := range forbiddenCalls {
		if strings.Contains(codeStr, fc) {
			result.Violations = append(result.Violations, fmt.Sprintf("禁止调用: %s", fc))
		}
	}

	if len(result.Violations) > 0 {
		result.Passed = false
	}

	// 风险分级: 有 violation → high; 无 violation → low
	if result.Passed {
		result.RiskLevel = 0 // low
	} else {
		result.RiskLevel = 2 // high
	}

	return result, nil
}

type AnalyzeResult struct {
	Passed     bool
	Violations []string
	RiskLevel  int
}

// ScriptTester Step 2 — 沙箱行为测试（TypeScript/Python 脚本）。
// 对每个测试用例在受控环境中执行脚本并对比输出。
// 全部通过 → Step 3; 失败 → 打回 LLM 修复（最多 3 轮）。
type ScriptTester struct {
	testCases   []TestCase
	scriptBytes []byte
	runtime     ScriptExecutor
}

// TestCase 测试用例。
type TestCase struct {
	Name   string
	Input  []byte
	Expect []byte
}

// AddTestCase 添加测试用例。
func (wt *ScriptTester) AddTestCase(name string, input, expect []byte) {
	wt.testCases = append(wt.testCases, TestCase{Name: name, Input: input, Expect: expect})
}

// Run 执行所有测试用例。
// MVP: 通过 ContainerSandbox 执行 TypeScript/Python 脚本，对比输出。
// 约束: 每个测试用例独立沙箱实例，禁止跨用例状态泄漏。
func (wt *ScriptTester) Run() error {
	if len(wt.testCases) == 0 {
		// 无测试用例 → 跳过（生产环境应至少有 schema.json 定义的输入/输出对）
		return nil
	}

	if wt.runtime == nil {
		return apperr.New(apperr.CodeInternal, "ScriptExecutor not injected")
	}

	for _, tc := range wt.testCases {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := wt.runtime.ExecuteTest(ctx, wt.scriptBytes, tc.Input)
		cancel()
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "脚本行为测试执行失败", err)
		}

		if !bytes.Equal(res, tc.Expect) {
			actualSummary := res
			if len(actualSummary) > 32 {
				actualSummary = actualSummary[:32]
			}
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("测试用例 %s 输出不匹配: 期望 %x, 实际 %x...", tc.Name, tc.Expect, actualSummary))
		}
	}
	return nil
}

// RiskAssessor Step 3 — 风险分级。
// 文件写入/网络请求 → RiskLevel=high → Container(L3); 纯计算 → RiskLevel=low → InProcess(L1).
type RiskAssessor struct{}

// Assess 根据代码内容评估风险级别和推荐沙箱层级。
// 返回 (riskLevel: 0=low, 1=medium, 2=high; sandboxTier: 1=InProc, 3=Container).
func (ra *RiskAssessor) Assess(code []byte) (riskLevel int, sandboxTier int) {
	codeStr := string(code)

	// 检测文件系统写入操作 → medium
	hasFSWrite := strings.Contains(codeStr, "WriteFile") ||
		strings.Contains(codeStr, "os.Create") ||
		strings.Contains(codeStr, "os.OpenFile") ||
		strings.Contains(codeStr, "ioutil.WriteFile")

	// 检测网络请求 → high
	hasNetwork := strings.Contains(codeStr, "http.") ||
		strings.Contains(codeStr, "net.Dial") ||
		strings.Contains(codeStr, "grpc.Dial")

	// 检测 shell 执行 → high (需最高隔离)
	hasShell := strings.Contains(codeStr, "exec.Command") ||
		strings.Contains(codeStr, "os/exec")

	// 风险级别判定
	switch {
	case hasShell:
		riskLevel = 2   // high
		sandboxTier = 3 // L3 Container
	case hasNetwork:
		riskLevel = 2   // high
		sandboxTier = 3 // L3 Container
	case hasFSWrite:
		riskLevel = 1   // medium
		sandboxTier = 3 // L3 Container
	default:
		riskLevel = 0   // low — 纯计算/转换
		sandboxTier = 1 // L1 InProc
	}

	return riskLevel, sandboxTier
}

// Signer Step 4 — 签名 + 入库。
// cosign sign → SIGNATURE 文件 → 写入 Skill Library.
// 签名私钥不对远程编译器暴露.
type Signer struct {
	privateKey []byte
}

// Sign 使用 HMAC-SHA256 对代码内容生成签名。
// 签名绑定代码哈希，防止篡改。
func (s *Signer) Sign(code []byte) (string, error) {
	if len(s.privateKey) == 0 {
		return "", apperr.New(apperr.CodeInternal, "签名私钥未配置——禁止对未签名的技能放行")
	}
	mac := hmac.New(sha256.New, s.privateKey)
	mac.Write(code)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Verify 验证签名是否匹配代码内容。
func (s *Signer) Verify(code []byte, signature string) bool {
	expected, err := s.Sign(code)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(expected), []byte(signature))
}

// ============================================================================
// SkillEvolutionEngine — 递归演化 + 四级废弃
// 架构文档: docs/arch/06-Skill-Library-深度选型.md §4

type SkillEvolutionEngine struct {
	skills           map[string]*Skill
	successHistories map[string][]bool
	failureReasons   map[string][]string
}

// EvaluateAndEvolve 评估并演化。
// 步骤1: UncontrollableFailure (网络不可达/API配额/OS kill) → 跳过
// 步骤2: 追加 result.Success 到 SuccessHistory
// 步骤3: 连续失败 >= 3 → 按策略分发:
//
//	UpdateValidate → Revalidate 重新测试; UpdateReflect → LLM反思改进; UpdateDiscard → deprecated
//
// 步骤4: SuccessHistory 保留最近 20 条
// 步骤5: 连续 UncontrollableFailure > 100 → 冻结废弃评估, 每60s探测
func (e *SkillEvolutionEngine) EvaluateAndEvolve(skillID string, success bool, reason string) {
	history := e.successHistories[skillID]
	history = append(history, success)
	if len(history) > 20 {
		history = history[len(history)-20:]
	}
	e.successHistories[skillID] = history

	// 连续 3 次 ControllableFailure → 触发演化
	if consecutiveFailures(history) >= 3 {
		e.triggerEvolution(skillID)
	}
}

func consecutiveFailures(history []bool) int {
	count := 0
	for i := len(history) - 1; i >= 0; i-- {
		if !history[i] {
			count++
		} else {
			break
		}
	}
	return count
}

// triggerEvolution 根据技能状态分发演化策略。
// - 成功率 < 30% 且使用次数 > 10 → DeprecationDynamic（移出主索引，保留手动恢复）
// - 连续失败但成功率尚可 → 标记待 LLM 反思改进
// - 安全漏洞/签名失效 → DeprecationHard（物理删除脚本 + 撤销签名）
func (e *SkillEvolutionEngine) triggerEvolution(skillID string) {
	sk, ok := e.skills[skillID]
	if !ok {
		return
	}

	history := e.successHistories[skillID]
	if len(history) == 0 {
		return
	}

	// 计算近期成功率
	recent := history
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}
	successes := 0
	for _, s := range recent {
		if s {
			successes++
		}
	}
	successRate := float64(successes) / float64(len(recent))

	// 四级废弃判定
	switch {
	case successRate < 0.3 && sk.UseCount > 10:
		// 动态废弃: 移出主索引，保留手动恢复路径
		sk.Deprecated = true
		sk.DeprecationLevel = int(DeprecationDynamic)
	case len(consecutiveFailureReasons(e.failureReasons[skillID])) >= 5:
		// 连续 5 次不可控失败 → 暂停使用，每 60s 探测
		sk.Deprecated = true
		sk.DeprecationLevel = int(DeprecationFiltered)
	default:
		// 标记待 LLM 反思改进（下次 Revalidate 触发）
		sk.NeedsRevalidate = true
	}
}

func consecutiveFailureReasons(reasons []string) []string {
	var result []string
	for i := len(reasons) - 1; i >= 0; i-- {
		if reasons[i] != "" {
			result = append(result, reasons[i])
		} else {
			break
		}
	}
	return result
}

// SkillDeprecationLevel 四级废弃。
type SkillDeprecationLevel int

const (
	DeprecationNormal   SkillDeprecationLevel = iota // LLM 生成更好版本 → version++
	DeprecationFiltered                              // 连续 3 次测试失败 → deprecated=true
	DeprecationDynamic                               // 成功率 < 30% 且使用 > 10 → 移出主索引
	DeprecationHard                                  // 安全漏洞/签名失效 → 物理删除脚本 + 撤销签名
)

var (
	ErrPipelineTaintedTrajectory = &SkillPipelineError{"tainted trajectory rejected"}
	ErrSkillCompileFailed        = &SkillPipelineError{"skill compilation failed"}
)

type SkillPipelineError struct{ msg string }

func (e *SkillPipelineError) Error() string { return e.msg }
