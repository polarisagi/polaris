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
	A2A           A2AConfig           `toml:"a2a"`
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
	defaultSTT := cfg.Inference.STT // 迁移用：旧错误默认值要换成的新默认值
	userData, readErr := os.ReadFile(path)
	if readErr == nil {
		if err := toml.Unmarshal(userData, cfg); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("Load: parse %s", path), err)
		}
		migrateLegacySTTDefaults(&cfg.Inference.STT, defaultSTT)
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

// 旧版 defaults.toml 模板里的两条错误 URL（会被首次启动原样写进用户 config.toml）：
//   - int8 SenseVoice 的 URL 把 "int8" 放在日期之后，实际归档名是 "...-int8-2025-09-09"，旧 URL 404；
//   - 标点模型是 fp32 版（279MB），而代码按 int8 归档的文件名假设处理。
//
// 用户 config.toml 是旧模板生成的副本，升级后若不迁移，新默认值被旧文件覆盖，STT 仍然是坏的。
const (
	legacySTTModelURLStd = "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2025-09-09-int8.tar.bz2"
	legacySTTPunctURL    = "https://github.com/k2-fsa/sherpa-onnx/releases/download/punctuation-models/sherpa-onnx-punct-ct-transformer-zh-en-vocab272727-2024-04-12.tar.bz2"
)

// migrateLegacySTTDefaults 仅在字段值与旧错误默认值精确相等时替换为新默认值并告警；
// 用户自定义过的 URL 一律不动。
func migrateLegacySTTDefaults(stt *STTConfig, def STTConfig) {
	if stt.SenseVoiceModelURLStd == legacySTTModelURLStd {
		slog.Warn("config: inference.stt.sense_voice_model_url_std 是已知错误的旧默认值（404），已迁移为新默认值",
			"old", legacySTTModelURLStd, "new", def.SenseVoiceModelURLStd)
		stt.SenseVoiceModelURLStd = def.SenseVoiceModelURLStd
	}
	if stt.PunctModelURL == legacySTTPunctURL {
		slog.Warn("config: inference.stt.punct_model_url 是旧默认值（fp32 279MB），已迁移为 int8 新默认值",
			"old", legacySTTPunctURL, "new", def.PunctModelURL)
		stt.PunctModelURL = def.PunctModelURL
	}
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
	if c.Inference.STT.Language == "" {
		c.Inference.STT.Language = "zh"
	}
	switch c.Inference.STT.ModelPrecision {
	case "":
		c.Inference.STT.ModelPrecision = "int8"
	case "int8", "fp32":
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf(
			"config: inference.stt.model_precision must be \"int8\" or \"fp32\", got %q", c.Inference.STT.ModelPrecision))
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

	if err := t.M1Router.Validate(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "GetThresholds", err)
	}
	if err := t.M4Kernel.Validate(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "GetThresholds", err)
	}
	return &t, nil
}
