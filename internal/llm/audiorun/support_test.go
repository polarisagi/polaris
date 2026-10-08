package audiorun

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/llm/tts"
)

const gib = uint64(1) << 30

const mib = uint64(1) << 20

func prof(ram uint64, cores int) HardwareProfile {
	return HardwareProfile{TotalRAMBytes: ram, LogicalCores: cores, GOOS: "linux", GOARCH: "amd64"}
}

// 三档判定矩阵（ADR-0110 决策 2）：边界取 1799/1800/3599/3600MB 与 1/2/3/4 核。
func TestAudioSupport_Tiers(t *testing.T) {
	cases := []struct {
		name   string
		p      HardwareProfile
		tier   string
		stt    bool
		model  string
		sttWhy string
	}{
		{"1GB/1核", prof(1*gib, 1), TierA, false, TTSModelNone, ReasonInsufficientRAM},
		{"1799MB/4核", prof(1799*mib, 4), TierA, false, TTSModelNone, ReasonInsufficientRAM},
		{"1800MB/4核 恰好 B", prof(1800*mib, 4), TierB, true, "matcha", ""},
		{"1800MB/1核", prof(1800*mib, 1), TierA, false, TTSModelNone, ReasonInsufficientCores},
		{"16GB/1核 单核即使内存充足也关", prof(16*gib, 1), TierA, false, TTSModelNone, ReasonInsufficientCores},
		{"2GB VPS 实际可见 1.9GB/2核", prof(gib*19/10, 2), TierB, true, "matcha", ""},
		{"16GB/2核", prof(16*gib, 2), TierB, true, "matcha", ""},
		{"16GB/3核", prof(16*gib, 3), TierB, true, "matcha", ""},
		{"4GB 双核旧笔记本", prof(4*gib, 2), TierB, true, "matcha", ""},
		{"3599MB/4核", prof(3599*mib, 4), TierB, true, "matcha", ""},
		{"3600MB/3核", prof(3600*mib, 3), TierB, true, "matcha", ""},
		{"3600MB/4核 恰好 C", prof(3600*mib, 4), TierC, true, "melo", ""},
		{"4GB 实际可见 3.8GB/4核", prof(gib*38/10, 4), TierC, true, "melo", ""},
		{"16GB/16核", prof(16*gib, 16), TierC, true, "melo", ""},
	}
	for _, c := range cases {
		got := AudioSupport(c.p, "auto")
		if got.Tier != c.tier || got.STT.Supported != c.stt || got.TTSModel != c.model {
			t.Errorf("%s: tier=%s stt=%v model=%s, want %s/%v/%s", c.name, got.Tier, got.STT.Supported, got.TTSModel, c.tier, c.stt, c.model)
			continue
		}
		wantTTS := c.model != TTSModelNone
		if got.TTS.Supported != wantTTS {
			t.Errorf("%s: tts supported=%v, want %v", c.name, got.TTS.Supported, wantTTS)
		}
		if !c.stt {
			if got.STT.Reason != c.sttWhy || got.TTS.Reason != c.sttWhy {
				t.Errorf("%s: reason stt=%q tts=%q, want %q", c.name, got.STT.Reason, got.TTS.Reason, c.sttWhy)
			}
		}
		if got.TTSNote == "" {
			t.Errorf("%s: 必须给出面向用户的说明", c.name)
		}
	}
}

// 核数边界：1/2/3/4 核（内存充足）。
func TestAudioSupport_CoreBoundaries(t *testing.T) {
	want := map[int]string{1: TierA, 2: TierB, 3: TierB, 4: TierC}
	for cores, tier := range want {
		if got := AudioSupport(prof(8*gib, cores), "").Tier; got != tier {
			t.Errorf("%d 核: tier=%s, want %s", cores, got, tier)
		}
	}
}

