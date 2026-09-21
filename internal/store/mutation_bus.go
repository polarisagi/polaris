package store

import (
	"context"
	"database/sql"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
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
	onPanic      func(err interface{}, stack []byte)

	// closeMu 保护 closed 与 channel 关闭的原子性：Submit 在 RLock 下检查 closed 后再发送，
	// Close 在 Lock 下置位并关闭 channel——否则停机窗口内仍在提交的生产者会
	// "send on closed channel" panic（GR-3-001 停机序列修复的前提）。
	closeMu sync.RWMutex
	closed  bool
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
// 退避等待使用 time.NewTimer + ctx.Done()，保证 context 取消可立即返回，不阻塞调用方 goroutine。
func (dw *DatabaseWriter) Submit(ctx context.Context, intent *MutationIntent) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
	}
	if sent, err := dw.trySend(intent); sent || err != nil {
		return err
	}

	// 指数退避重试: 10ms→50ms→250ms→1s→2s
	// 每次等待均响应 ctx.Done()，避免 time.Sleep 阻塞调用方 goroutine
	backoff := []time.Duration{
		10 * time.Millisecond,
		50 * time.Millisecond,
		250 * time.Millisecond,
		time.Second,
		2 * time.Second,
	}
	for i, d := range backoff {
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err() //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
		case <-timer.C:
		}
		if sent, err := dw.trySend(intent); sent || err != nil {
			return err
		}
		if i == len(backoff)-1 {
			return ErrMutationBusOverloaded
		}
	}
	return ErrMutationBusOverloaded
}

// trySend 非阻塞投递。RLock 只覆盖一次 select，不跨退避等待，避免 Close 被长时间挡住。
func (dw *DatabaseWriter) trySend(intent *MutationIntent) (sent bool, err error) {
	dw.closeMu.RLock()
	defer dw.closeMu.RUnlock()
	if dw.closed {
		return false, ErrDatabaseWriterClosed
	}
	targetCh := dw.ch
	if intent.Priority == PriorityFlush {
		targetCh = dw.priorityCh
	}
	select {
	case targetCh <- intent:
		return true, nil
	default:
		return false, nil
	}
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
				return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.SubmitBatch", err)
			}
		}
		if end < len(intents) {
			select {
			case <-ctx.Done():
				return ctx.Err() //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
			default:
			}
			runtime.Gosched()
		}
	}
	return nil
}

// SubmitComposite 提交复合事务——同一组 MutationIntent 原子提交（全成功或全失败）。
func (dw *DatabaseWriter) SubmitComposite(ctx context.Context, comp *CompositeMutationIntent) error {
	for i := range comp.Intents {
		comp.Intents[i].CompositeGroupID = comp.GroupID
		if err := dw.Submit(ctx, &comp.Intents[i]); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.SubmitComposite", err)
		}
	}
	return nil
}

// InjectOnPanic 注入 panic 时的回调（例如用于通知外部重启）
func (dw *DatabaseWriter) InjectOnPanic(cb func(err interface{}, stack []byte)) {
	dw.onPanic = cb
}

// Run 启动 DatabaseWriter 消费循环（由 M2 StorageFabric.Open() 调用）。
func (dw *DatabaseWriter) Run(ctx context.Context) {
	dw.closeMu.RLock()
	if dw.closed {
		dw.closeMu.RUnlock()
		dw.finalFlush(context.WithoutCancel(ctx))
		return
	}
	dw.wg.Add(1)
	dw.closeMu.RUnlock()

	defer dw.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			slog.Error("CRITICAL: DatabaseWriter panicked", "err", r, "stack", string(stack))
			metrics.RecordDbWriterPanic()

			if dw.onPanic != nil {
				dw.onPanic(r, stack)
			}
		}
	}()

	ticker := time.NewTicker(TickerInterval)
	defer ticker.Stop()

	for {
		// 优先处理高优先级队列，防饥饿
		if dw.drainPriorityOne(ctx) {
			return
		}

		select {
		case <-ctx.Done():
			dw.finalFlush(ctx)
			return
		case <-ticker.C:
			if len(dw.batch) > 0 {
				dw.flushBatch(ctx) //nolint:errcheck
			}
		case intent, ok := <-dw.priorityCh:
			if !ok {
				// priorityCh 已关闭（Close）：排空两条通道残留并落盘后退出，
				// 否则等待 ResultCh 的调用方永久阻塞（GR-1-001）。
				dw.finalFlush(ctx)
				return
			}
			dw.appendAndFlush(ctx, intent)
		case intent, ok := <-dw.ch:
			if !ok {
				dw.finalFlush(ctx)
				return
			}
			dw.appendAndFlush(ctx, intent)
		}
	}
}

