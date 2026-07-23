// OpenClaw → Polaris 一次性迁移工具。
// 设计意图: OpenClaw 用户迁移到 polaris 时，通过 CLI 子命令一键导入配置、API 密钥、人设文件、技能和记忆，
// 避免用户手动搬运数据。记忆迁移采用 staging 隔离写入 + 渐进吸收策略，防止外部低质量记忆污染主线 EventLog。
// 架构文档: docs/arch/M13-Interface-Scheduler.md §1.1 "外部平台迁移"
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── OpenClaw 数据结构 ──────────────────────────────────────────────────────────────

type openclawConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Agents   *struct {
		Defaults *struct {
			Provider  string `json:"provider,omitempty"`
			Model     string `json:"model,omitempty"`
			Workspace string `json:"workspace,omitempty"`
		} `json:"defaults,omitempty"`
	} `json:"agents,omitempty"`
	Integrations *struct {
		Telegram *struct {
			Token string `json:"token,omitempty"`
		} `json:"telegram,omitempty"`
		OpenRouter *struct {
			Key string `json:"key,omitempty"`
		} `json:"openrouter,omitempty"`
		OpenAI *struct {
			Key string `json:"key,omitempty"`
		} `json:"openai,omitempty"`
		Anthropic *struct {
			Key string `json:"key,omitempty"`
		} `json:"anthropic,omitempty"`
		ElevenLabs *struct {
			Key string `json:"key,omitempty"`
		} `json:"elevenlabs,omitempty"`
	} `json:"integrations,omitempty"`
}

type migrateReport struct {
	ConfigKeys []string
	Persona    []string // SOUL.md, AGENTS.md, TOOLS.md
	Skills     []skillEntry
	MemoryDB   string // path if found
	Workspace  string
}

type skillEntry struct {
	Name string
	Path string
}

// ─── 入口 ──────────────────────────────────────────────────────────────────────────

func runMigrateOpenClaw(args []string) error { //nolint:gocyclo
	dryRun := true
	preset := "all"
	overwrite := false
	withMemory := false
	stage := true
	smart := false
	ocDir := ""
	clawhubURL := ""

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--dry-run=false":
			dryRun = false
		case args[i] == "--dry-run" || args[i] == "--dry":
			dryRun = true
		case strings.HasPrefix(args[i], "--preset="):
			preset = strings.TrimPrefix(args[i], "--preset=")
		case args[i] == "--overwrite":
			overwrite = true
		case args[i] == "--with-memory":
			withMemory = true
		case strings.HasPrefix(args[i], "--openclaw-dir="):
			ocDir = strings.TrimPrefix(args[i], "--openclaw-dir=")
		case strings.HasPrefix(args[i], "--clawhub-url="):
			clawhubURL = strings.TrimPrefix(args[i], "--clawhub-url=")
		case args[i] == "--help" || args[i] == "-h":
			printMigrateUsage()
			return nil
		}
	}

	if ocDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "无法检测用户主目录", err)
		}
		ocDir = filepath.Join(home, ".openclaw")
	}

	info, err := os.Stat(ocDir)
	if err != nil || !info.IsDir() {
		return apperr.New(apperr.CodeNotFound,
			"OpenClaw 目录未找到: "+ocDir+" (使用 --openclaw-dir 指定)")
	}

	report, err := scanOpenClaw(ocDir)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "扫描 OpenClaw 目录失败", err)
	}

	if dryRun {
		printMigrateDryRun(report, clawhubURL, withMemory, stage, smart)
		return nil
	}

	if err := applyMigration(report, preset, overwrite); err != nil {
		return err
	}
	if withMemory && report.MemoryDB != "" {
		return migrateMemory(report.MemoryDB, stage, smart)
	}
	return nil
}

func printMigrateUsage() {
	fmt.Println(`用法: polaris migrate openclaw [选项]

从 OpenClaw (~/.openclaw) 一次性迁移配置、记忆、技能到 polaris。

选项:
  --dry-run            预览迁移内容（默认 true）
  --dry-run=false      执行实际写入
  --preset=all         迁移范围: all | user-data | skills
  --overwrite          覆盖已有 polaris 配置（默认不覆盖）
  --openclaw-dir=<path>指定 OpenClaw 数据目录（默认 ~/.openclaw）
  --with-memory         启用记忆迁移（默认 staging 隔离写入）
  --stage=false         记忆不入 staging, 直接写主线 events
  --smart               LLM 启发式预压缩（去重+摘要, 减少低价值记忿）
  --clawhub-url=<url>  从 ClawHub 拉取技能（可选）
  --help              显示此帮助

示例:
  polaris migrate openclaw --dry-run
  polaris migrate openclaw --dry-run=false --preset=user-data
  polaris migrate openclaw --dry-run=false --with-memory
  polaris migrate openclaw --dry-run=false --with-memory --smart --stage=false
  polaris migrate openclaw --dry-run=false --preset=skills --clawhub-url=https://clawhub.ai`)
}

