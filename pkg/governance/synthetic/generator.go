package synthetic

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	perrors "github.com/polarisagi/polaris/internal/errors"

	"github.com/polarisagi/polaris/internal/protocol"
)

// QuestionType 问题类型分类（对齐 RAGAS / Giskard RAGET）。
type QuestionType string

const (
	QTypeFactual        QuestionType = "factual"        // 单跳，答案在单一 chunk
	QTypeMultiHop       QuestionType = "multi_hop"      // 需跨 2-3 chunk 聚合
	QTypeAbstractive    QuestionType = "abstractive"    // 需归纳/比较多文档
	QTypeCounterfactual QuestionType = "counterfactual" // 反事实，答案在语料中被否定
)

// DifficultyLevel 难度分级。
type DifficultyLevel string

const (
	DiffEasy   DifficultyLevel = "easy"   // 单跳，答案为连续 span
	DiffMedium DifficultyLevel = "medium" // 多跳或条件推理
	DiffHard   DifficultyLevel = "hard"   // 反事实 / 干扰项 / 抽象归纳
)

// SyntheticCase 合成评测用例，比 EvalCase 携带更多生成元数据。
// 可通过调用方的适配器转为 eval.EvalCase 注入评测套件。
type SyntheticCase struct {
	ID           string          `json:"id"`
	Question     string          `json:"question"`
	GroundTruth  string          `json:"ground_truth"`
	ChunkID      string          `json:"chunk_id"` // 来源 chunk 哈希
	Type         QuestionType    `json:"type"`
	Difficulty   DifficultyLevel `json:"difficulty"`
	ContextBound bool            `json:"context_bound"` // 仅凭 chunk 可回答（防污染）
}

// EvalGenerator 从知识库 chunks 生成合成评测用例。
// 实现 RAGAS Evolution 三阶段：Simple → Reasoning/Conditioning → Groundedness 验证。
//
// 调用时机：M9 SelfImprovement 离线批处理，禁止在 RunSuite 热路径中调用。
type EvalGenerator struct {
	Enabled     bool
	TargetRatio float64 // 每批生成比例，e.g. 0.05 = 每 100 chunks 生成 5 条
	provider    protocol.Provider
}

// NewEvalGenerator 构造 EvalGenerator。provider 用于 LLM 批量生成，必须注入。
func NewEvalGenerator(enabled bool, provider protocol.Provider) *EvalGenerator {
	return &EvalGenerator{
		Enabled:     enabled,
		TargetRatio: 0.05,
		provider:    provider,
	}
}

// GenerateCases 对传入的 chunks 批量生成合成评测用例。
// 三阶段流水线：
//  1. Simple 生成：从单 chunk 生成基础 factual QA
//  2. Evolution：按 TargetRatio 抽样做难度演化（Reasoning / Conditioning）
//  3. Groundedness 验证：judge LLM 过滤 context-unbound 问题
func (g *EvalGenerator) GenerateCases(ctx context.Context, chunks []string) ([]SyntheticCase, error) { //nolint:gocyclo
	if !g.Enabled || g.provider == nil || len(chunks) == 0 {
		return nil, nil
	}

	target := max(1, int(float64(len(chunks))*g.TargetRatio))
	cases := make([]SyntheticCase, 0, target)
	seen := make(map[uint64]struct{}) // n-gram 去重指纹

	for i, chunk := range chunks {
		if len(cases) >= target {
			break
		}
		if chunk == "" {
			continue
		}

		// Stage 1: 生成 Simple factual QA
		base, err := g.generateSimple(ctx, chunk)
		if err != nil || base == nil {
			continue
		}
		base.ChunkID = chunkID(chunk)

		// Stage 2: 按索引奇偶决定是否演化难度（简单分流，避免全量调用 LLM）
		if i%3 == 1 {
			if evolved, err := g.evolveReasoning(ctx, chunk, base); err == nil && evolved != nil {
				base = evolved
			}
		} else if i%3 == 2 {
			if evolved, err := g.evolveConditioning(ctx, chunk, base); err == nil && evolved != nil {
				base = evolved
			}
		}

		// Stage 3: Groundedness 验证（答案必须能从 chunk 中找到依据）
		if grounded, err := g.validateGroundedness(ctx, chunk, base); err != nil || !grounded {
			continue
		}
		base.ContextBound = true

		// n-gram 去重（3-gram 指纹防测试集污染）
		fp := ngramFingerprint(base.Question, 3)
		if _, dup := seen[fp]; dup {
			continue
		}
		seen[fp] = struct{}{}

		cases = append(cases, *base)
	}

	return cases, nil
}

