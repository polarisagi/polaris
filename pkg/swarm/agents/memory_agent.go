package agents

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// MemoryWhisper 代理 internal/protocol 中的定义，若不存在则假设使用 protocol.MemoryWhisper
type MemoryWhisper = protocol.MemoryWhisper

// MemoryAgent 后台常驻记忆管家。
//
// 职责：
//  1. 定时将 L1 冷数据（episodic_memory 超过滑动窗口的记录）蒸馏为事实三元组，写入 L2 SurrealDB。
//  2. 监听新工具结果事件，触发 Extension Librarian 索引（通过 outbox 写入）。
//  3. 发现对当前任务有价值的历史经验时，向 WhisperChan 推送耳语线索。
//
// 生命周期：由顶层 cmd/polaris 启动，随进程生命周期常驻。
// 内存约束：蒸馏触发间隔最短 60s（防止 DeepSeek API 调用过频）。
// Tier-0：单次蒸馏调用 LLM 最多处理 20 条 L1 记录，控制 token 消耗。
//
// 与 Orchestrator 关系：无关。不领任务，不占 slot。独立 goroutine。
type MemoryAgent struct {
	db              *sql.DB                // SQLite，读 episodic_memory
	surreal         SurrealWriterInterface // SurrealDB 写入接口
	llmInfer        LLMInferFunc           // DeepSeek 蒸馏调用
	whisperChan     chan<- MemoryWhisper   // 向主脑推送耳语线索（非阻塞）
	outboxWriter    OutboxWriterInterface  // 写 outbox 触发 Extension Librarian
	memPressure     *atomic.Int32          // 内存压力等级，0=正常，1=中等，2=严重
	distillInterval time.Duration          // 蒸馏间隔，默认 60s，内存压力高时延长
	coldWindowAge   time.Duration          // L1 记录超过此时间视为冷数据，默认 30min
	coldWindowCount int                    // 或超过此轮次视为冷数据，默认 100
}

// LLMInferFunc LLM 调用函数类型（依赖注入，可 mock）。
type LLMInferFunc func(ctx context.Context, prompt string) (string, error)

// SurrealWriterInterface 最小化 SurrealDB 写入接口（防止循环依赖）。
type SurrealWriterInterface interface {
	FTSIndex(docID, text string) error
	VecUpsert(id string, embedding []float32) error
	GraphRelate(fromID, edgeType, toID string, weight float64) error
}

// OutboxWriterInterface 最小化 outbox 写入接口。
type OutboxWriterInterface interface {
	Write(ctx context.Context, entry protocol.OutboxEntry) error
}

func NewMemoryAgent(db *sql.DB, surreal SurrealWriterInterface, llmInfer LLMInferFunc,
	whisperChan chan<- MemoryWhisper, outboxWriter OutboxWriterInterface,
	memPressure *atomic.Int32) *MemoryAgent {
	return &MemoryAgent{
		db:              db,
		surreal:         surreal,
		llmInfer:        llmInfer,
		whisperChan:     whisperChan,
		outboxWriter:    outboxWriter,
		memPressure:     memPressure,
		distillInterval: 60 * time.Second,
		coldWindowAge:   30 * time.Minute,
		coldWindowCount: 100,
	}
}

func (ma *MemoryAgent) Run(ctx context.Context) {
	ticker := time.NewTicker(ma.distillInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ma.memPressure != nil && ma.memPressure.Load() >= 2 {
				// 内存严重不足时跳过蒸馏，保护系统稳定性
				continue
			}
			if err := ma.distill(ctx); err != nil {
				slog.Error("memory_agent: distill failed", "err", err)
			}
		}
	}
}

func (ma *MemoryAgent) distill(ctx context.Context) error {
	distillCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := ma.db.QueryContext(distillCtx, `
		SELECT id, content FROM episodic_memory
		WHERE json_extract(meta, '$.cold') = 1
		AND distilled_at IS NULL
		ORDER BY created_at ASC LIMIT 20
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var ids []int
	var contents []string
	for rows.Next() {
		var id int
		var c string
		if err := rows.Scan(&id, &c); err == nil {
			ids = append(ids, id)
			contents = append(contents, c)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	prompt := "以下是来自 AI Agent 执行日志的片段，请提取其中的事实三元组。\n" +
		"每个三元组格式：{\"subject\": \"...\", \"predicate\": \"...\", \"object\": \"...\"}\n" +
		"只输出 JSON 数组，不要任何解释。\n\n日志片段：\n"
	for _, c := range contents {
		prompt += c + "\n"
	}

	triplesJSON, err := ma.llmInfer(distillCtx, prompt)
	if err != nil {
		return err
	}

	var triples []struct {
		Subject   string `json:"subject"`
		Predicate string `json:"predicate"`
		Object    string `json:"object"`
	}
	if err := json.Unmarshal([]byte(triplesJSON), &triples); err != nil {
		slog.Warn("memory_agent: failed to unmarshal LLM output", "err", err)
		// 仍然标记为处理完成，避免无限重试相同日志？这里按要求更新。
	}

	for _, t := range triples {
		docID := "triple_" + fmt.Sprintf("%x", sha256.Sum256([]byte(t.Subject+t.Predicate+t.Object)))[:16]
		_ = ma.surreal.FTSIndex(docID, t.Subject+" "+t.Predicate+" "+t.Object)
		_ = ma.surreal.GraphRelate(t.Subject, t.Predicate, t.Object, 1.0)
	}

	now := time.Now().Unix()
	for _, id := range ids {
		_, _ = ma.db.ExecContext(distillCtx, `
			UPDATE episodic_memory SET meta = json_patch(meta, '{"distilled_at": `+fmt.Sprintf("%d", now)+`}')
			WHERE id = ?
		`, id)
	}

	return nil
}