// ─── 扫描 ──────────────────────────────────────────────────────────────────────────

func scanOpenClaw(ocDir string) (*migrateReport, error) {
	rep := &migrateReport{}

	cfgPath := filepath.Join(ocDir, "openclaw.json")
	cfgData, err := os.ReadFile(cfgPath)
	if err == nil {
		var cfg openclawConfig
		if err := json.Unmarshal(cfgData, &cfg); err == nil {
			rep.ConfigKeys = extractKeys(&cfg)
		}
	}

	wsDir := filepath.Join(ocDir, "workspace")
	wsInfo, err := os.Stat(wsDir)
	if err == nil && wsInfo.IsDir() { //nolint:nestif
		rep.Workspace = wsDir

		for _, name := range []string{"SOUL.md", "AGENTS.md", "TOOLS.md"} {
			fp := filepath.Join(wsDir, name)
			if _, err := os.Stat(fp); err == nil {
				rep.Persona = append(rep.Persona, fp)
			}
		}

		skillsDir := filepath.Join(wsDir, "skills")
		if entries, err := os.ReadDir(skillsDir); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					skillMD := filepath.Join(skillsDir, e.Name(), "SKILL.md")
					if _, err := os.Stat(skillMD); err == nil {
						rep.Skills = append(rep.Skills, skillEntry{
							Name: e.Name(),
							Path: skillMD,
						})
					}
				}
			}
		}

		memDir := filepath.Join(wsDir, "memories")
		if memEntries, err := os.ReadDir(memDir); err == nil {
			for _, e := range memEntries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".db") {
					rep.MemoryDB = filepath.Join(memDir, e.Name())
					break
				}
			}
		}
	}

	return rep, nil
}

func extractKeys(cfg *openclawConfig) []string {
	var keys []string
	add := func(name, val string) {
		if val != "" {
			keys = append(keys, fmt.Sprintf("%s=%s", name, val))
		}
	}
	if cfg.Provider != "" {
		keys = append(keys, fmt.Sprintf("provider=%s", cfg.Provider))
	}
	if cfg.Model != "" {
		keys = append(keys, fmt.Sprintf("model=%s", cfg.Model))
	}
	if cfg.Agents != nil && cfg.Agents.Defaults != nil {
		if cfg.Agents.Defaults.Provider != "" {
			add("default_provider", cfg.Agents.Defaults.Provider)
		}
		if cfg.Agents.Defaults.Model != "" {
			add("default_model", cfg.Agents.Defaults.Model)
		}
	}
	if cfg.Integrations != nil {
		add("telegram_token", cfg.Integrations.Telegram.Token)
		add("openrouter_key", cfg.Integrations.OpenRouter.Key)
		add("openai_key", cfg.Integrations.OpenAI.Key)
		add("anthropic_key", cfg.Integrations.Anthropic.Key)
		add("elevenlabs_key", cfg.Integrations.ElevenLabs.Key)
	}
	return keys
}

// ─── Dry-Run 输出 ──────────────────────────────────────────────────────────────────

func printMigrateDryRun(rep *migrateReport, clawhubURL string, withMemory bool, stage bool, smart bool) {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  OpenClaw → Polaris 迁移预览 (DRY RUN)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if len(rep.ConfigKeys) > 0 {
		fmt.Printf("\n📋 配置 & API 密钥 (%d 项):\n", len(rep.ConfigKeys))
		for _, k := range rep.ConfigKeys {
			val := k
			if idx := strings.IndexByte(k, '='); idx > 0 {
				keyPart := k[:idx]
				if strings.HasSuffix(keyPart, "_key") || strings.HasSuffix(keyPart, "_token") {
					val = keyPart + "=****"
				}
			}
			fmt.Printf("  • %s\n", val)
		}
	} else {
		fmt.Println("\n📋 配置: 未发现")
	}

	if len(rep.Persona) > 0 {
		fmt.Printf("\n🧑 人设文件 (%d 个):\n", len(rep.Persona))
		for _, p := range rep.Persona {
			fmt.Printf("  • %s\n", filepath.Base(p))
		}
	}

	if len(rep.Skills) > 0 {
		fmt.Printf("\n🔧 技能 (%d 个):\n", len(rep.Skills))
		for _, s := range rep.Skills {
			fmt.Printf("  • %s\n", s.Name)
		}
	}

	if rep.MemoryDB != "" {
		fmt.Printf("\n💾 记忆数据库: %s\n", filepath.Base(rep.MemoryDB))
		fmt.Println("  ⚠  Schema 非直接兼容, 需按 EventLog 格式重放")
	}

	if clawhubURL != "" {
		fmt.Printf("\n🌐 ClawHub: %s\n", clawhubURL)
		fmt.Println("  将尝试拉取并导入可用技能")
	}

	if rep.Workspace == "" {
		fmt.Println("\n⚠  未发现 workspace 目录 — 部分数据可能缺失")
	}

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("运行 `polaris migrate openclaw --dry-run=false` 执行迁移")
}

