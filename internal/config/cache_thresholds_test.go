package config

import "testing"

// ADR-0105 决策三：缓存相关阈值的默认值与枚举校验。
func TestM1CacheThresholds_DefaultsAndValidate(t *testing.T) {
	d := DefaultThresholds()
	if d.M1Router.AnthropicCacheTTL != "5m" {
		t.Fatalf("anthropic.cache_ttl 默认应为 5m：%q", d.M1Router.AnthropicCacheTTL)
	}
	if d.M1Router.OpenAIPromptCacheRetention != "" {
		t.Fatalf("openai.prompt_cache_retention 默认应为空（不发送）：%q", d.M1Router.OpenAIPromptCacheRetention)
	}
	if d.M4Kernel.CacheUniformTools {
		t.Fatal("cache.uniform_tools 默认必须为 false（实验开关）")
	}
	if err := d.M1Router.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, ttl := range []string{"5m", "1h", ""} {
		m := d.M1Router
		m.AnthropicCacheTTL = ttl
		if err := m.Validate(); err != nil {
			t.Fatalf("ttl %q 应合法：%v", ttl, err)
		}
	}
	for _, r := range []string{"in_memory", "24h", ""} {
		m := d.M1Router
		m.OpenAIPromptCacheRetention = r
		if err := m.Validate(); err != nil {
			t.Fatalf("retention %q 应合法：%v", r, err)
		}
	}
	bad := d.M1Router
	bad.AnthropicCacheTTL = "2h"
	if bad.Validate() == nil {
		t.Fatal("非法 TTL 必须在加载期报错")
	}
	bad = d.M1Router
	bad.OpenAIPromptCacheRetention = "7d"
	if bad.Validate() == nil {
		t.Fatal("非法 retention 必须在加载期报错")
	}
}

// ADR-0105 决策一：非首部 system 内联开关默认开启（内核主路径依赖 L3 阶段层位置），
// bool 无非法取值，Validate 不需额外分支。
func TestM1InlineNonLeadingSystem_DefaultsOn(t *testing.T) {
	d := DefaultThresholds()
	if !d.M1Router.AnthropicInlineNonLeadingSystem || !d.M1Router.GoogleInlineNonLeadingSystem {
		t.Fatalf("anthropic/google.inline_nonleading_system 默认应为 true：%+v", d.M1Router)
	}
	if err := d.M1Router.Validate(); err != nil {
		t.Fatal(err)
	}
}
