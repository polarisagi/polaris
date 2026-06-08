package governance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// ============================================================================
// 轨迹压缩器：训练数据飞轮
// 参照: hermes-agent/trajectory_compressor.py
// 架构文档: docs/arch/12-Eval-Harness-深度选型.md §5（Training Data Pipeline）
//
// 压缩策略：
//   保护头部（首个 llm_request/response 对，bootstrap 上下文不可丢）
//   保护尾部（最后 ProtectLastN 个有效 turn，结论不可丢）
//   压缩中段（替换为单条 compression_summary 事件）
//
// Token 估算：len(Data)/4（无外部 tokenizer 依赖，Tier-0 友好）
// ============================================================================

// CompressConfig 轨迹压缩配置。
type CompressConfig struct {
	// TargetTokenBudget 目标 token 上限。超出后触发压缩。
	TargetTokenBudget int
	// ProtectLastN 保护尾部 N 个有效 turn（llm_request/response 对）。
	ProtectLastN int
}

func defaultCompressConfig(cfg CompressConfig) CompressConfig {
	if cfg.TargetTokenBudget == 0 {
		cfg.TargetTokenBudget = 15000
	}
	if cfg.ProtectLastN == 0 {
		cfg.ProtectLastN = 4
	}
	return cfg
}

// CompressResult 压缩统计。
type CompressResult struct {
	OriginalEvents   int
	CompressedEvents int
	// 估算值：bytes/4
	OriginalTokens   int
	CompressedTokens int
	// Skipped = true 表示未超预算，原样输出。
	Skipped bool
}

// TrajectoryCompressor 对已录制轨迹进行有损压缩，保留训练信号。
type TrajectoryCompressor struct {
	cfg CompressConfig
}

func NewTrajectoryCompressor(cfg CompressConfig) *TrajectoryCompressor {
	return &TrajectoryCompressor{cfg: defaultCompressConfig(cfg)}
}

// Compress 压缩事件序列。原样事件不被修改；返回新切片。
func (c *TrajectoryCompressor) Compress(events []TrajectoryEvent) ([]TrajectoryEvent, CompressResult) {
	origTokens := estimateTokens(events)
	result := CompressResult{
		OriginalEvents: len(events),
		OriginalTokens: origTokens,
	}

	if origTokens <= c.cfg.TargetTokenBudget {
		result.Skipped = true
		result.CompressedEvents = len(events)
		result.CompressedTokens = origTokens
		return events, result
	}

	turns := groupIntoTurns(events)
	if len(turns) <= 2 {
		// 不足以拆分头/尾，不压缩
		result.Skipped = true
		result.CompressedEvents = len(events)
		result.CompressedTokens = origTokens
		return events, result
	}

	headTurns := turns[:1]
	tailStart := max(1, len(turns)-c.cfg.ProtectLastN)
	middleTurns := turns[1:tailStart]
	tailTurns := turns[tailStart:]

	var out []TrajectoryEvent
	for _, t := range headTurns {
		out = append(out, t...)
	}

	if len(middleTurns) > 0 {
		midEvents := 0
		for _, t := range middleTurns {
			midEvents += len(t)
		}
		placeholder, _ := buildSummaryEvent(midEvents, estimateTokensFromTurns(middleTurns))
		if placeholder != nil {
			out = append(out, *placeholder)
		}
	}

	for _, t := range tailTurns {
		out = append(out, t...)
	}

	compTokens := estimateTokens(out)
	result.CompressedEvents = len(out)
	result.CompressedTokens = compTokens
	return out, result
}

// ExportTrainingJSONL 将（已压缩的）事件序列导出为 OpenAI fine-tuning JSONL 格式。
// 每行是一个独立 {"messages": [...]} 对象，可直接提交 OpenAI / DeepSeek fine-tuning API。
func (c *TrajectoryCompressor) ExportTrainingJSONL(events []TrajectoryEvent, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "compressor: mkdir failed", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "compressor: create output failed", err)
	}
	defer f.Close()

	msgs := eventsToTrainingMessages(events)
	if len(msgs) == 0 {
		return nil
	}

	line := map[string]any{"messages": msgs}
	b, err := json.Marshal(line)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "compressor: marshal training record failed", err)
	}
	_, err = fmt.Fprintf(f, "%s\n", b)
	return err
}

