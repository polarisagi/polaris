package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 旧模板生成的用户 config.toml 带着已知错误的 URL：升级后必须被新默认值替换，
// 否则新默认值被旧文件覆盖，STT 仍然是坏的。
func TestLoad_MigratesLegacySTTDefaults(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	legacy := "[inference.stt]\n" +
		"sense_voice_model_url_std = \"" + legacySTTModelURLStd + "\"\n" +
		"punct_model_url = \"" + legacySTTPunctURL + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	stt := cfg.Inference.STT
	if !strings.HasSuffix(stt.SenseVoiceModelURLStd, "sense-voice-zh-en-ja-ko-yue-int8-2025-09-09.tar.bz2") {
		t.Errorf("std URL not migrated: %s", stt.SenseVoiceModelURLStd)
	}
	if !strings.HasSuffix(stt.PunctModelURL, "2024-04-12-int8.tar.bz2") {
		t.Errorf("punct URL not migrated: %s", stt.PunctModelURL)
	}
	if stt.ModelPrecision != "int8" || stt.UseITN {
		t.Errorf("defaults wrong: precision=%q use_itn=%v", stt.ModelPrecision, stt.UseITN)
	}
}

// 用户自定义过的 URL 不得被迁移覆盖。
func TestLoad_KeepsCustomSTTURL(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	custom := "https://mirror.example/my-int8.tar.bz2"
	if err := os.WriteFile(cfgPath, []byte("[inference.stt]\nsense_voice_model_url_std = \""+custom+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Inference.STT.SenseVoiceModelURLStd != custom {
		t.Errorf("custom URL must be kept, got %s", cfg.Inference.STT.SenseVoiceModelURLStd)
	}
}

func TestValidate_ModelPrecision(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[inference.stt]\nmodel_precision = \"fp16\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("invalid model_precision should be rejected")
	}
}
