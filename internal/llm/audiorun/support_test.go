package audiorun

import (
	"strings"
	"testing"
)

const gib = uint64(1) << 30

func prof(ram uint64, cores int) HardwareProfile {
	return HardwareProfile{TotalRAMBytes: ram, LogicalCores: cores, GOOS: "linux", GOARCH: "amd64"}
}

// 最低配置边界（audio-v2-spec §1）：STT 2GB/2 核，服务端 TTS 4GB/4 核。
func TestAudioSupport_Matrix(t *testing.T) {
	cases := []struct {
		name     string
		p        HardwareProfile
		stt, tts bool
		sttWhy   string
		ttsWhy   string
	}{
		{"1GB/1核", prof(1*gib, 1), false, false, ReasonInsufficientRAM, ReasonInsufficientRAM},
		{"2GB VPS 实际可见 1.9GB/2核", prof(gib*19/10, 2), true, false, "", ReasonInsufficientRAM},
		{"2GB/1核", prof(2*gib, 1), false, false, ReasonInsufficientCores, ReasonInsufficientRAM},
		{"4GB 实际可见 3.8GB/4核", prof(gib*38/10, 4), true, true, "", ""},
		{"4GB/2核", prof(4*gib, 2), true, false, "", ReasonInsufficientCores},
		{"16GB/16核", prof(16*gib, 16), true, true, "", ""},
	}
	for _, c := range cases {
		got := AudioSupport(c.p)
		if got.STT.Supported != c.stt || got.TTS.Supported != c.tts {
			t.Errorf("%s: stt=%v tts=%v, want %v/%v", c.name, got.STT.Supported, got.TTS.Supported, c.stt, c.tts)
		}
		if !c.stt && got.STT.Reason != c.sttWhy {
			t.Errorf("%s: stt reason=%q, want %q", c.name, got.STT.Reason, c.sttWhy)
		}
		if !c.tts && got.TTS.Reason != c.ttsWhy {
			t.Errorf("%s: tts reason=%q, want %q", c.name, got.TTS.Reason, c.ttsWhy)
		}
	}
}

// 不受支持的平台两者都 unsupported_platform，且文案点名平台。
func TestAudioSupport_UnsupportedPlatform(t *testing.T) {
	p := prof(16*gib, 16)
	p.GOOS, p.GOARCH = "freebsd", "riscv64"
	got := AudioSupport(p)
	if got.STT.Supported || got.TTS.Supported || got.STT.Reason != ReasonUnsupportedPlatform {
		t.Errorf("未知平台应全部不支持: %+v", got)
	}
	if !strings.Contains(got.STT.Message, "freebsd/riscv64") {
		t.Errorf("文案应点名平台: %s", got.STT.Message)
	}
}

// 不支持的文案必须包含最低配置（前端直接展示）。
func TestAudioSupport_MessageStatesMinimum(t *testing.T) {
	got := AudioSupport(prof(1*gib, 1))
	if !strings.Contains(got.STT.Message, "2GB") || !strings.Contains(got.TTS.Message, "4GB") {
		t.Errorf("文案缺少最低配置: stt=%q tts=%q", got.STT.Message, got.TTS.Message)
	}
}

// 判定只看稳定画像：HardwareProfile 结构里就没有空闲内存字段（编译期保证），
// 这里确认同一画像重复判定结果恒定。
func TestAudioSupport_Deterministic(t *testing.T) {
	p := prof(8*gib, 8)
	a, b := AudioSupport(p), AudioSupport(p)
	if a != b {
		t.Error("同一画像判定结果必须恒定")
	}
}