// CompressFile 读取 JSONL 轨迹文件（每行一个 TrajectoryEvent），
// 压缩后写入 outputPath，返回统计。
func (c *TrajectoryCompressor) CompressFile(inputPath, outputPath string) (CompressResult, error) {
	events, err := loadTrajectoryJSONL(inputPath)
	if err != nil {
		return CompressResult{}, err
	}

	compressed, result := c.Compress(events)

	if err := writeTrajectoryJSONL(compressed, outputPath); err != nil {
		return result, err
	}
	return result, nil
}

// ─── 内部工具 ─────────────────────────────────────────────────────────────────

// turn 是一组逻辑相关的事件（一个 LLM 交互周期）。
type turn []TrajectoryEvent

// groupIntoTurns 将扁平事件序列按 llm_request 边界切成 turn 列表。
// 每个 turn 从一个 llm_request 开始，包含其后紧随的 llm_response 和 tool_call/tool_result 对。
func groupIntoTurns(events []TrajectoryEvent) []turn {
	var turns []turn
	var cur turn //nolint:prealloc

	for _, ev := range events {
		if ev.Type == "llm_request" && len(cur) > 0 {
			turns = append(turns, cur)
			cur = nil
		}
		cur = append(cur, ev)
	}
	if len(cur) > 0 {
		turns = append(turns, cur)
	}
	return turns
}

func estimateTokens(events []TrajectoryEvent) int {
	total := 0
	for _, ev := range events {
		total += len(ev.Data)
	}
	return total / 4 // bytes/4 ≈ tokens（BPE 粗估）
}

func estimateTokensFromTurns(turns []turn) int {
	total := 0
	for _, t := range turns {
		total += estimateTokens(t)
	}
	return total
}

// compressionSummaryData summary 占位事件的 Data 内容。
type compressionSummaryData struct {
	OriginalEventCount int    `json:"original_event_count"`
	EstimatedTokens    int    `json:"estimated_tokens"`
	Notice             string `json:"notice"`
}

func buildSummaryEvent(eventCount, tokens int) (*TrajectoryEvent, error) {
	d := compressionSummaryData{
		OriginalEventCount: eventCount,
		EstimatedTokens:    tokens,
		Notice:             fmt.Sprintf("[COMPRESSED: %d events (~%d tokens) summarized to fit context budget]", eventCount, tokens),
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	return &TrajectoryEvent{
		Type: "compression_summary",
		Data: b,
	}, nil
}

// trainingMessage OpenAI 兼容消息格式。
type trainingMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// eventsToTrainingMessages 将事件序列转换为 OpenAI messages 数组。
// 最终格式：[system?, user*, assistant*, tool*...]
func eventsToTrainingMessages(events []TrajectoryEvent) []trainingMessage {
	var msgs []trainingMessage

	for _, ev := range events {
		switch ev.Type {
		case "llm_request":
			// 尝试提取最后一条 user 消息
			var req struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if json.Unmarshal(ev.Data, &req) == nil {
				for _, m := range req.Messages {
					if m.Role == "system" || m.Role == "user" {
						msgs = append(msgs, trainingMessage{Role: m.Role, Content: m.Content})
					}
				}
			}

		case "llm_response":
			var resp struct {
				Content string `json:"content"`
			}
			content := string(ev.Data)
			if json.Unmarshal(ev.Data, &resp) == nil && resp.Content != "" {
				content = resp.Content
			}
			msgs = append(msgs, trainingMessage{Role: "assistant", Content: content})

		case "tool_result":
			var res struct {
				Output string `json:"output"`
			}
			content := string(ev.Data)
			if json.Unmarshal(ev.Data, &res) == nil && res.Output != "" {
				content = res.Output
			}
			msgs = append(msgs, trainingMessage{Role: "tool", Content: content})

		case "compression_summary":
			var s compressionSummaryData
			if json.Unmarshal(ev.Data, &s) == nil {
				msgs = append(msgs, trainingMessage{Role: "user", Content: s.Notice})
			}
		}
	}
	return msgs
}

func loadTrajectoryJSONL(path string) ([]TrajectoryEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "compressor: open trajectory file failed", err)
	}
	defer f.Close()

	var events []TrajectoryEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024) // 4MB 行缓冲
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev TrajectoryEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "compressor: parse event failed", err)
		}
		events = append(events, ev)
	}
	return events, scanner.Err()
}

func writeTrajectoryJSONL(events []TrajectoryEvent, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "compressor: mkdir failed", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "compressor: create output failed", err)
	}
	defer f.Close()

	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal, "compressor: marshal event failed", err)
		}
		if _, err := fmt.Fprintf(f, "%s\n", b); err != nil {
			return perrors.Wrap(perrors.CodeInternal, "compressor: write event failed", err)
		}
	}
	return nil
}