// 显式指定可越档，但不能解锁 A 档。
func TestAudioSupport_ExplicitModel(t *testing.T) {
	if got := AudioSupport(prof(2*gib, 2), "melo"); got.TTSModel != "melo" || !got.TTS.Supported || got.Tier != TierB {
		t.Errorf("B 档指定 melo 应允许: %+v", got)
	}
	if got := AudioSupport(prof(16*gib, 16), "matcha"); got.TTSModel != "matcha" || got.Tier != TierC {
		t.Errorf("C 档指定 matcha 应允许: %+v", got)
	}
	for _, pref := range []string{"melo", "matcha", "auto"} {
		got := AudioSupport(prof(1*gib, 1), pref)
		if got.TTS.Supported || got.TTSModel != TTSModelNone || got.STT.Supported {
			t.Errorf("A 档指定 %s 不得解锁: %+v", pref, got)
		}
	}
	if m, ok := AudioSupport(prof(16*gib, 16), "").Model(); !ok || m != tts.ModelMelo {
		t.Errorf("Model() = %v %v", m, ok)
	}
	if _, ok := AudioSupport(prof(1*gib, 1), "").Model(); ok {
		t.Error("A 档 Model() 应返回 ok=false")
	}
}

// 不受支持的平台两者都 unsupported_platform，且文案点名平台。
func TestAudioSupport_UnsupportedPlatform(t *testing.T) {
	p := prof(16*gib, 16)
	p.GOOS, p.GOARCH = "freebsd", "riscv64"
	got := AudioSupport(p, "")
	if got.STT.Supported || got.TTS.Supported || got.STT.Reason != ReasonUnsupportedPlatform || got.TTSModel != TTSModelNone {
		t.Errorf("未知平台应全部不支持: %+v", got)
	}
	if !strings.Contains(got.STT.Message, "freebsd/riscv64") {
		t.Errorf("文案应点名平台: %s", got.STT.Message)
	}
}

// 不支持的文案必须包含最低配置（前端直接展示）。
func TestAudioSupport_MessageStatesMinimum(t *testing.T) {
	got := AudioSupport(prof(1*gib, 1), "")
	if !strings.Contains(got.STT.Message, "2GB") || !strings.Contains(got.TTS.Message, "2GB") {
		t.Errorf("文案缺少最低配置: stt=%q tts=%q", got.STT.Message, got.TTS.Message)
	}
}

// 判定只看稳定画像：HardwareProfile 结构里就没有空闲内存字段（编译期保证），
// 这里确认同一画像重复判定结果恒定。
func TestAudioSupport_Deterministic(t *testing.T) {
	p := prof(8*gib, 8)
	a, b := AudioSupport(p, ""), AudioSupport(p, "")
	if a != b {
		t.Error("同一画像判定结果必须恒定")
	}
}

// 线程数：Melo min(4, 核)，Matcha min(2, 核)，至少 1。
func TestThreadsFor(t *testing.T) {
	cases := []struct {
		m     tts.Model
		cores int
		want  int
	}{
		{tts.ModelMelo, 1, 1}, {tts.ModelMelo, 2, 2}, {tts.ModelMelo, 4, 4}, {tts.ModelMelo, 16, 4}, {tts.ModelMelo, 0, 1},
		{tts.ModelMatcha, 1, 1}, {tts.ModelMatcha, 2, 2}, {tts.ModelMatcha, 8, 2},
	}
	for _, c := range cases {
		if got := ThreadsFor(c.m, c.cores); got != c.want {
			t.Errorf("ThreadsFor(%s,%d)=%d, want %d", c.m, c.cores, got, c.want)
		}
	}
}

func TestMinFreeMB(t *testing.T) {
	if MinFreeMB(tts.ModelMelo) != 800 || MinFreeMB(tts.ModelMatcha) != 450 || sttMinFreeMB != 600 {
		t.Error("空闲内存门槛应为 STT 600 / Melo 800 / Matcha 450")
	}
}
