// Package bootstrap 实现 Polaris 模块生命周期统一编排。
// 设计来源：参照 polaris-agent internal/bootstrap/（Bootable + DependencyMap + Kahn 拓扑排序）。
//
// 核心思路：
//   - 每个核心模块实现 Bootable 接口，声明自身依赖（Dependencies()）
//   - Bootstrapper 执行 Kahn 拓扑排序，按正确顺序初始化模块
//   - 优雅关停分四阶段（停流→排干→刷盘→释放），每个模块按需实现对应阶段
//
// 禁止：
//   - 模块通过全局变量自取依赖（必须经 DependencyMap.Get 注入）
//   - 在 Init 完成前调用模块的业务方法
package bootstrap

import "context"

// ─── 依赖注入 ──────────────────────────────────────────────────────────────────

// DependencyMap L8→L0 单向下沉依赖注入表。
// 所有模块在 Init 时从此处取依赖，禁止自行查找全局变量。
type DependencyMap struct {
	deps map[string]any
}

// NewDependencyMap 创建空依赖表。
func NewDependencyMap() *DependencyMap {
	return &DependencyMap{deps: make(map[string]any)}
}

// Register 注册一个命名依赖。重复注册将覆盖旧值。
func (m *DependencyMap) Register(name string, dep any) {
	m.deps[name] = dep
}

// Get 取依赖，不存在返回 nil。
func (m *DependencyMap) Get(name string) any {
	return m.deps[name]
}

// MustGet 取依赖，不存在 panic（用于确认性断言）。
func (m *DependencyMap) MustGet(name string) any {
	v, ok := m.deps[name]
	if !ok {
		panic("bootstrap: missing required dependency: " + name)
	}
	return v
}

// ─── 生命周期契约 ──────────────────────────────────────────────────────────────

// Bootable 所有 Polaris 核心模块必须实现的生命周期契约。
//
// 使用方式：
//  1. 模块在 Init 中从 DependencyMap 取依赖并完成内部初始化
//  2. Init 成功后 Ready() 必须返回 true
//  3. Dependencies() 声明本模块依赖哪些已注册名称
//     Bootstrapper 会按依赖关系拓扑排序，确保被依赖模块先 Init
type Bootable interface {
	// Init 显式接收由 bootstrap 传入的依赖集（DI 锚定）。
	// 禁止模块自行通过全局变量获取依赖。
	Init(deps *DependencyMap) error

	// Ready 只有在 Init 成功且内部状态闭环后才返回 true。
	// Bootstrapper 在 Init 后验证 Ready()，若为 false 则启动失败。
	Ready() bool

	// Dependencies 返回该模块在拓扑排序中必须依赖的模块标识。
	// 返回的名称必须与 Bootstrapper.RegisterModule 注册的名称对应。
	Dependencies() []string
}

// ─── 四阶优雅关停 ─────────────────────────────────────────────────────────────

// Stage1Stopper 停流：停止接收外部新请求（Gateway/Adapter/Channel）。
// 触发条件：收到 SIGTERM/SIGINT，立即熔断外部感知。
// 目标模块：gateway.Server、channel.Manager。
type Stage1Stopper interface {
	StopIngress(ctx context.Context) error
}

// Stage2Drainer 结账：排干当前内存队列中的未决任务。
// 触发条件：Stage1 完成后，给 Agent/Scheduler 宽限期处理剩余任务。
// 目标模块：swarm.Orchestrator、automation.Scheduler。
type Stage2Drainer interface {
	Drain(ctx context.Context) error
}

// Stage3Flusher 刷盘：执行最终 DB Commit 与 WAL Checkpoint。
// 触发条件：Stage2 完成后，Store 执行 wal_checkpoint(TRUNCATE)。
// 目标模块：store.SQLiteStore。
type Stage3Flusher interface {
	Flush(ctx context.Context) error
}

// Stage4Closer 灭火：释放底层句柄与系统资源。
// 触发条件：Stage3 完成后，关闭 DB 连接池、VFS 游标、MCP 子进程。
// 目标模块：store、vfs、extension/mcp。
type Stage4Closer interface {
	Close(ctx context.Context) error
}

// ─── 启动辅助接口 ─────────────────────────────────────────────────────────────

// ConfigProvider 提供内存级 KMS 密钥总线分发。
// 由 Bootstrapper 实现，供需要主密钥的模块在 Init 时调用。
// 调用方必须负责用后即焚（memclr）。
type ConfigProvider interface {
	GetMasterKey() []byte
}

// HealthChecker 模块健康检查接口（用于启动阶段门控）。
// 与 swarm.Pinger 语义等价，在此统一定义。
type HealthChecker interface {
	HealthCheck(ctx context.Context) error
}
