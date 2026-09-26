package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
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

// Install 独立技能安装：SKILL.md 经 pluginspec 按 agentskills.io + 两家扩展字段解析。
// 标准技能是指令包（正文 + scripts、references、assets 等资源目录），不要求入口脚本；存在 Polaris
// 脚本技能入口（src/index.ts 等）时额外做静态分析与风险分级。
func (s *SkillInstaller) Install(ctx context.Context, req InstallReq) (InstallResult, error) {
	installDir := req.LocalPath
	if installDir == "" {
		return InstallResult{}, apperr.New(apperr.CodeInvalidInput, "skill_installer: LocalPath required")
	}
	if s.skillReg == nil {
		return InstallResult{}, apperr.New(apperr.CodeInternal, "skill_installer: skill registry unavailable")
	}
	spec, diags := pluginspec.ParseSkillDir(installDir)
	if spec == nil {
		return InstallResult{}, apperr.New(apperr.CodeInvalidInput, "skill_installer: invalid SKILL.md: "+joinDiagnostics(diags))
	}
	for _, d := range diags {
		slog.Warn("skill_installer: diagnostic", "inst_id", req.InstID, "diag", d.String())
	}
	name := StandaloneSkillName(spec.Name)
	if err := s.ensureNameAvailable(ctx, req.InstID, name); err != nil {
		return InstallResult{}, err
	}
	meta := skillMetaFromSpec(spec, name, "", "", trustOrCommunity(req.TrustTier))
	if err := s.applyScriptEntry(installDir, req.InstID, &meta); err != nil {
		return InstallResult{}, err
	}
	if err := s.skillReg.Register(ctx, meta); err != nil {
		return InstallResult{}, apperr.Wrap(apperr.CodeInternal, "skill_installer.Install", err)
	}
	slog.Info("skill_installer: skill registered", "skill_name", name, "inst_id", req.InstID, "script", meta.ScriptPath)
	return InstallResult{Dir: installDir, RuntimeID: name}, nil
}

// ensureNameAvailable 同名技能已由另一安装实例持有时拒绝：静默覆盖会让先装的技能
// 被替换而其实例记录仍显示已安装。
func (s *SkillInstaller) ensureNameAvailable(ctx context.Context, instID, name string) error {
	insts, err := s.extRepo.ListInstances(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "skill_installer: list instances", err)
	}
	for _, inst := range insts {
		if inst.RuntimeID == name && inst.ID != instID {
			return apperr.New(apperr.CodeAlreadyExists, fmt.Sprintf("skill_installer: skill %q already installed by %s", name, inst.ID))
		}
	}
	return nil
}

// applyScriptEntry Polaris 脚本技能入口：静态分析 fail-closed，风险分级决定沙箱层级。
func (s *SkillInstaller) applyScriptEntry(installDir, instID string, meta *types.SkillMeta) error {
	scriptPath := ""
	for _, candidate := range []string{"src/index.ts", "index.ts", "src/index.js", "index.js"} {
		full := filepath.Join(installDir, candidate)
		if _, statErr := os.Stat(full); statErr == nil {
			scriptPath = full
			break
		}
	}
	if scriptPath == "" {
		return nil
	}
	meta.ScriptPath = scriptPath
	if s.analyzer == nil || s.riskAssessor == nil {
		slog.Warn("skill_installer: no validators injected, installing skill without content inspection (test-only default)",
			"inst_id", instID, "script", scriptPath)
		meta.RiskLevel, meta.Sandbox = "medium", 3
		return nil
	}
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "skill_installer: failed to read entry script for validation", err)
	}
	passed, violations, err := s.analyzer.Analyze(scriptBytes)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "skill_installer: static analysis failed", err)
	}
	if !passed {
		slog.Error("skill_installer: rejecting skill install, static analysis found forbidden patterns",
			"inst_id", instID, "script", scriptPath, "violations", violations)
		return apperr.New(apperr.CodeForbidden, fmt.Sprintf("skill_installer: static analysis rejected skill %q: %v", meta.Name, violations))
	}
	level, tier := s.riskAssessor.Assess(scriptBytes)
	meta.RiskLevel, meta.Sandbox = riskLevelLabel(level), tier
	return nil
}

func trustOrCommunity(tier int) types.TrustTier {
	if tier == 0 {
		return types.TrustCommunity
	}
	return types.TrustTier(tier)
}

func joinDiagnostics(ds []pluginspec.Diagnostic) string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		if d.Severity == pluginspec.SeverityError {
			parts = append(parts, d.Message)
		}
	}
	return strings.Join(parts, "; ")
}

func (s *SkillInstaller) Uninstall(ctx context.Context, req UninstallReq) error {
	if err := s.extRepo.UninstallCleanup(ctx, "", req.RuntimeID, string(types.TypeSkill)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "skill_installer.Uninstall", err)
	}
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
