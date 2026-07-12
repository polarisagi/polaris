package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ScriptStaticAnalyzer 消费方接口（防止 internal/extension/lifecycle 反向依赖
// internal/extension/skill——skill 包经 skill_creator.go 已导入
// internal/extension/marketplace，marketplace 又导入本包 lifecycle，若本包再
// 导入 skill 会形成 lifecycle→skill→marketplace→lifecycle 循环）。由
// skill.StaticAnalyzer 通过 cmd/polaris 组合根的适配器满足（2026-07-12
// unwired-code-audit 补齐：SkillInstaller 此前对脚本内容零检查）。
type ScriptStaticAnalyzer interface {
	// Analyze 返回是否通过、违规详情列表、执行期错误。
	Analyze(code []byte) (passed bool, violations []string, err error)
}

// ScriptRiskAssessor 消费方接口（同上，防循环依赖）。返回
// (riskLevel: 0=low/1=medium/2=high, sandboxTier: 1=InProc/3=Container)。
type ScriptRiskAssessor interface {
	Assess(code []byte) (riskLevel int, sandboxTier int)
}

type SkillInstaller struct {
	extRepo      protocol.ExtensionRepository
	skillReg     protocol.SkillRegistry
	analyzer     ScriptStaticAnalyzer
	riskAssessor ScriptRiskAssessor
}

func NewSkillInstaller(extRepo protocol.ExtensionRepository, skillReg protocol.SkillRegistry) *SkillInstaller {
	return &SkillInstaller{
		extRepo:  extRepo,
		skillReg: skillReg,
	}
}

// WithValidators 注入脚本静态分析器 + 风险分级器（可选；2026-07-12
// unwired-code-audit 补齐）。未注入时 Install 退化为改造前行为
// （RiskLevel="medium"/Sandbox=3 硬编码，不做内容检查）——仅用于测试等
// 无法满足接口的场景；生产环境启动装配必须注入，否则第三方技能脚本将
// 在零检查下被直接激活。
func (s *SkillInstaller) WithValidators(analyzer ScriptStaticAnalyzer, riskAssessor ScriptRiskAssessor) *SkillInstaller {
	s.analyzer = analyzer
	s.riskAssessor = riskAssessor
	return s
}

func (s *SkillInstaller) ExtType() types.ExtType { return types.TypeSkill }

func (s *SkillInstaller) Install(ctx context.Context, req InstallReq) (string, error) {
	installDir := req.LocalPath
	if installDir == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "skill_installer: LocalPath required")
	}

	if s.skillReg == nil {
		return installDir, nil
	}

	// 读取 SKILL.md
	var instructions string
	if raw, err := os.ReadFile(filepath.Join(installDir, "SKILL.md")); err == nil {
		instructions = string(raw)
	}

	// 脚本入口
	scriptPath := ""
	for _, candidate := range []string{"src/index.ts", "index.ts", "src/index.js", "index.js"} {
		full := filepath.Join(installDir, candidate)
		if _, statErr := os.Stat(full); statErr == nil {
			scriptPath = full
			break
		}
	}
	if scriptPath == "" {
		slog.Warn("skill_installer: skill has no entry script, skip",
			"inst_id", req.InstID, "install_dir", installDir)
		return installDir, nil
	}

	skillName := strings.TrimPrefix(req.InstID, "ext_")

	// Default values
	trustTier := types.TrustCommunity
	if req.TrustTier != 0 {
		trustTier = types.TrustTier(req.TrustTier)
	}

	// 默认值：s.analyzer/riskAssessor 未注入时（测试等场景）保持改造前行为。
	riskLabel := "medium"
	sandboxTier := 3

	if s.analyzer != nil && s.riskAssessor != nil {
		// 静态分析 + 风险分级（2026-07-12 unwired-code-audit 补齐）：此前本方法
		// 对脚本内容零检查，直接以硬编码 RiskLevel="medium"/Sandbox=3 入库——
		// 任何 Marketplace 安装的第三方技能脚本（无论实际内容是否包含 shell/
		// 网络/文件写入等高危操作）都被同等对待，违规内容（如
		// child_process.exec）从不被拦截。fail-closed：静态分析命中违规直接
		// 拒绝安装，不静默放行、不降级为警告。
		scriptBytes, readErr := os.ReadFile(scriptPath)
		if readErr != nil {
			return installDir, apperr.Wrap(apperr.CodeInternal, "skill_installer: failed to read entry script for validation", readErr)
		}
		passed, violations, analyzeErr := s.analyzer.Analyze(scriptBytes)
		if analyzeErr != nil {
			return installDir, apperr.Wrap(apperr.CodeInternal, "skill_installer: static analysis failed", analyzeErr)
		}
		if !passed {
			slog.Error("skill_installer: rejecting skill install, static analysis found forbidden patterns",
				"inst_id", req.InstID, "script", scriptPath, "violations", violations)
			return installDir, apperr.New(apperr.CodeForbidden,
				fmt.Sprintf("skill_installer: static analysis rejected skill %q: %v", skillName, violations))
		}
		riskLevelInt, tier := s.riskAssessor.Assess(scriptBytes)
		riskLabel = riskLevelLabel(riskLevelInt)
		sandboxTier = tier
	} else {
		slog.Warn("skill_installer: no validators injected, installing skill without content inspection (test-only default)",
			"inst_id", req.InstID, "script", scriptPath)
	}

	meta := types.SkillMeta{
		Name:         skillName,
		Version:      "1.0.0",
		Runtime:      "script",
		RiskLevel:    riskLabel,
		Sandbox:      sandboxTier,
		ExecMode:     "tool",
		Trust:        trustTier,
		ScriptPath:   scriptPath,
		Instructions: instructions,
	}

	if err := s.skillReg.Register(ctx, meta); err != nil {
		return installDir, apperr.Wrap(apperr.CodeInternal, "skill_installer.Install", err)
	}

	slog.Info("skill_installer: skill registered to SkillRegistry",
		"skill_name", skillName, "inst_id", req.InstID, "script", scriptPath)
	return installDir, nil
}

func (s *SkillInstaller) Uninstall(ctx context.Context, req UninstallReq) error {
	_ = s.extRepo.UninstallCleanup(ctx, "", req.RuntimeID, "skill")
	return nil
}

// riskLevelLabel 将 ScriptRiskAssessor.Assess 的数值风险等级映射为
// types.SkillMeta.RiskLevel 所需的字符串标签（0/1/2 = low/medium/high）。
func riskLevelLabel(level int) string {
	switch level {
	case 2:
		return "high"
	case 1:
		return "medium"
	default:
		return "low"
	}
}