func (dw *DatabaseWriter) drainPriorityOne(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		dw.finalFlush(ctx)
		return true
	case intent, ok := <-dw.priorityCh:
		if !ok {
			dw.finalFlush(ctx)
			return true
		}
		dw.appendAndFlush(ctx, intent)
	default:
	}
	return false
}

func (dw *DatabaseWriter) appendAndFlush(ctx context.Context, intent *MutationIntent) {
	dw.batch = append(dw.batch, intent)
	if len(dw.batch) >= MaxBatchSize {
		dw.flushBatch(ctx) //nolint:errcheck
	}
}

// Close 停止接收新写入，排空 channel 残余并最终落盘，阻塞至 Run 退出。
// 与 Run 的 ctx 无关：停机序列应先停掉所有生产者，再调用 Close（见 cmd/polaris/main.go §14）。
// 幂等：重复调用只等待。
func (dw *DatabaseWriter) Close() {
	dw.closeMu.Lock()
	if !dw.closed {
		dw.closed = true
		close(dw.ch)
		close(dw.priorityCh)
	}
	dw.closeMu.Unlock()
	dw.wg.Wait()
}

// IsClosed 供外层重启循环判断是否应停止重启 Run。
func (dw *DatabaseWriter) IsClosed() bool {
	dw.closeMu.RLock()
	defer dw.closeMu.RUnlock()
	return dw.closed
}

// finalFlush 退出前排空两条通道并落盘。
// 必须脱离取消信号：ctx 已取消时 BeginTx/COMMIT 前检查都会直接失败，
// 停机前最后一批写入（含审计事件）会被整批 failAll 丢弃。
func (dw *DatabaseWriter) finalFlush(ctx context.Context) {
	dw.drainInto(dw.priorityCh)
	dw.drainInto(dw.ch)
	for len(dw.batch) > 0 {
		before := len(dw.batch)
		if err := dw.flushBatch(context.WithoutCancel(ctx)); err != nil {
			// L2：停机最终落盘失败只能留痕（进程即将退出，无重试载体）
			slog.Error("db_writer: final flush failed, remaining batch lost", "pending", len(dw.batch), "err", err)
		}
		if len(dw.batch) >= before {
			return // CompositeGroup 等待等场景未推进，避免空转
		}
	}
}

// drainInto 非阻塞排空指定通道残余到 batch（通道已关闭或已空即返回）。
func (dw *DatabaseWriter) drainInto(ch chan *MutationIntent) {
	for {
		select {
		case intent, ok := <-ch:
			if !ok {
				return
			}
			dw.batch = append(dw.batch, intent)
		default:
			return
		}
	}
}

var (
	ErrMutationBusOverloaded = &MutationBusError{"mutation bus overloaded"}
	ErrDatabaseWriterClosed  = &MutationBusError{"database writer closed"}
	ErrStaleLease            = &MutationBusError{"stale lease"}
	ErrCompositeIncomplete   = &MutationBusError{"composite mutation incomplete"}
)

type MutationBusError struct{ msg string }

func (e *MutationBusError) Error() string { return e.msg }
