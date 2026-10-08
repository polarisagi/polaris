package audiorun

import (
	"strings"
	"testing"
)

const gib = uint64(1) << 30

const mib = uint64(1) << 20

func prof(ram uint64, cores int) HardwareProfile {
	return HardwareProfile{TotalRAMBytes: ram, LogicalCores: cores, GOOS: "linux", GOARCH: "amd64"}
}

// 两档判定矩阵（ADR-0110 修订）：边界取 1799/1800MB 与 1/2 核；STT 与 TTS 同进退。
func TestAudioSupport_Tiers(t *testing.T) {
	cases := []struct {
		name string
		p    HardwareProfile
		ok   bool
		why  string
	}{
		{"1GB/1核", prof(1*gib, 1), false, ReasonInsufficientRAM},
		{"1799MB/4核", prof(1799*mib, 4), false, ReasonInsufficientRAM},
		{"1800MB/2核 恰好支持", prof(1800*mib, 2), true, ""},
		{"1800MB/1核", prof(1800*mib, 1), false, ReasonInsufficientCores},
		{"16GB/1核 单核即使内存充足也关", prof(16*gib, 1), false, ReasonInsufficientCores},
		{"2GB VPS 实际可见 1.9GB/2核", prof(gib*19/10, 2), true, ""},
		{"4GB 双核旧笔记本", prof(4*gib, 2), true, ""},
		{"16GB/16核", prof(16*gib, 16), true, ""},
	}
	for _, c := range cases {
		got := AudioSupport(c.p)
		if got.STT.Supported != c.ok || got.TTS.Supported != c.ok {
			t.Errorf("%s: stt=%v tts=%v, want %v", c.name, got.STT.Supported, got.TTS.Supported, c.ok)
			continue
		}
		if !c.ok && (got.STT.Reason != c.why || got.TTS.Reason != c.why) {
			t.Errorf("%s: reason stt=%q tts=%q, want %q", c.name, got.STT.Reason, got.TTS.Reason, c.why)
		}
	}
}

// 核数边界：1/2 核（内存充足）。
func TestAudioSupport_CoreBoundaries(t *testing.T) {
	if AudioSupport(prof(8*gib, 1)).TTS.Supported {
		t.Error("1 核不应支持")
	}
	if !AudioSupport(prof(8*gib, 2)).TTS.Supported {
		t.Error("2 核应支持")
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
	if !strings.Contains(got.STT.Message, "2GB") || !strings.Contains(got.TTS.Message, "2GB") {
		t.Errorf("文案缺少最低配置: stt=%q tts=%q", got.STT.Message, got.TTS.Message)
	}
	if !strings.Contains(got.STT.Message, "语音输入") || !strings.Contains(got.TTS.Message, "服务端朗读") {
		t.Errorf("文案应按能力点名: stt=%q tts=%q", got.STT.Message, got.TTS.Message)
	}
}

// 判定只看稳定画像：HardwareProfile 结构里就没有空闲内存字段（编译期保证），
// 这里确认同一画像重复判定结果恒定。
func TestAudioSupport_Deterministic(t *testing.T) {
	p := prof(8*gib, 8)
	if a, b := AudioSupport(p), AudioSupport(p); a != b {
		t.Error("同一画像判定结果必须恒定")
	}
}

// 线程数：Melo min(4, 核)，至少 1。
func TestTTSThreads(t *testing.T) {
	for cores, want := range map[int]int{0: 1, 1: 1, 2: 2, 4: 4, 16: 4} {
		if got := ttsThreads(cores); got != want {
			t.Errorf("ttsThreads(%d)=%d, want %d", cores, got, want)
		}
	}
}

func TestFreeMemThresholds(t *testing.T) {
	if sttMinFreeMB != 600 || ttsMinFreeMB != 800 {
		t.Error("空闲内存门槛应为 STT 600 / Melo 800")
	}
}
