package agents

import (
	"context"
	"database/sql"
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
			// Check mem pressure
			if ma.memPressure != nil && ma.memPressure.Load() >= 2 { // MemPressureCritical
				// skip distillation
				continue
			}
			_ = ma.distill(ctx)
		}
	}
}

func (ma *MemoryAgent) distill(ctx context.Context) error {
	// 找到需要降维/蒸馏的冷记录
	cutoff := time.Now().Add(-ma.coldWindowAge).Unix()

	rows, err := ma.db.QueryContext(ctx, `
		SELECT id, content FROM episodic_events
		WHERE cold = 0 AND timestamp < ?
		ORDER BY timestamp ASC LIMIT 20
	`, cutoff)
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

	// 模拟将 contents 合并发给 LLM 蒸馏
	// 在实际实现中，会提取成 json，包含 (sub, pred, obj)
	prompt := "Extract factual triples from the following memory:\n"
	for _, c := range contents {
		prompt += c + "\n"
	}

	triplesJSON, err := ma.llmInfer(ctx, prompt)
	if err == nil && len(triplesJSON) > 0 {
		// 假装写入 SurrealDB
		_ = ma.surreal.FTSIndex("distilled_memory", triplesJSON)
	}

	// 更新为 cold = 1
	for _, id := range ids {
		_, _ = ma.db.ExecContext(ctx, "UPDATE episodic_events SET cold = 1 WHERE id = ?", id)
	}

	return nil
}
