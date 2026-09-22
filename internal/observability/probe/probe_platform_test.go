package probe

import (
	"runtime"
	"testing"
)

// TestProbeOSMemory_NotFallback 守住"平台原生探针必须真的生效"这条线。
//
// 2026-09-22 背景：darwin 实现基于 sysctl "vm.vmtotal"，而该 sysctl 在现代
// macOS 上根本不存在，于是每次都静默落到 `available = total * 40 / 100` 的盲猜
// 兜底上（16GB 机器恒报 6553MB，与真实可用量差 2.6 倍）；windows 实现则直接
// `return fallbackMemoryProbe()`，连总量都是硬编码的"假设 8GB"。两者都不会报错，
// 只会安静地返回编造的数字，而 Tier 分级、FeatureGate 硬件门控、
// ResourceGovernor 准入判定全建立在这个数字上。
//
// 判据：三大平台上 probeOSMemory 的结果必须与 fallbackMemoryProbe 不同。
// fallback 的 total 是硬编码 8GiB 且 available 走 runtime.MemStats，真实探针
// 两个值同时与之相等的概率可忽略。
func TestProbeOSMemory_NotFallback(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skipf("平台 %s 无原生探针实现，跳过", runtime.GOOS)
	}

	total, available := probeOSMemory()
	fbTotal, fbAvailable := fallbackMemoryProbe()

	if total == fbTotal && available == fbAvailable {
		t.Fatalf("probeOSMemory 退化为 fallback（total=%d available=%d）——平台原生探针未生效", total, available)
	}
	if total == 0 {
		t.Fatal("total 不应为 0")
	}
	if available == 0 {
		t.Fatal("available 不应为 0")
	}
	if available > total {
		t.Fatalf("available(%d) 不应大于 total(%d)", available, total)
	}
	t.Logf("%s: total=%d MB available=%d MB", runtime.GOOS, total/1024/1024, available/1024/1024)
}

// TestCPUSampler_InRange 校验 CPU 探针取值域。
// 旧实现在非 Linux 平台按 goroutine 数量返回固定的 20/50/80，本测试不区分
// 平台断言具体值（CI runner 负载不可控），只守住取值域与不 panic。
func TestCPUSampler_InRange(t *testing.T) {
	s := NewCPUSampler()
	for range 3 {
		v := s.Usage()
		if v < 0 || v > 100 {
			t.Fatalf("CPU 占用率越界: %f", v)
		}
	}
}
