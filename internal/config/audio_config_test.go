package config

import (
	"os"
	"path/filepath"
	"testing"
)

func loadCfg(t *testing.T, toml string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// ADR-0107 默认值：sherpa/auto/sid=3/speed=1.0/空闲 10 分钟卸载。
func TestLoad_AudioDefaults(t *testing.T) {
	cfg := loadCfg(t, "")
	tts := cfg.Inference.TTS
	if tts.Provider != "sherpa" || tts.Engine != "auto" || tts.KokoroSID != 3 || tts.Speed != 1.0 {
		t.Errorf("TTS 默认值错误: %+v", tts)
	}
	if cfg.Inference.Audio.IdleUnloadMinutes != 10 {
		t.Errorf("idle_unload_minutes 默认应为 10，got %d", cfg.Inference.Audio.IdleUnloadMinutes)
	}
	if cfg.Inference.STT.UseITN || cfg.Inference.STT.Language != "zh" {
		t.Errorf("STT 默认值错误: %+v", cfg.Inference.STT)
	}
}

// 旧默认 config.toml 带 provider="edge" 与全套 edge_* 键：必须迁移为 sherpa 而不是让升级用户起不来。
func TestLoad_MigratesEdgeProviderToSherpa(t *testing.T) {
	cfg := loadCfg(t, `[inference.tts]
provider = "edge"
edge_voice = "zh-CN-XiaoxiaoNeural"
edge_style = "chat"
model_url = "https://example.invalid/old.tar.bz2"

[inference.stt]
model_precision = "fp32"
sense_voice_model_url = "https://example.invalid/fp32.tar.bz2"
`)
	if cfg.Inference.TTS.Provider != "sherpa" {
		t.Errorf("edge 应迁移为 sherpa，got %q", cfg.Inference.TTS.Provider)
	}
}

// 已删除的 provider 之外的非法值仍要 Fail-Fast。
func TestValidate_RejectsBadAudioConfig(t *testing.T) {
	cases := map[string]string{
		"provider": "[inference.tts]\nprovider = \"azure\"\n",
		"engine":   "[inference.tts]\nengine = \"cloud\"\n",
		"sid":      "[inference.tts]\nkokoro_sid = -1\n",
		"idle":     "[inference.audio]\nidle_unload_minutes = -5\n",
	}
	for name, body := range cases {
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%s: 非法配置应被拒绝", name)
		}
	}
}

func TestLoad_AudioUserOverrides(t *testing.T) {
	cfg := loadCfg(t, "[inference.tts]\nkokoro_sid = 50\nspeed = 1.2\nengine = \"system\"\n[inference.audio]\nidle_unload_minutes = 0\n")
	if cfg.Inference.TTS.KokoroSID != 50 || cfg.Inference.TTS.Speed != 1.2 || cfg.Inference.TTS.Engine != "system" {
		t.Errorf("用户覆盖未生效: %+v", cfg.Inference.TTS)
	}
	if cfg.Inference.Audio.IdleUnloadMinutes != 0 {
		t.Error("idle_unload_minutes=0（不卸载）应被保留")
	}
}
