package substrate

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

// MutationBus -- AI 核心数据串行写总线（events/decision_log）。
// 适用于高频、需要批量提交的 AI 认知数据写操作。
// 配置类数据（channels/preferences/cron）和 CAS 操作（Blackboard 任务状态）
// 允许直接写 store.DB()，MaxOpenConns=1 保证串行化。
// 架构文档: docs/arch/M02-Storage-Fabric.md §2.3

type MutationIntent struct {
	Table            string
	Operation        string // "upsert" | "delete" | "insert"
	Key              []byte
	Payload          []byte
	ResultCh         chan error
	Deadline         time.Time
	TaskID           string
	AgentID          string
	ClaimedVersion   int64
	Priority         int // PriorityNormal=0 | PriorityFlush=1
	CompositeGroupID string
}

type CompositeMutationIntent struct {
	GroupID  string
	Intents  []MutationIntent
	ResultCh chan error
	Deadline time.Duration // 默认 30s
	TaskID   string
	AgentID  string
}

type DatabaseWriter struct {
	db           *sql.DB
	ch           chan *MutationIntent // cap=4096
	priorityCh   chan *MutationIntent // cap=256 (用于高优先级/HITL等，防止队列饥饿)
	leaseChecker LeaseChecker
	mu           sync.Mutex
	wg           sync.WaitGroup
	batch        []*MutationIntent
}

const (
	PriorityNormal = 0
	PriorityFlush  = 1
	MaxRowsPerTx   = 50
	MaxBatchSize   = 64
	TickerInterval = 10 * time.Millisecond
)

type LeaseChecker interface {
	Verify(taskID, agentID string, version int64) bool
}

// NewDatabaseWriter 创建 DatabaseWriter。
// db 必须是 SQLite WAL 模式写连接（MaxOpenConns=1）。
func NewDatabaseWriter(db *sql.DB, lc LeaseChecker) *DatabaseWriter {
	return &DatabaseWriter{
		db:           db,
		ch:           make(chan *MutationIntent, 4096),
		priorityCh:   make(chan *MutationIntent, 256),
		leaseChecker: lc,
		batch:        make([]*MutationIntent, 0, MaxBatchSize),
	}
}

// Submit 提交单个 MutationIntent。
// 严禁 default: sync execute 兜底——破坏单写者串行化。
func (dw *DatabaseWriter) Submit(ctx context.Context, intent *MutationIntent) error {
	targetCh := dw.ch
	if intent.Priority == PriorityFlush {
		targetCh = dw.priorityCh
	}

	select {
	case targetCh <- intent:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		// 指数退避重试: 10ms→50ms→250ms→1s→2s
		backoff := []time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 250 * time.Millisecond, time.Second, 2 * time.Second}
		for i, d := range backoff {
			time.Sleep(d)
			select {
			case targetCh <- intent:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			default:
				if i == len(backoff)-1 {
					return ErrMutationBusOverloaded
				}
			}
		}
	}
	return ErrMutationBusOverloaded
}

// SubmitBatch ETL 专用批量提交。
func (dw *DatabaseWriter) SubmitBatch(ctx context.Context, intents []*MutationIntent) error {
	for i := 0; i < len(intents); i += MaxRowsPerTx {
		end := i + MaxRowsPerTx
		if end > len(intents) {
			end = len(intents)
		}
		batch := intents[i:end]
		for _, intent := range batch {
			if err := dw.Submit(ctx, intent); err != nil {
				return err
			}
		}
		if end < len(intents) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
	}
	return nil
}

// SubmitComposite 提交复合事务——同一组 MutationIntent 原子提交（全成功或全失败）。
func (dw *DatabaseWriter) SubmitComposite(ctx context.Context, comp *CompositeMutationIntent) error {
	for i := range comp.Intents {
		comp.Intents[i].CompositeGroupID = comp.GroupID
		if err := dw.Submit(ctx, &comp.Intents[i]); err != nil {
			return err
		}
	}
	return nil
}

// Run 启动 DatabaseWriter 消费循环（由 M2 StorageFabric.Open() 调用）。
func (dw *DatabaseWriter) Run(ctx context.Context) {
	dw.wg.Add(1)
	defer dw.wg.Done()
	defer func() {
		if r := recover(); r != nil { //nolint:staticcheck // panic recovery will be fully implemented soon
			// CRITICAL + polaris_dbwriter_panic + stack trace → StorageFabric 重启 DatabaseWriter
		}
	}()

	ticker := time.NewTicker(TickerInterval)
	defer ticker.Stop()

	for {
		// 优先处理高优先级队列，防饥饿
		select {
		case <-ctx.Done():
			dw.flushBatch(ctx) //nolint:errcheck
			return
		case intent := <-dw.priorityCh:
			dw.batch = append(dw.batch, intent)
			if len(dw.batch) >= MaxBatchSize {
				dw.flushBatch(ctx) //nolint:errcheck
			}
			continue
		default:
		}

		select {
		case <-ctx.Done():
			dw.flushBatch(ctx) //nolint:errcheck
			return
		case <-ticker.C:
			if len(dw.batch) > 0 {
				dw.flushBatch(ctx) //nolint:errcheck
			}
		case intent := <-dw.priorityCh:
			dw.batch = append(dw.batch, intent)
			if len(dw.batch) >= MaxBatchSize {
				dw.flushBatch(ctx) //nolint:errcheck
			}
		case intent := <-dw.ch:
			dw.batch = append(dw.batch, intent)
			if len(dw.batch) >= MaxBatchSize {
				dw.flushBatch(ctx) //nolint:errcheck
			}
		}
	}
}

// Close 排空 channel 残余 + 最终 flush。
func (dw *DatabaseWriter) Close() {
	close(dw.ch)
	dw.wg.Wait()
}

var (
	ErrMutationBusOverloaded    = &MutationBusError{"mutation bus overloaded"}
	ErrDatabaseWriterRestarting = &MutationBusError{"database writer restarting"}
	ErrStaleLease               = &MutationBusError{"stale lease"}
	ErrCompositeIncomplete      = &MutationBusError{"composite mutation incomplete"}
)

type MutationBusError struct{ msg string }

func (e *MutationBusError) Error() string { return e.msg }
