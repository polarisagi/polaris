package security

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件声明 security 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// security 包自身是基础设施层（L0），通常不依赖其他业务模块。
// 但以下两个外部依赖需要通过接口解耦：
//   1. AuditRepository（数据持久化）→ store/repo 实现
//   2. MetricsReporter（指标上报）→ observability/metrics 实现
//
// 设计目标：
//   - security 包禁止直接 import store/repo、observability/metrics 具体包
//   - 通过此文件的接口声明，由 cli.go/Bootstrapper 注入具体实现
//
// @consumer: security/audit_trail.go, security/killswitch.go
// @producer: store/repo.AuditRepo, observability/metrics.KillSwitchInstrument

// AuditRepo security 包对审计存储的消费端接口。
// 实现：store/repo.SQLiteAuditRepo
// 注：protocol.AuditRepository 与此接口等价，此处定义防止 security 直接 import protocol（已由 protocol 反向依赖 security 类型）。
// 当前 audit_trail.go 使用 protocol.AuditRepository，过渡期两者共存，后续统一迁移到此接口。
type AuditRepo interface {
	// Insert 插入一条审计记录（持久化到 SQLite）。
	Insert(ctx context.Context, record *AuditRecord) error
	// LoadSince 加载指定时间戳之后的所有记录（用于 hash chain 校验恢复）。
	LoadSince(ctx context.Context, afterTimestampMicro int64) ([]*AuditRecord, error)
}

// KillSwitchMetrics security 包对指标上报的消费端接口。
// 实现：observability/metrics.KillSwitchInstrument
// nil 时静默（单元测试/最小化部署场景）。
type KillSwitchMetrics interface {
	// RecordStateTransition 记录 KillSwitch 状态转移（Normal→Throttle→Pause→FullStop）。
	RecordStateTransition(from, to types.KillState)
	// IncrErrorCount 递增错误计数器（用于阈值触发可视化）。
	IncrErrorCount()
}

// GuardProvider security 包对 Factuality Guard / PII 检测的消费端接口。
// 实现：security/guard 子包（允许跨子包调用，guard 不反向依赖 security 根包）。
// 此接口供 facade.go 的 IsAuthorized 扩展使用（当前 v1 只用 Cedar，不启用 Guard）。
type GuardProvider interface {
	// ContainsPII 检测文本是否包含 PII（返回命中类型列表）。
	ContainsPII(text string) []string
	// IsFactual 检测 LLM 输出是否存在明显幻觉（基于 Factuality Guard）。
	IsFactual(ctx context.Context, claim string) (bool, float64)
}
