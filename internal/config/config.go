package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/pkg/apperr"
)

var BuildVersion = "dev"

type Config struct {
	System        SystemConfig        `toml:"system"`
	Download      DownloadConfig      `toml:"download"`
	Inference     InferenceConfig     `toml:"inference"`
	Embedding     EmbeddingConfig     `toml:"embedding"`
	Cognition     CognitionConfig     `toml:"cognition"`
	Storage       StorageConfig       `toml:"storage"`
	Observability ObservabilityConfig `toml:"observability"`
	Agent         AgentConfig         `toml:"agent"`
	Orchestrator  OrchestratorConfig  `toml:"orchestrator"`
	SelfImprove   SelfImproveConfig   `toml:"self_improve"`
	Knowledge     KnowledgeConfig     `toml:"knowledge"`
	Policy        PolicyConfig        `toml:"policy"`
	Eval          EvalConfig          `toml:"eval"`
	Interface     InterfaceConfig     `toml:"interface"`
	Compressor    CompressorConfig    `toml:"compressor"`
	Sandbox       SandboxConfig       `toml:"sandbox"`
	Security      SecurityConfig      `toml:"security"`
	Thresholds    Thresholds          `toml:"-"`
}

// 各子模块配置结构体定义（CognitionConfig...CompressorConfig）见 config_types.go（R7 拆分）。

func loadModuleTOML(modulePath string, target interface{}) error {
	if _, err := os.Stat(modulePath); os.IsNotExist(err) {
		return nil
	}
	data, err := os.ReadFile(modulePath)
	if err != nil {
		slog.Error("polaris: failed to read threshold override", "file", modulePath, "err", err)
		return apperr.Wrap(apperr.CodeInternal, "loadModuleTOML", err)
	}
	if err := toml.Unmarshal(data, target); err != nil {
		slog.Error("polaris: failed to parse threshold override", "file", modulePath, "err", err)
		return apperr.Wrap(apperr.CodeInternal, "loadModuleTOML", err)
	}
	slog.Info("polaris: threshold override loaded", "file", modulePath)
	return nil
}

func Load(path string) (*Config, error) {
	// 1. 先以 defaults.toml 作为基底（保证所有字段有默认值）
	defaultsData, err := configs.FS.ReadFile("defaults.toml")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Load: read embedded defaults", err)
	}
	cfg := &Config{Thresholds: DefaultThresholds()}
	if err := toml.Unmarshal(defaultsData, cfg); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Load: parse embedded defaults", err)
	}

	// 2. 若用户 config.toml 存在，叠加覆盖（仅写入的字段生效，其余保留 defaults）
	userData, readErr := os.ReadFile(path)
	if readErr == nil {
		if err := toml.Unmarshal(userData, cfg); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("Load: parse %s", path), err)
		}
	} else {
		// 用户配置不存在，导出 defaults 供后续手动编辑（幂等，失败忽略）
		if errMkdir := os.MkdirAll(filepath.Dir(path), 0755); errMkdir == nil {
			os.WriteFile(path, defaultsData, 0600) //nolint:errcheck
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Load", err)
	}
	return cfg, nil
}

// Validate 对边界非法值做 Fail-Fast 校验，防止明显错误配置在运行期才暴露 panic。
// 未填写的字段（零值）代表"使用系统默认"，不视为错误。
func (c *Config) Validate() error {
	if c.Storage.Tier0VectorScanLimit <= 0 {
		c.Storage.Tier0VectorScanLimit = 500
	}
	// 若 TTS 未指定 sherpa_version，则自动复用 STT 的版本
	if c.Inference.TTS.SherpaVersion == "" {
		c.Inference.TTS.SherpaVersion = c.Inference.STT.SherpaVersion
	}

	if c.System.Tier < 0 || c.System.Tier > 3 {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("config: system.tier must be 0-3, got %d", c.System.Tier))
	}
	// go_memlimit_mb 为 0 代表不设 GOMEMLIMIT（由运行时自动管理），合法。
	// 非零时要求最低 64MB，低于此值会导致频繁 GC 甚至 OOM。
	if c.System.GoMemLimitMB != 0 && c.System.GoMemLimitMB < 64 {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("config: system.go_memlimit_mb must be >= 64 when set, got %d", c.System.GoMemLimitMB))
	}

	// Removed Sandbox.AllowedDomains check as the field is deleted.
	return nil
}

func GetThresholds(dataDir string) (*Thresholds, error) {
	t := DefaultThresholds()
	configDir := os.Getenv("POLARIS_THRESHOLDS_DIR")
	if configDir == "" {
		configDir = filepath.Join(dataDir, "config")
	}

	modules := map[string]interface{}{
		"m1_router.toml":        &t.M1Router,
		"m2_storage.toml":       &t.M2Storage,
		"m3_observability.toml": &t.M3Observability,
		"m4_kernel.toml":        &t.M4Kernel,
		"m5_memory.toml":        &t.M5Memory,
		"m6_skill.toml":         &t.M6Skill,
		"m7_tool.toml":          &t.M7Tool,
		"m8_orchestrator.toml":  &t.M8Orchestrator,
		"m9_self_improve.toml":  &t.M9SelfImprove,
		"m10_knowledge.toml":    &t.M10Knowledge,
		"m11_policy.toml":       &t.M11Policy,
		"m12_eval.toml":         &t.M12Eval,
		"m13_interface.toml":    &t.M13Interface,
	}

	for file, target := range modules {
		if err := loadModuleTOML(filepath.Join(configDir, file), target); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "GetThresholds", err)
		}
	}

	return &t, nil
}
