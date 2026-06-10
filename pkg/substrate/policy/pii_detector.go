package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// PIIDetector 个人信息检测器。
// Tier 0: Go 正则规则引擎（覆盖结构化 PII：邮箱/手机/身份证/信用卡/IP）。
// Tier 1+: Presidio HTTP sidecar（NER 级语义 PII，FeaturePresidioPII 门控）。
// 架构文档: docs/arch/M11-Policy-Safety-深度选型.md §5.1
type PIIDetector struct {
	rules          []*piiRule
	presidioClient *presidioClient // nil = Tier 0 only
}

type PIIMatch struct {
	Type  string // email | phone | id_card | credit_card | ip | custom
	Value string
	Start int
	End   int
	Score float64
}

type piiRule struct {
	name    string
	pattern *regexp.Regexp
	score   float64
}

// NewPIIDetector 构造 Tier 0 规则引擎（仅 Go 正则，无外部依赖）。
func NewPIIDetector() *PIIDetector {
	return &PIIDetector{rules: defaultPIIRules()}
}

// NewPIIDetectorWithPresidio 构造 Tier 1+ 检测器（规则引擎 + Presidio sidecar）。
// endpoint 为 Presidio Analyzer HTTP 地址（如 http://localhost:3000/analyze）。
func NewPIIDetectorWithPresidio(endpoint string, client *http.Client) *PIIDetector {
	d := NewPIIDetector()
	if endpoint != "" {
		d.presidioClient = &presidioClient{endpoint: endpoint, client: client}
	}
	return d
}

// Detect 检测文本中的 PII，返回所有命中项。
func (d *PIIDetector) Detect(ctx context.Context, text string) ([]PIIMatch, error) {
	// Tier 0: 正则规则引擎
	matches := d.detectByRules(text)

	// Tier 1+: Presidio（若已配置）
	if d.presidioClient != nil {
		presidioMatches, err := d.presidioClient.analyze(ctx, text)
		if err == nil {
			matches = mergePIIMatches(matches, presidioMatches)
		}
	}
	return matches, nil
}

// Redact 将文本中所有 PII 替换为 [REDACTED:<type>]。
// PIIMatch.Start/End 是 regexp.FindAllStringIndex 返回的字节偏移。
// 从后往前替换保证前面的偏移量在本次迭代结束前始终有效。
// 注意：替换操作在原始 string 字节层面进行，偏移来自正则匹配结果，天然是字节偏移，无需转换。
func (d *PIIDetector) Redact(ctx context.Context, text string) (string, int, error) {
	matches, err := d.Detect(ctx, text)
	if err != nil {
		return text, 0, err
	}
	if len(matches) == 0 {
		return text, 0, nil
	}
	// 从后往前替换：保持已处理片段前的字节偏移不变
	out := text
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		// 边界防御：来自 Presidio 的 Start/End 可能超出当前 out 长度（迭代中 out 长度会变化）
		// 但正则结果的偏移基于原始 text，从后往前替换时 m.End <= len(out) 始终成立
		if m.Start < 0 || m.End > len(out) || m.Start > m.End {
			continue
		}
		out = out[:m.Start] + "[REDACTED:" + m.Type + "]" + out[m.End:]
	}
	return out, len(matches), nil
}

// HasPII 快速判断是否含 PII（不返回详情）。
func (d *PIIDetector) HasPII(text string) bool {
	return len(d.detectByRules(text)) > 0
}

// ─── 规则引擎 ─────────────────────────────────────────────────────────────────

func (d *PIIDetector) detectByRules(text string) []PIIMatch {
	var matches []PIIMatch
	for _, rule := range d.rules {
		locs := rule.pattern.FindAllStringIndex(text, -1)
		for _, loc := range locs {
			matches = append(matches, PIIMatch{
				Type:  rule.name,
				Value: text[loc[0]:loc[1]],
				Start: loc[0],
				End:   loc[1],
				Score: rule.score,
			})
		}
	}
	return matches
}

func defaultPIIRules() []*piiRule {
	return []*piiRule{
		{
			name:    "email",
			pattern: regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
			score:   0.95,
		},
		{
			// 中国大陆手机号
			name:    "phone_cn",
			pattern: regexp.MustCompile(`(?:(?:\+?86)|0)?\s*1[3-9]\d{9}`),
			score:   0.90,
		},
		{
			// 国际通用电话（E.164 格式）
			name:    "phone_intl",
			pattern: regexp.MustCompile(`\+[1-9]\d{6,14}\b`),
			score:   0.80,
		},
		{
			// 中国居民身份证 18 位
			name:    "id_card_cn",
			pattern: regexp.MustCompile(`\b[1-9]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b`),
			score:   0.98,
		},
		{
			// 信用卡号（4 段 4 位，Visa/MasterCard/AmEx 等）
			name:    "credit_card",
			pattern: regexp.MustCompile(`\b(?:\d{4}[\s\-]?){3}\d{4}\b`),
			score:   0.85,
		},
		{
			// IPv4
			name:    "ipv4",
			pattern: regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\b`),
			score:   0.70,
		},
		{
			// API Key 通用模式（32-64 位 hex/base64 紧凑串）
			name:    "api_key",
			pattern: regexp.MustCompile(`(?i)(?:api[_\-]?key|token|secret|password|passwd|pwd)\s*[=:]\s*["']?([A-Za-z0-9+/=_\-]{20,64})["']?`),
			score:   0.88,
		},
		{
			// AWS Access Key
			name:    "aws_key",
			pattern: regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`),
			score:   0.99,
		},
	}
}

// mergePIIMatches 合并两组 PII 匹配，去重（按 Start+Type 去重）。
func mergePIIMatches(a, b []PIIMatch) []PIIMatch {
	seen := make(map[string]bool, len(a))
	for _, m := range a {
		seen[fmt.Sprintf("%d:%s", m.Start, m.Type)] = true
	}
	result := append([]PIIMatch{}, a...)
	for _, m := range b {
		key := fmt.Sprintf("%d:%s", m.Start, m.Type)
		if !seen[key] {
			seen[key] = true
			result = append(result, m)
		}
	}
	return result
}

// ─── Presidio HTTP 客户端 ─────────────────────────────────────────────────────

type presidioClient struct {
	endpoint string
	client   *http.Client
}

type presidioAnalyzeReq struct {
	Text     string   `json:"text"`
	Language string   `json:"language"`
	Entities []string `json:"entities,omitempty"`
}

type presidioResult struct {
	EntityType string  `json:"entity_type"`
	Start      int     `json:"start"`
	End        int     `json:"end"`
	Score      float64 `json:"score"`
}

func (pc *presidioClient) analyze(ctx context.Context, text string) ([]PIIMatch, error) {
	body, _ := json.Marshal(presidioAnalyzeReq{Text: text, Language: "zh"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pc.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := pc.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("presidio status %d: %s", resp.StatusCode, raw))
	}

	var results []presidioResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}

	matches := make([]PIIMatch, 0, len(results))
	for _, r := range results {
		if r.Start < 0 || r.End > len(text) {
			continue
		}
		matches = append(matches, PIIMatch{
			Type:  strings.ToLower(r.EntityType),
			Value: text[r.Start:r.End],
			Start: r.Start,
			End:   r.End,
			Score: r.Score,
		})
	}
	return matches, nil
}
