package guard

import (
	"strings"
	"testing"
)

func TestSystemPromptGuard_ScanRedactsVerbatimLeak(t *testing.T) {
	g := NewSystemPromptGuard(0)
	secret := strings.Repeat("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar ", 1)
	g.AddFragment(secret)

	leaked := "here is the answer: " + secret + " end of leak"
	cleaned, err := g.Scan(leaked, true)
	if err != nil {
		t.Fatalf("unexpected error with redact=true: %v", err)
	}
	if strings.Contains(cleaned, "alpha bravo charlie") {
		t.Errorf("expected leaked fragment to be redacted, got: %q", cleaned)
	}
	if !strings.Contains(cleaned, "[SYSTEM_REDACTED]") {
		t.Errorf("expected redaction marker in output, got: %q", cleaned)
	}
}

func TestSystemPromptGuard_ScanBlocksWhenRedactFalse(t *testing.T) {
	g := NewSystemPromptGuard(0)
	secret := strings.Repeat("one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen ", 1)
	g.AddFragment(secret)

	_, err := g.Scan("prefix "+secret+"suffix", false)
	if err != ErrPromptLeakage {
		t.Errorf("expected ErrPromptLeakage, got: %v", err)
	}
}

func TestSystemPromptGuard_ScanIgnoresShortFragmentsAndCleanOutput(t *testing.T) {
	g := NewSystemPromptGuard(0)
	g.AddFragment("too short to matter")    // < 15 words, should never match
	g.AddFragment("")                       // empty, should be ignored by AddFragment
	g.AddFragment(strings.Repeat("x ", 20)) // 20-word fragment, but output below doesn't contain it

	cleaned, err := g.Scan("completely unrelated benign response to the user", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cleaned != "completely unrelated benign response to the user" {
		t.Errorf("expected output unchanged, got: %q", cleaned)
	}
}

// TestKernelPromptFragments_LoadsRealTemplates 验证 KernelPromptFragments 真的
// 从 embed FS 加载到了非空的内核阶段模板内容（perceive/plan/reflect），且这些
// 内容本身满足 SystemPromptGuard 的最小 token 阈值——防止未来模板改名/移动后
// 该函数静默返回空列表，让 headless/SSE 两条路径的注册变成无意义的空操作。
func TestKernelPromptFragments_LoadsRealTemplates(t *testing.T) {
	frags := KernelPromptFragments()
	if len(frags) == 0 {
		t.Fatal("expected at least one kernel prompt fragment to load successfully")
	}
	for i, f := range frags {
		if len(strings.Fields(f)) < spgDefaultTokenThreshold {
			t.Errorf("fragment[%d] has fewer words than tokenThreshold, would never match in Scan: %q", i, f)
		}
	}
}

// TestKernelPromptFragments_DetectsVerbatimKernelLeak 端到端验证：把真实内核
// 模板注册进 Guard 后，若模型输出里逐字复现了模板的一段静态指令文本，会被检测到。
func TestKernelPromptFragments_DetectsVerbatimKernelLeak(t *testing.T) {
	frags := KernelPromptFragments()
	if len(frags) == 0 {
		t.Skip("no kernel fragments loaded, covered by TestKernelPromptFragments_LoadsRealTemplates")
	}

	g := NewSystemPromptGuard(0)
	for _, f := range frags {
		g.AddFragment(f)
	}

	// 取第一份模板的前 20 个词作为"泄露"样本。
	words := strings.Fields(frags[0])
	if len(words) < spgDefaultTokenThreshold {
		t.Skip("first fragment too short for this test")
	}
	sample := strings.Join(words[:spgDefaultTokenThreshold], " ")

	_, err := g.Scan("model said: "+sample, false)
	if err != ErrPromptLeakage {
		t.Errorf("expected verbatim kernel template leak to be detected, got: %v", err)
	}
}
