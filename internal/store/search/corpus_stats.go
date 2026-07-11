package search

import (
	"context"
	"database/sql"
	"math"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// CorpusStats BM25 语料库统计（docCount/totalLen/termDocFreq），供 bm25Score 计算 IDF/AvgDocLen。
// 从 hybrid_retrieve.go 拆分（R7 文件行数上限修复：新增 ExplainBits 归因逻辑后原文件超过
// 400 行阈值，按职责边界拆出 CorpusStats 独立文件，不改变任何行为）。
type CorpusStats struct {
	mu          sync.RWMutex
	docCount    int
	totalLen    int
	termDocFreq map[string]int
	// [Task 18] dirty 标记：有更新时置位，FlushTo 落盘后清零。
	// 使用 bool 而非 atomic 以复用现有 mu 锁，保持一致性。
	dirty bool
}

func NewCorpusStats() *CorpusStats {
	return &CorpusStats{
		termDocFreq: make(map[string]int),
	}
}

func (cs *CorpusStats) AvgDocLen() float64 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.docCount == 0 {
		return 100.0
	}
	return float64(cs.totalLen) / float64(cs.docCount)
}

func (cs *CorpusStats) IDF(term string) float64 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	df := cs.termDocFreq[term]
	if df == 0 {
		return 1.5
	}
	return math.Log(float64(cs.docCount-df+1)/float64(df+1) + 1.0)
}

func (cs *CorpusStats) AddDoc(terms []string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.docCount++
	cs.totalLen += len(terms)
	seen := make(map[string]bool, len(terms))
	for _, t := range terms {
		if !seen[t] {
			cs.termDocFreq[t]++
			seen[t] = true
		}
	}
	cs.dirty = true // [Task 18] 标记需要落盘
}

// RestoreStatsFromDB 从 corpus_stats 表加载持久化状态（Task 18）。
// 必须在引擎提供服务前调用，以保证 IDF 统计的连续性。
func (cs *CorpusStats) RestoreStatsFromDB(ctx context.Context, db interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}) error {
	rows, err := db.QueryContext(ctx, `SELECT term, doc_freq, doc_count, total_len FROM corpus_stats`)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CorpusStats.RestoreStatsFromDB", err)
	}
	defer rows.Close()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for rows.Next() {
		var term string
		var docFreq, docCount, totalLen int
		if err := rows.Scan(&term, &docFreq, &docCount, &totalLen); err != nil {
			continue
		}
		if term == "" {
			// 全局统计行
			cs.docCount = docCount
			cs.totalLen = totalLen
		} else {
			cs.termDocFreq[term] = docFreq
		}
	}
	cs.dirty = false
	if err := rows.Err(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CorpusStats.RestoreStatsFromDB: rows iteration", err)
	}
	return nil
}

// FlushTo 将内存中 dirty 的统计增量写入 corpus_stats（Task 18）。
// 使用 INSERT OR REPLACE 语义，幂等安全。
// 建议由后台 goroutine 定期调用（复用 DatabaseWriter ticker 模式），不要在检索热路径上同步调用。
func (cs *CorpusStats) FlushTo(ctx context.Context, db interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}) error {
	cs.mu.RLock()
	if !cs.dirty {
		cs.mu.RUnlock()
		return nil
	}
	// 快照当前状态，尽快释放读锁，不阻塞检索热路径
	docCount := cs.docCount
	totalLen := cs.totalLen
	snapshot := make(map[string]int, len(cs.termDocFreq))
	for k, v := range cs.termDocFreq {
		snapshot[k] = v
	}
	cs.mu.RUnlock()

	// 写入全局统计行（term=''）
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO corpus_stats (term, doc_freq, doc_count, total_len, updated_at)
		 VALUES ('', 0, ?, ?, unixepoch())`, docCount, totalLen); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CorpusStats.FlushTo: global row", err)
	}
	// 写入各词条行
	for term, freq := range snapshot {
		if _, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO corpus_stats (term, doc_freq, doc_count, total_len, updated_at)
			 VALUES (?, ?, 0, 0, unixepoch())`, term, freq); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "CorpusStats.FlushTo: term row", err)
		}
	}
	// 清除 dirty 标记
	cs.mu.Lock()
	cs.dirty = false
	cs.mu.Unlock()
	return nil
}
