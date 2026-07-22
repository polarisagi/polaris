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

// SkillValidationOption defines initialization options for the pipeline.
type SkillValidationOption func(*SkillValidationPipeline)

// WithMaxCodeSize injects a maximum code size limit for Logic-Collapse generated scripts.
func WithMaxCodeSize(bytes int) SkillValidationOption {
	return func(p *SkillValidationPipeline) {
		p.maxCodeSize = bytes
	}
}

// SkillValidationPipeline LLM 生成技能的四步验证。
// Step 0: Taint-Check → Step 1: 静态分析 → Step 2: 行为测试 → Step 3: 风险分级 → Step 4: 签名入库.
type SkillValidationPipeline struct {
	taintChecker   *TaintChecker
	staticAnalyzer *StaticAnalyzer
	scriptTester   *ScriptTester
	riskAssessor   *RiskAssessor
	signer         *Signer
	maxCodeSize    int
}

// NewSkillValidationPipeline 创建完整验证管线。
func NewSkillValidationPipeline(signingKey []byte, executor ScriptExecutor, opts ...SkillValidationOption) *SkillValidationPipeline {
	p := &SkillValidationPipeline{
		taintChecker:   &TaintChecker{},
		staticAnalyzer: &StaticAnalyzer{},
		scriptTester:   &ScriptTester{runtime: executor},
		riskAssessor:   &RiskAssessor{},
		signer:         &Signer{privateKey: signingKey},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Validate 执行完整四步验证。返回最终风险分级和签名，任一步骤失败立即终止。
func (p *SkillValidationPipeline) Validate(code []byte, taintLevel int) (*ValidateResult, error) {
	if p.maxCodeSize > 0 && len(code) > p.maxCodeSize {
		return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("skill_pipeline: code size %d bytes exceeds maximum limit of %d bytes", len(code), p.maxCodeSize))
	}
	p.scriptTester.scriptBytes = code

	// Step 0: Taint 检查
	if err := p.taintChecker.Check(taintLevel); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SkillValidationPipeline.Validate", err)
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
//
// 模式覆盖 Go/Python/JS-TS 三类当前实际产生技能脚本的语言（2026-07-12
// unwired-code-audit 补齐 SkillInstaller 接线时发现：本管线最初只有 Go 语法
// 模式，但 LogicCollapse 生成 Python、Marketplace 安装的社区技能是 TS/JS，
// 原始模式集对这两类实际目标语言形同虚设——纯字符串子串匹配代价低，一次性
// 补齐三语言不引入额外依赖或性能负担）。
func (sa *StaticAnalyzer) Analyze(code []byte) (*AnalyzeResult, error) {
	result := &AnalyzeResult{Passed: true}
	codeStr := string(code)

	// 禁止的导入模式
	forbiddenImports := []string{
		// Go
		`"os/exec"`,
		`"net/http"`,
		`"unsafe"`,
		`"C"`, // CGO
		`"syscall"`,
		// Python
		`import os`,
		`import subprocess`,
		`import socket`,
		`import ctypes`,
		`from os import`,
		`from subprocess import`,
		// Node.js / TypeScript
		`require('child_process')`,
		`require("child_process")`,
		`from 'child_process'`,
		`from "child_process"`,
		`require('fs')`,
		`require("fs")`,
		`from 'fs'`,
		`from "fs"`,
	}
	for _, fi := range forbiddenImports {
		if strings.Contains(codeStr, fi) {
			result.Violations = append(result.Violations, fmt.Sprintf("禁止导入: %s", fi))
		}
	}

	// 禁止的包调用模式（即使用别名导入也检测）
	forbiddenCalls := []string{
		// Go
		"exec.Command",
		"http.Get",
		"http.Post",
		"unsafe.Pointer",
		// Python
		"subprocess.run",
		"subprocess.Popen",
		"os.system",
		"os.popen",
		"eval(",
		"exec(",
		// Node.js / TypeScript
		"child_process.exec",
		"child_process.spawn",
		"process.binding",
		"new Function(",
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
// 模式覆盖 Go/Python/JS-TS（理由同 StaticAnalyzer.Analyze 头部注释）。
func (ra *RiskAssessor) Assess(code []byte) (riskLevel int, sandboxTier int) {
	codeStr := string(code)

	// 检测文件系统写入操作 → medium
	hasFSWrite := strings.Contains(codeStr, "WriteFile") ||
		strings.Contains(codeStr, "os.Create") ||
		strings.Contains(codeStr, "os.OpenFile") ||
		strings.Contains(codeStr, "ioutil.WriteFile") ||
		strings.Contains(codeStr, "fs.writeFile") ||
		strings.Contains(codeStr, "fs.writeFileSync") ||
		strings.Contains(codeStr, "open(") // Python open() 读写两用，宁可偏保守判定

	// 检测网络请求 → high
	hasNetwork := strings.Contains(codeStr, "http.") ||
		strings.Contains(codeStr, "net.Dial") ||
		strings.Contains(codeStr, "grpc.Dial") ||
		strings.Contains(codeStr, "fetch(") ||
		strings.Contains(codeStr, "XMLHttpRequest") ||
		strings.Contains(codeStr, "requests.get") ||
		strings.Contains(codeStr, "requests.post") ||
		strings.Contains(codeStr, "urllib.request")

	// 检测 shell 执行 → high (需最高隔离)
	hasShell := strings.Contains(codeStr, "exec.Command") ||
		strings.Contains(codeStr, "os/exec") ||
		strings.Contains(codeStr, "subprocess.") ||
		strings.Contains(codeStr, "os.system") ||
		strings.Contains(codeStr, "child_process")

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

var (
	ErrPipelineTaintedTrajectory = &SkillPipelineError{"tainted trajectory rejected"}
	ErrSkillCompileFailed        = &SkillPipelineError{"skill compilation failed"}
)

type SkillPipelineError struct{ msg string }

func (e *SkillPipelineError) Error() string { return e.msg }