// ── Stage 1 ──────────────────────────────────────────────────────────────────

type simpleQA struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

func (g *EvalGenerator) generateSimple(ctx context.Context, chunk string) (*SyntheticCase, error) {
	prompt := fmt.Sprintf(
		"根据以下文本生成一个事实性问答对。问题必须仅凭该文本可回答（不依赖外部知识）。\n\n"+
			"文本：\n%s\n\n"+
			"输出 JSON，字段：question（问题）、answer（答案，来自文本原句或合理摘要）",
		truncate(chunk, 1500),
	)

	out, err := g.infer(ctx, prompt, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string"},
			"answer":   map[string]any{"type": "string"},
		},
		"required": []string{"question", "answer"},
	})
	if err != nil {
		return nil, err
	}

	var qa simpleQA
	if err := json.Unmarshal(out, &qa); err != nil || qa.Question == "" {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("synthetic: parse simple QA: %v", err), err)
	}
	return &SyntheticCase{
		ID:          caseID(qa.Question),
		Question:    qa.Question,
		GroundTruth: qa.Answer,
		Type:        QTypeFactual,
		Difficulty:  DiffEasy,
	}, nil
}

// ── Stage 2a：Reasoning Evolution（提升为 multi-hop）────────────────────────

func (g *EvalGenerator) evolveReasoning(ctx context.Context, chunk string, base *SyntheticCase) (*SyntheticCase, error) {
	prompt := fmt.Sprintf(
		"将以下简单问题改写为需要多步推理才能回答的复杂问题，答案仍必须能从文本中找到依据。\n\n"+
			"原始问题：%s\n原始答案：%s\n文本：\n%s\n\n"+
			"输出 JSON：question（改写后的复杂问题）、answer（答案）",
		base.Question, base.GroundTruth, truncate(chunk, 1200),
	)

	out, err := g.infer(ctx, prompt, map[string]any{
		"type":       "object",
		"properties": map[string]any{"question": map[string]any{"type": "string"}, "answer": map[string]any{"type": "string"}},
		"required":   []string{"question", "answer"},
	})
	if err != nil {
		return nil, err
	}

	var qa simpleQA
	if err := json.Unmarshal(out, &qa); err != nil || qa.Question == "" {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("synthetic: parse reasoning QA: %v", err), err)
	}
	return &SyntheticCase{
		ID:          caseID(qa.Question),
		Question:    qa.Question,
		GroundTruth: qa.Answer,
		ChunkID:     base.ChunkID,
		Type:        QTypeMultiHop,
		Difficulty:  DiffMedium,
	}, nil
}

// ── Stage 2b：Conditioning Evolution（加限定条件，提升为 Hard）───────────────