// ─── 写入 ──────────────────────────────────────────────────────────────────────────

func applyMigration(rep *migrateReport, preset string, overwrite bool) error {
	polarisDir := resolvePolarisDir()

	switch preset {
	case "user-data":
		return applyUserData(rep, polarisDir, overwrite)
	case "skills":
		return applySkills(rep, polarisDir, overwrite)
	case "all":
		if err := applyUserData(rep, polarisDir, overwrite); err != nil {
			return err
		}
		return applySkills(rep, polarisDir, overwrite)
	default:
		return apperr.New(apperr.CodeInvalidInput,
			"未知 preset: "+preset+" (可选: all, user-data, skills)")
	}
}

func resolvePolarisDir() string {
	dir := os.Getenv("POLARIS_DATA_DIR")
	if dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".polarisagi/polaris")
}

func applyUserData(rep *migrateReport, polarisDir string, overwrite bool) error { //nolint:gocyclo
	if err := os.MkdirAll(polarisDir, 0o700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "创建 polaris 数据目录失败", err)
	}

	if len(rep.ConfigKeys) > 0 { //nolint:nestif
		envPath := filepath.Join(polarisDir, ".env")
		exists := false
		if _, err := os.Stat(envPath); err == nil {
			exists = true
			if !overwrite {
				fmt.Printf("SKIP  %s already exists (use --overwrite to replace)\n", envPath)
				goto persona
			}
		}

		f, err := os.Create(envPath)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "创建配置文件失败: "+envPath, err)
		}
		defer f.Close()

		for _, k := range rep.ConfigKeys {
			if idx := strings.IndexByte(k, '='); idx > 0 {
				key := k[:idx]
				val := k[idx+1:]
				switch key {
				case "openai_key":
					fmt.Fprintf(f, "OPENAI_API_KEY=%s\n", val)
				case "anthropic_key":
					fmt.Fprintf(f, "ANTHROPIC_API_KEY=%s\n", val)
				case "default_provider":
					fmt.Fprintf(f, "POLARIS_PROVIDER=%s\n", val)
				case "default_model":
					fmt.Fprintf(f, "POLARIS_MODEL=%s\n", val)
				case "telegram_token":
					fmt.Fprintf(f, "TELEGRAM_BOT_TOKEN=%s\n", val)
				case "openrouter_key":
					fmt.Fprintf(f, "OPENROUTER_API_KEY=%s\n", val)
				case "elevenlabs_key":
					fmt.Fprintf(f, "ELEVENLABS_API_KEY=%s\n", val)
				}
			}
		}
		action := "CREATED"
		if exists {
			action = "OVERWRITTEN"
		}
		fmt.Printf("OK  %s %s (%d config keys)\n", action, envPath, len(rep.ConfigKeys))
	}

persona:
	for _, src := range rep.Persona {
		dst := filepath.Join(polarisDir, filepath.Base(src))
		exists := false
		if _, err := os.Stat(dst); err == nil {
			exists = true
			if !overwrite {
				fmt.Printf("SKIP  persona %s (use --overwrite)\n", filepath.Base(src))
				continue
			}
		}
		data, err := os.ReadFile(src)
		if err != nil {
			fmt.Printf("WARN  read %s: %v\n", src, err)
			continue
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "写入人设文件失败: "+dst, err)
		}
		action := "CREATED"
		if exists {
			action = "OVERWRITTEN"
		}
		fmt.Printf("OK  %s %s\n", action, dst)
	}

	return nil
}

func applySkills(rep *migrateReport, polarisDir string, overwrite bool) error {
	if len(rep.Skills) == 0 {
		fmt.Println("SKIP  no skills to migrate")
		return nil
	}

	skillDir := filepath.Join(polarisDir, "workspace", "skills")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "创建技能目录失败", err)
	}

	for _, s := range rep.Skills {
		target := filepath.Join(skillDir, s.Name)
		if err := os.MkdirAll(target, 0o700); err != nil {
			fmt.Printf("WARN  mkdir %s: %v\n", target, err)
			continue
		}

		dst := filepath.Join(target, "SKILL.md")
		exists := false
		if _, err := os.Stat(dst); err == nil {
			exists = true
			if !overwrite {
				fmt.Printf("SKIP  skill %s (use --overwrite)\n", s.Name)
				continue
			}
		}

		data, err := os.ReadFile(s.Path)
		if err != nil {
			fmt.Printf("WARN  read %s: %v\n", s.Path, err)
			continue
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "写入技能文件失败: "+dst, err)
		}

		action := "CREATED"
		if exists {
			action = "OVERWRITTEN"
		}
		fmt.Printf("OK  %s %s\n", action, dst)
	}

	return nil
}
