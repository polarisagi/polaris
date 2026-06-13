package agents

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// GovernanceAgent 后台常驻治理守门人。
//
// 职责：
//  1. 包装现有 PolicyGate（Cedar），作为策略评估的统一入口。
//  2. 管理幂等执行网关：CodeAct 产生副作用前检查 outbox 幂等键，
//     命中则返回历史快照，不产生新的物理副作用。
//  3. 内存压力监控：持续读取系统内存状态，更新共享 MemPressureLevel atomic。
//
// 生命周期：常驻 goroutine，不通过 Orchestrator。
// 与 PolicyGate 关系：GovernanceAgent 内部持有 PolicyGate，对外提供更高级的治理接口。
type GovernanceAgent struct {
	policyGate    protocol.PolicyGate // 现有 Cedar PolicyGate，保持不变
	db            *sql.DB             // 读写 outbox 幂等键
	memPressure   *atomic.Int32       // 共享内存压力等级（Memory Agent 也读这个）
	probeInterval time.Duration       // 内存探测间隔，默认 5s
}

// MemPressureLevel 内存压力等级。
type MemPressureLevel int32

const (
	MemPressureNormal   MemPressureLevel = 0 // 空闲内存 > 30%
	MemPressureModerate MemPressureLevel = 1 // 空闲内存 10%-30%
	MemPressureCritical MemPressureLevel = 2 // 空闲内存 < 10%
)

// NewGovernanceAgent 构造函数。
func NewGovernanceAgent(policyGate protocol.PolicyGate, db *sql.DB) (*GovernanceAgent, *atomic.Int32) {
	pressure := &atomic.Int32{}
	pressure.Store(int32(MemPressureNormal))
	return &GovernanceAgent{
		policyGate:    policyGate,
		db:            db,
		memPressure:   pressure,
		probeInterval: 5 * time.Second,
	}, pressure
}

// Run 启动内存监控循环（阻塞，调用方用 goroutine 启动）。
func (ga *GovernanceAgent) Run(ctx context.Context) {
	ticker := time.NewTicker(ga.probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ga.probeMemory()
		}
	}
}

// CheckIdempotent 幂等检查：给定 CodeAct 要执行的操作哈希，
// 查 outbox 表，命中返回 (mockResponse, true)，未命中返回 (nil, false)。
// 哈希算法：SHA256(method + url + body)，截取前 32 字节作为 idempotency_key。
func (ga *GovernanceAgent) CheckIdempotent(ctx context.Context, operationHash string) ([]byte, bool) {
	// 略：从 db 中查 operationHash
	return nil, false
}

// RecordExecution 记录执行成功的操作到 outbox（用于下次幂等命中）。
func (ga *GovernanceAgent) RecordExecution(ctx context.Context, operationHash string, response []byte) error {
	// 略：记录到 db
	return nil
}

// probeMemory 探测系统空闲内存，更新 memPressure atomic。
func (ga *GovernanceAgent) probeMemory() {
	// 略：读取 /proc/meminfo 等
}
