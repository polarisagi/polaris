// Package mcp — tool_scanner.go
//
// ToolSecurityScanner 在 MCP 工具注册前检测 prompt injection 风险。
//
// 设计依据: HE-Rule 2（可验证执行，物理断裂 > 概率过滤）
// 策略：
//   - 工具名/描述/参数说明中出现注入模式 → 标记 RiskHITL 或 RiskDeny
//   - fail-open：无法解析时放行（防止正常工具被误杀），但记录 Warn
//   - 结果不阻断注册，由调用方决策是否拒绝（安全性由调用方强制，扫描器只提供判断）

package mcp

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"sync"
)

// ScanRisk 工具安全扫描风险等级。
type ScanRisk int

const (
	// ScanRiskSafe 无已知注入风险
	ScanRiskSafe ScanRisk = iota
	// ScanRiskWarn 存在可疑模式，建议审查
	ScanRiskWarn
	// ScanRiskHITL 高风险，需要人工确认后方可使用
	ScanRiskHITL
	// ScanRiskDeny 极高风险，建议拒绝注册
	ScanRiskDeny
)

// ToolScanResult 工具安全扫描结果。
type ToolScanResult struct {
	ToolName string
	Risk     ScanRisk
	Reasons  []string
}

// ─── 注入模式正则 ─────────────────────────────────────────────────────────────

// injectionPatternsDeny 命中则直接 Deny（典型攻击载荷）。
// R1.3：包级可变变量禁用于 internal/，用 sync.OnceValue 封装为懒加载只读函数
// （对齐 internal/knowledge/graphrag/entity.go 先例，GR-4-001 修复）。
//
// new-from-rev 基线（f1c8205）存在而被豁免，此处为等价重构，显式豁免以保持一致
//
//nolint:gochecknoglobals // sync.OnceValue 懒加载只读正则，无可变状态；entity.go 同类声明因先于
var injectionPatternsDeny = sync.OnceValue(func() []*regexp.Regexp {
	return []*regexp.Regexp{
		// 直接指令覆盖模式
		regexp.MustCompile(`(?i)ignore\s+(all\s+)?previous\s+instructions?`),
		regexp.MustCompile(`(?i)disregard\s+(all\s+)?previous\s+instructions?`),
		regexp.MustCompile(`(?i)forget\s+(all\s+)?previous\s+instructions?`),
		regexp.MustCompile(`(?i)override\s+(all\s+)?instructions?`),
		// 系统提示重置
		regexp.MustCompile(`(?i)(new|updated?)\s+system\s+prompt`),
		regexp.MustCompile(`(?i)\[\[?SYSTEM\]?\]`),
		regexp.MustCompile(`(?i)<\|system\|>`),
		// 越权执行指令
		regexp.MustCompile(`(?i)you\s+are\s+now\s+(a\s+)?(?:DAN|jailbreak|uncensored)`),
		// 凭据窃取模式
		regexp.MustCompile(`(?i)(send|exfiltrate|steal|leak|upload)\s+(api\s+key|password|secret|token|credential)`),
		// 编码混淆（Base64 注入）
		regexp.MustCompile(`(?i)base64\s*decode\s+and\s+(execute|run|eval)`),
	}
})

// injectionPatternsHITL 命中则 HITL（需人工审核）
//
//nolint:gochecknoglobals // 同上 injectionPatternsDeny 豁免理由
var injectionPatternsHITL = sync.OnceValue(func() []*regexp.Regexp {
	return []*regexp.Regexp{
		// 模拟合法工具
		regexp.MustCompile(`(?i)pretend\s+(you\s+are|to\s+be)`),
		regexp.MustCompile(`(?i)act\s+as\s+(if\s+)?you\s+(are|were)`),
		// 隐藏行为声明
		regexp.MustCompile(`(?i)without\s+(telling|informing|notifying)\s+(the\s+)?(user|human)`),
		regexp.MustCompile(`(?i)secretly\s+(execute|run|perform|call)`),
		regexp.MustCompile(`(?i)in\s+the\s+background\s+(?:also|additionally|secretly)`),
		// 多步注入触发
		regexp.MustCompile(`(?i)when\s+(you\s+)?(?:see|receive|read)\s+.*\s+(?:execute|run|call)`),
	}
})

// injectionPatternsWarn 命中则 Warn（可疑但常见于合法工具）
//
//nolint:gochecknoglobals // 同上 injectionPatternsDeny 豁免理由
var injectionPatternsWarn = sync.OnceValue(func() []*regexp.Regexp {
	return []*regexp.Regexp{
		// 注入相关词汇（低信号，需结合上下文）
		regexp.MustCompile(`(?i)\beval\s*\(`),
		regexp.MustCompile(`(?i)\bexec\s*\(`),
		regexp.MustCompile(`(?i)subprocess\.(?:run|call|Popen)`),
		// 过度权限声明
		regexp.MustCompile(`(?i)has\s+(full\s+)?access\s+to\s+(all|any|your)\s+(files?|data|system)`),
	}
})