func (g *EvalGenerator) evolveConditioning(ctx context.Context, chunk string, base *SyntheticCase) (*SyntheticCase, error) {
	prompt := fmt.Sprintf(
		"将以下问题改写为带有反事实或限定条件的问题"+
			"（如：如果X不成立那么会怎样、在Y条件下结果如何）。"+
			"改写后的问题可以没有明确答案，ground truth 为文本中与该假设相关的事实陈述。\n\n"+
			"原始问题：%s\n文本：\n%s\n\n"+
			"输出 JSON：question、answer",
		base.Question, truncate(chunk, 1200),
	)

	out, err := g.infer(ctx, prompt, map[string]any{
		"type":       "object",
		"properties": map[string]any{"question": map[string]any{"type": "string"}, "answer": map[string]any{"type": "string"}},
		"required":   []string{"question", "answer"},
	})
	if err != nil {
		return nil, err
	}

	var qa simpleQA
	if err := json.Unmarshal(out, &qa); err != nil || qa.Question == "" {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("synthetic: parse conditioning QA: %v", err), err)
	}
	return &SyntheticCase{
		ID:          caseID(qa.Question),
		Question:    qa.Question,
		GroundTruth: qa.Answer,
		ChunkID:     base.ChunkID,
		Type:        QTypeCounterfactual,
		Difficulty:  DiffHard,
	}, nil
}

// ── Stage 3：Groundedness 验证 ───────────────────────────────────────────────

type groundednessOutput struct {
	Grounded bool   `json:"grounded"`
	Reason   string `json:"reason"`
}

func (g *EvalGenerator) validateGroundedness(ctx context.Context, chunk string, c *SyntheticCase) (bool, error) {
	prompt := fmt.Sprintf(
		"判断以下问题的答案是否能从给定文本中找到依据（不依赖外部知识）。\n\n"+
			"文本：\n%s\n\n问题：%s\n答案：%s\n\n"+
			"输出 JSON：grounded（true/false）、reason（简短说明）",
		truncate(chunk, 1200), c.Question, c.GroundTruth,
	)

	out, err := g.infer(ctx, prompt, map[string]any{
		"type":       "object",
		"properties": map[string]any{"grounded": map[string]any{"type": "boolean"}, "reason": map[string]any{"type": "string"}},
		"required":   []string{"grounded"},
	})
	if err != nil {
		return false, err
	}

	var result groundednessOutput
	if err := json.Unmarshal(out, &result); err != nil {
		return false, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("synthetic: parse groundedness: %v", err), err)
	}
	return result.Grounded, nil
}

// ── 内部工具 ─────────────────────────────────────────────────────────────────

func (g *EvalGenerator) infer(ctx context.Context, prompt string, schema map[string]any) ([]byte, error) {
	resp, err := g.provider.Infer(ctx, []protocol.Message{{Role: "user", Content: prompt}},
		protocol.WithModel("deepseek-chat"), // budget 层：批量生成无需推理能力
		protocol.WithMaxTokens(512),
		protocol.WithResponseFormat(&protocol.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: schema,
		}),
	)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("synthetic: infer: %v", err), err)
	}
	return []byte(resp.Content), nil
}

// chunkID 以 SHA-256 前 8 字节作为 chunk 指纹。
func chunkID(chunk string) string {
	h := sha256.Sum256([]byte(chunk))
	return fmt.Sprintf("%x", h[:8])
}

// caseID 以问题文本的 SHA-256 前 6 字节作为用例 ID。
func caseID(question string) string {
	h := sha256.Sum256([]byte(question))
	return fmt.Sprintf("syn_%x", h[:6])
}

// ngramFingerprint 计算文本的 n-gram 哈希指纹（用于去重）。
func ngramFingerprint(text string, n int) uint64 {
	words := strings.Fields(strings.ToLower(text))
	if len(words) < n {
		h := sha256.Sum256([]byte(text))
		return uint64(h[0])<<56 | uint64(h[1])<<48 | uint64(h[2])<<40 | uint64(h[3])<<32
	}
	// 取前 n 个词的联合哈希作为指纹
	key := strings.Join(words[:n], " ")
	h := sha256.Sum256([]byte(key))
	return uint64(h[0])<<56 | uint64(h[1])<<48 | uint64(h[2])<<40 | uint64(h[3])<<32 |
		uint64(h[4])<<24 | uint64(h[5])<<16 | uint64(h[6])<<8 | uint64(h[7])
}

// truncate 截断字符串到指定字节长度，防止超出 LLM context 窗口。
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "…"
}
