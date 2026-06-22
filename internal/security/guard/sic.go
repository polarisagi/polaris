package guard

import (
	"context"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// SICCleaner — Spotlighting + SIC Instruction Cleaner（M11 §2.2）。
// 架构文档: docs/arch/M11-Policy-Safety.md §2.2
//
// SIC CleanInstructions 算法（maxIter=5）:
//   步骤1 detect: 检测 override/extract/reset/伪系统指令模式 → bool
//   步骤2 rewrite: 将危险模式替换为安全标记 [REDACTED_INJECTION]
//   步骤3 iterate: 连续两次文本相同 → 完成
//   步骤4 bailout: 达 maxIter 仍检测到注入 → ErrUncleanableContent

const maxSICIter = 5

// ErrUncleanableContent 经 maxIter 次迭代清洗后仍检测到注入模式。
var ErrUncleanableContent = apperr.New(apperr.CodeInvalidInput, "policy: SIC — content still contains injection patterns after max iterations")

// SICCleaner 执行 SIC 指令清洗。
// MVP 期间以本地正则规则为主；Tier 1+ 可替换为 LLM 感知的检测器。
type SICCleaner struct {
	// detectFn 允许调用方注入 LLM 感知的检测器（nil 则使用内置正则）。
	// 返回 true 表示文本中存在注入模式。
	detectFn func(ctx context.Context, text string) (bool, error)
}

// NewSICCleaner 创建使用内置规则的 SIC 清洗器。
func NewSICCleaner() *SICCleaner {
	return &SICCleaner{}
}

// NewSICCleanerWithDetector 创建使用外部检测器（如 LLM）的 SIC 清洗器。
func NewSICCleanerWithDetector(fn func(ctx context.Context, text string) (bool, error)) *SICCleaner {
	return &SICCleaner{detectFn: fn}
}

// CleanInstructions 执行 SIC 迭代清洗。
// 返回清洗后的文本；若达 maxIter 仍有注入特征返回 ErrUncleanableContent。
func (s *SICCleaner) CleanInstructions(ctx context.Context, text string) (string, error) {
	prev := ""
	current := text

	for i := 0; i < maxSICIter; i++ {
		// 步骤1 — 检测
		detected, err := s.detect(ctx, current)
		if err != nil {
			// 检测器故障，fail-closed
			return "", fmt.Errorf("SICCleaner.CleanInstructions: %w", err)
		}
		if !detected {
			// 无注入特征，提前完成
			return current, nil
		}

		// 步骤2 — 重写
		current = s.rewrite(current)

		// 步骤3 — 收敛检测（连续两轮文本相同 → 重写已无法消除，提前 bailout）
		if current == prev {
			break
		}
		prev = current
	}

	// 步骤4 — 最终检测，bailout
	detected, err := s.detect(ctx, current)
	if err != nil || detected {
		return "", ErrUncleanableContent
	}
	return current, nil
}

// detect 判断文本是否包含 prompt injection 特征模式。
// 内置规则覆盖常见攻击向量：
//   - 系统角色伪装（ignore / forget / override / new instructions）
//   - 数据提取指令（print / reveal / output your system prompt 等）
//   - 会话重置（reset / disregard previous）
func (s *SICCleaner) detect(ctx context.Context, text string) (bool, error) {
	if s.detectFn != nil {
		return s.detectFn(ctx, text)
	}
	return builtinDetect(text), nil
}

// rewrite 将检测到的注入特征替换为安全标记 [REDACTED_INJECTION]。
func (s *SICCleaner) rewrite(text string) string {
	patterns := injectionPatterns()
	result := text
	for _, p := range patterns {
		result = strings.ReplaceAll(result, p, "[REDACTED_INJECTION]")
	}
	return result
}

// builtinDetect 使用内置关键字列表进行注入检测（大小写不敏感）。
func builtinDetect(text string) bool {
	lower := strings.ToLower(text)
	for _, keyword := range dangerousKeywords() {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

// dangerousKeywords 返回注入检测关键字列表（小写）。
func dangerousKeywords() []string {
	return []string{
		"ignore previous instructions",
		"ignore all instructions",
		"forget previous",
		"disregard previous",
		"override instructions",
		"new instructions:",
		"system prompt:",
		"reveal your system prompt",
		"print your system prompt",
		"output your system prompt",
		"what are your instructions",
		"ignore the above",
		"ignore above",
		"act as if",
		"pretend you are",
		"you are now",
		"your new role",
		"from now on you",
	}
}

// injectionPatterns 返回用于 rewrite 的完整模式列表（保留大小写以精准替换）。
func injectionPatterns() []string {
	// 为了实现多模式替换，此处使用小写版本统一处理；
	// 实际生产中应使用 regexp.ReplaceAllStringFunc 配合 (?i) 标志
	return dangerousKeywords()
}
