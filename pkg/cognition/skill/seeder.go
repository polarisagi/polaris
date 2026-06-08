package skill

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// SeedBuiltinSkills 在启动时将 skillsDir 下已编译的内置技能注册到 DB。
// 幂等：已存在的记录按 runtime_id ON CONFLICT 更新 install_path。
// skillsDir 必须是绝对路径；若目录不存在则静默跳过（技能未编译的合法状态）。
func SeedBuiltinSkills(ctx context.Context, db *sql.DB, skillsDir string) error {
	fi, err := os.Stat(skillsDir)
	if os.IsNotExist(err) {
		return nil // 技能目录未编译，属合法状态，静默跳过
	}
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "seeder: stat skills dir", err)
	}
	if !fi.IsDir() {
		return nil
	}

	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "seeder: read skills dir", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var errs []string

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(skillsDir, e.Name())
		if _, err := os.Stat(filepath.Join(skillDir, "impl.wasm")); err != nil {
			continue // impl.wasm 不存在（未编译），跳过
		}
		if err := seedOne(ctx, db, e.Name(), skillDir, now); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Name(), err))
		}
	}

	if len(errs) > 0 {
		return perrors.New(perrors.CodeInternal, "seeder: partial failure: "+strings.Join(errs, "; "))
	}
	return nil
}

type skillFrontmatter struct {
	Name       string   `yaml:"name"`
	Version    string   `yaml:"version"`
	RiskLevel  string   `yaml:"risk_level"`
	Sandbox    string   `yaml:"sandbox"`
	Capability string   `yaml:"capability"`
	Tags       []string `yaml:"tags"`
	ExecMode   string   `yaml:"exec_mode"`
}

func parseBuiltinSkillMD(skillDir string) skillFrontmatter {
	data, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return skillFrontmatter{}
	}
	content := string(data)
	lines := strings.Split(content, "\n")
	first, second := -1, -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "---" {
			if first == -1 {
				first = i
			} else {
				second = i
				break
			}
		}
	}
	if first < 0 || second <= first {
		return skillFrontmatter{}
	}
	var fm skillFrontmatter
	_ = yaml.Unmarshal([]byte(strings.Join(lines[first+1:second], "\n")), &fm)
	return fm
}

func sandboxLevel(s string) int {
	switch strings.ToUpper(s) {
	case "L2":
		return 2
	case "L3":
		return 3
	default:
		return 1 // L1 默认
	}
}

func seedOne(ctx context.Context, db *sql.DB, dirName, skillDir, now string) error {
	fm := parseBuiltinSkillMD(skillDir)

	skillName := fm.Name
	if skillName == "" {
		skillName = dirName
	}
	runtimeID := "skill:" + skillName

	version := fm.Version
	if version == "" {
		version = "1.0.0"
	}
	riskLevel := fm.RiskLevel
	if riskLevel == "" {
		riskLevel = "low"
	}
	execMode := fm.ExecMode
	if execMode == "" {
		execMode = "tool"
	}
	sandbox := sandboxLevel(fm.Sandbox)

	caps := []string{}
	if fm.Capability != "" {
		caps = []string{fm.Capability}
	}
	capsJSON, _ := json.Marshal(caps)

	// 1. UPSERT skills 表
	_, err := db.ExecContext(ctx, `
		INSERT INTO skills(name, version, runtime, risk_level, sandbox, capabilities,
		                   exec_mode, trust_tier, idempotent, benchmarks, instructions,
		                   deprecated, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,4,0,'{}','',0,?,?)
		ON CONFLICT(name) DO UPDATE SET
			version=excluded.version,
			risk_level=excluded.risk_level,
			sandbox=excluded.sandbox,
			capabilities=excluded.capabilities,
			exec_mode=excluded.exec_mode,
			trust_tier=4,
			updated_at=excluded.updated_at
	`, runtimeID, version, "wasm", riskLevel, sandbox, string(capsJSON), execMode, now, now)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "seeder: upsert skills", err)
	}

	// 2. UPSERT extension_instances 表（按 id 去重）
	extID := "ext_builtin_" + dirName
	_, err = db.ExecContext(ctx, `
		INSERT INTO extension_instances(id, ext_type, origin, catalog_id, name, publisher,
		                                trust_tier, runtime_id, install_path,
		                                status, created_at, updated_at)
		VALUES(?,?,?,?,?,?,4,?,?,'installed',?,?)
		ON CONFLICT(id) DO UPDATE SET
			install_path=excluded.install_path,
			runtime_id=excluded.runtime_id,
			updated_at=excluded.updated_at
	`, extID, "skill", "builtin", "", skillName, "polarisagi",
		runtimeID, skillDir, now, now)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "seeder: upsert extension_instances", err)
	}

	return nil
}
