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
	userData, readErr := os.ReadFile(path)
	if readErr == nil {
		if err := toml.Unmarshal(userData, cfg); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("Load: parse %s", path), err)
		}
		warnLegacyAudioKeys(userData)
		migrateLegacyTTSProvider(&cfg.Inference.TTS)
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

// legacyAudioKeys 是 ADR-0107 之前 [inference.stt] / [inference.tts] 里的配置键，现已无效：
// 资产 URL 改由代码内清单（含 sha256）管理，STT 固定 int8，Edge TTS 整条删除。
// 用户 config.toml 是首次启动时由旧模板导出的副本，这些键会一直留在里面。
func legacyAudioKeys() (stt, tts []string) {
	return []string{"sense_voice_model_url", "sense_voice_model_url_std", "punct_model_url", "model_precision"},
		[]string{"edge_voice", "edge_style", "edge_client_version", "model_url", "tokens_url"}
}

// warnLegacyAudioKeys 对用户 config.toml 里出现的已失效音频配置键各打一条 Warn（忽略而非报错：
// 报错会让升级后的老用户无法启动）。toml.Unmarshal 对未知键静默忽略，所以必须单独探测。
func warnLegacyAudioKeys(userData []byte) {
	var probe struct {
		Inference struct {
			STT map[string]any `toml:"stt"`
			TTS map[string]any `toml:"tts"`
		} `toml:"inference"`
	}
	if err := toml.Unmarshal(userData, &probe); err != nil {
		return // 解析错误已由上层 Unmarshal 报告，这里只做尽力探测
	}
	sttKeys, ttsKeys := legacyAudioKeys()
	for _, k := range sttKeys {
		if _, ok := probe.Inference.STT[k]; ok {
			slog.Warn("config: inference.stt 配置键已失效，被忽略（资产改由内置清单管理，STT 固定 int8）", "key", k)
		}
	}
	for _, k := range ttsKeys {
		if _, ok := probe.Inference.TTS[k]; ok {
			slog.Warn("config: inference.tts 配置键已失效，被忽略（Edge TTS 已删除，资产改由内置清单管理）", "key", k)
		}
	}
}

// migrateLegacyTTSProvider 把已删除的 provider="edge" 迁移为 "sherpa" 并 Warn。
// 为什么不是直接报错：旧默认 config.toml 就是 provider="edge"，升级即报错等于让全部老用户起不来。
func migrateLegacyTTSProvider(tts *TTSConfig) {
	if tts.Provider == "edge" {
		slog.Warn("config: inference.tts.provider=\"edge\" 已删除（ADR-0107），迁移为 \"sherpa\"（本地 Kokoro，按需下载）")
		tts.Provider = "sherpa"
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
	switch c.Inference.TTS.Provider {
	case "":
		c.Inference.TTS.Provider = "sherpa"
	case "sherpa", "http":
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf(
			"config: inference.tts.provider must be \"sherpa\" or \"http\", got %q", c.Inference.TTS.Provider))
	}
	switch c.Inference.TTS.Engine {
	case "":
		c.Inference.TTS.Engine = "auto"
	case "auto", "server", "system":
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf(
			"config: inference.tts.engine must be \"auto\", \"server\" or \"system\", got %q", c.Inference.TTS.Engine))
	}
	if c.Inference.TTS.KokoroSID < 0 {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("config: inference.tts.kokoro_sid must be >= 0, got %d", c.Inference.TTS.KokoroSID))
	}
	if c.Inference.TTS.Speed <= 0 {
		c.Inference.TTS.Speed = 1.0
	}
	if c.Inference.Audio.IdleUnloadMinutes < 0 {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf(
			"config: inference.audio.idle_unload_minutes must be >= 0 (0 = never unload), got %d", c.Inference.Audio.IdleUnloadMinutes))
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