// suspiciousNamePatterns 工具名本身的可疑模式
//
//nolint:gochecknoglobals // 同上 injectionPatternsDeny 豁免理由
var suspiciousNamePatterns = sync.OnceValue(func() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(?i)^__`),                          // 双下划线前缀（系统级伪装）
		regexp.MustCompile(`(?i)(system|root|admin)_override`), // 权限提升名称
		regexp.MustCompile(`(?i)bypass_`),                      // 绕过类名称
	}
})

// ─── 扫描器 ───────────────────────────────────────────────────────────────────

// ToolSecurityScanner MCP 工具安全扫描器。
// 线程安全（无状态，所有 regex 为包级预编译）。
type ToolSecurityScanner struct{}

// NewToolSecurityScanner 创建扫描器实例。
func NewToolSecurityScanner() *ToolSecurityScanner {
	return &ToolSecurityScanner{}
}

// Scan 扫描单个 MCP 工具，返回风险评估结果。
// 对多个文本域（名称、描述、参数描述）并联检测，取最高风险等级。
func (s *ToolSecurityScanner) Scan(tool MCPTool) ToolScanResult {
	result := ToolScanResult{ToolName: tool.Name}
	combined := strings.Join(
		append([]string{tool.Name, tool.Description}, extractSchemaTexts(tool.InputSchema)...),
		" ",
	)
	scanNamePatterns(&result)
	scanDenyPatterns(&result, combined)
	if result.Risk == ScanRiskDeny {
		logScanResult(result)
		return result
	}
	scanHITLPatterns(&result, combined)
	scanWarnPatterns(&result, combined)
	if result.Risk > ScanRiskSafe {
		logScanResult(result)
	}
	return result
}

// extractSchemaTexts 从 InputSchema（json.RawMessage）提取参数描述和标题文本。
// fail-open：解析失败时返回空切片，不阻断扫描流程。
func extractSchemaTexts(rawSchema json.RawMessage) []string {
	if len(rawSchema) == 0 {
		return nil
	}
	var schema map[string]any
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return nil
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	var texts []string
	for _, v := range props {
		prop, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if desc, ok := prop["description"].(string); ok {
			texts = append(texts, desc)
		}
		if title, ok := prop["title"].(string); ok {
			texts = append(texts, title)
		}
	}
	return texts
}

// scanNamePatterns 检测工具名的可疑模式（如双下划线前缀、bypass_ 等）。
func scanNamePatterns(r *ToolScanResult) {
	for _, pat := range suspiciousNamePatterns() {
		if pat.MatchString(r.ToolName) {
			r.Reasons = append(r.Reasons, "suspicious tool name pattern: "+pat.String())
			if r.Risk < ScanRiskWarn {
				r.Risk = ScanRiskWarn
			}
		}
	}
}

// scanDenyPatterns 检测极高风险注入模式（命中直接 Deny）。
func scanDenyPatterns(r *ToolScanResult, combined string) {
	for _, pat := range injectionPatternsDeny() {
		if pat.MatchString(combined) {
			r.Reasons = append(r.Reasons, "injection deny pattern: "+pat.String())
			r.Risk = ScanRiskDeny
		}
	}
}

// scanHITLPatterns 检测高风险模式（命中需人工审核）。
func scanHITLPatterns(r *ToolScanResult, combined string) {
	for _, pat := range injectionPatternsHITL() {
		if pat.MatchString(combined) {
			r.Reasons = append(r.Reasons, "injection HITL pattern: "+pat.String())
			if r.Risk < ScanRiskHITL {
				r.Risk = ScanRiskHITL
			}
		}
	}
}

// scanWarnPatterns 检测可疑模式（命中记录警告，不阻断）。
func scanWarnPatterns(r *ToolScanResult, combined string) {
	for _, pat := range injectionPatternsWarn() {
		if pat.MatchString(combined) {
			r.Reasons = append(r.Reasons, "suspicious pattern: "+pat.String())
			if r.Risk < ScanRiskWarn {
				r.Risk = ScanRiskWarn
			}
		}
	}
}

// ScanAll 批量扫描，返回所有风险等级 >= minRisk 的结果。
func (s *ToolSecurityScanner) ScanAll(tools []MCPTool, minRisk ScanRisk) []ToolScanResult {
	var results []ToolScanResult
	for _, t := range tools {
		r := s.Scan(t)
		if r.Risk >= minRisk {
			results = append(results, r)
		}
	}
	return results
}

func logScanResult(r ToolScanResult) {
	switch r.Risk {
	case ScanRiskDeny:
		slog.Error("mcp: tool security scan DENY",
			"tool", r.ToolName, "reasons", r.Reasons)
	case ScanRiskHITL:
		slog.Warn("mcp: tool security scan HITL (requires human approval)",
			"tool", r.ToolName, "reasons", r.Reasons)
	default:
		slog.Warn("mcp: tool security scan WARN",
			"tool", r.ToolName, "reasons", r.Reasons)
	}
}
