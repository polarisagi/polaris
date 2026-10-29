package store

// outbox_errors.go — OutboxWorker 的哨兵错误定义（从 outbox_worker.go 拆出，R7 行数上限）。
//
// 这三个哨兵刻意**不用** `apperr.New(...)` 构造，原因值得单独记一笔：
// `apperr.Error.Is` 按 Code 比较而非按身份比较（见 pkg/apperr/apperr.go §Is 的注释，
// 那是为了让 `errors.Is(err, &apperr.Error{Code: CodeNotFound})` 这种用法成立）。
// 一旦拿 `apperr.New(CodeInternal, ...)` 当哨兵，`errors.Is(err, ErrPoisonPill)` 就会对
// **任意** CodeInternal 错误为真——而 CodeInternal 是全仓最常见的错误码。
// 后果实测于 2026-08-13 轮：一次普通的投递失败被判成毒丸，直接落 dead，
// 跳过 attempts 自增与指数退避，属静默丢数据（TestOutboxWorker_BackoffSequence 报出）。
//
// 这里采用与同包 mutation_bus.go 的 MutationBusError 一致的自定义类型：
// errors.Is 在没有 Is 方法时退化为指针身份比较，正是哨兵需要的语义。

// OutboxError 是 outbox 内部哨兵错误类型：只承载消息、按身份比较，
// 不参与 apperr 的 Code 语义匹配。
type OutboxError struct{ msg string }

func (e *OutboxError) Error() string { return e.msg }

// ErrVersionStale outbox 版本号过时（incoming_version <= existing_version），触发跳过。
var ErrVersionStale = &OutboxError{"outbox: version stale"}

// ErrUnknownTargetEngine outbox 目标引擎未注册，触发 dead 状态。
var ErrUnknownTargetEngine = &OutboxError{"outbox: unknown target engine"}

// ErrPoisonPill outbox 毒丸（crash_recovery_count >= 3），触发 dead 状态。
// 对应 docs/arch/spec/state.yaml §outbox 的 outbox_inv_03。
var ErrPoisonPill = &OutboxError{"outbox_inv_03: poison pill"}
