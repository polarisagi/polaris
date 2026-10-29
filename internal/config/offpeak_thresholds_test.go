package config

import "testing"

// ADR-0105 决策七/八：错峰窗口默认空（不错峰）、格式在加载期校验；llm_calls 保留期默认 90 天。
func TestM1OffpeakAndRetention_DefaultsAndValidate(t *testing.T) {
	d := DefaultThresholds().M1Router
	if len(d.OffpeakWindows) != 0 {
		t.Fatalf("offpeak.windows 默认必须为空（不硬编码任何厂商时刻表）：%v", d.OffpeakWindows)
	}
	if d.UsageRetentionDays != 90 {
		t.Fatalf("usage.retention_days 默认应为 90：%d", d.UsageRetentionDays)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	ok := d
	ok.OffpeakWindows = []string{"16:00-24:00", "22:00-06:00"}
	ok.UsageRetentionDays = 0 // 0 = 不清理，合法
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法窗口/保留期被拒：%v", err)
	}
	for _, bad := range []string{"16:00", "25:00-26:00", "10:00-10:00", ""} {
		m := d
		m.OffpeakWindows = []string{bad}
		if m.Validate() == nil {
			t.Fatalf("非法窗口 %q 必须在加载期报错", bad)
		}
	}
	neg := d
	neg.UsageRetentionDays = -1
	if neg.Validate() == nil {
		t.Fatal("负保留期必须报错")
	}
}
