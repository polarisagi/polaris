package store

import "context"

// 本文件声明 store 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// store 包是 L0 基础设施层，直接依赖数据库驱动（*sql.DB）。
// 对其他 internal/ 模块的消费端接口极少：
//   - MutationBus 在每次 flush 前需要检查系统租约（KillSwitch 联动）
//
// 注意：
//   LeaseChecker 已在 mutation_bus.go 中声明（与实现紧耦合，当前保持原位）。
//   本文件补充文档说明，并为未来可能新增的 L0 外部依赖预留占位。
//
// @consumer: store/mutation_bus.go（LeaseChecker），store/outbox_worker.go
// @producer: security.KillSwitch（IsOperational() 映射为 HasLease）

// LeaseProvider store 包对租约检查的消费端接口（文档化定义，规范名称）。
//
// 实现：security.KillSwitch（KillState==Normal 时 HasLease 返回 true）。
// 与 mutation_bus.go 中的 LeaseChecker 语义等价，此处作为规范接口用于跨模块文档索引。
// 注入路径：cmd/polaris/boot_substrate.go 在构造 DatabaseWriter 时传入 KillSwitch 适配器。
type LeaseProvider interface {
	// HasLease 返回当前节点是否持有数据库写入租约。
	// false 表示 KillSwitch 触发全停 / 节点降级，拒绝所有写入操作。
	HasLease(ctx context.Context) bool
}
