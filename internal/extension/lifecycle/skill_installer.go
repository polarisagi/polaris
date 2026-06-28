package lifecycle

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type SkillInstaller struct {
	extRepo  protocol.ExtensionRepository
	skillReg protocol.SkillRegistry
}

func NewSkillInstaller(extRepo protocol.ExtensionRepository, skillReg protocol.SkillRegistry) *SkillInstaller {
	return &SkillInstaller{
		extRepo:  extRepo,
		skillReg: skillReg,
	}
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

	meta := types.SkillMeta{
		Name:         skillName,
		Version:      "1.0.0",
		Runtime:      "script",
		RiskLevel:    "medium",
		Sandbox:      3,
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
