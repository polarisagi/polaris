package agents

import (
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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
	auditAgent    *SecurityAuditAgent // nil = 跳过 LLM 语义审查
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
	var payload []byte
	err := ga.db.QueryRowContext(ctx, `
		SELECT payload FROM outbox 
		WHERE idempotency_key = 'idem:' || ? 
		AND status = 'done' 
		LIMIT 1
	`, operationHash).Scan(&payload)

	if err != nil {
		return nil, false
	}
	return payload, true
}

// WithSecurityAuditAgent 注入安全审查代理
func (ga *GovernanceAgent) WithSecurityAuditAgent(a *SecurityAuditAgent) {
	ga.auditAgent = a
}

// ValidateCodeWithAudit 三层安全审查入口。
// Layer 0（同步）：AST 前置拦截。
// Layer 1（同步）：规则命中 → 立即返回 error。
// Layer 2（异步）：规则通过 → 后台 LLM 审查，发现风险通过 HITL 通知用户。
// taskID/agentID 为空时自动生成临时 ID。
func (ga *GovernanceAgent) ValidateCodeWithAudit(
	ctx context.Context,
	language string,
	code []byte,
	caps CapabilitySet,
	taskID, agentID string,
) error {
	// Layer 0：同步 AST 前置拦截（最快 fast-fail）
	if err := ga.AuditAST(language, code, caps); err != nil {
		return err
	}

	// Layer 1: 硬边界
	if err := ga.ValidateCode(language, code, caps); err != nil {
		return err
	}
	// Layer 2: AI 审查（异步，不阻塞）
	if ga.auditAgent != nil {
		ga.auditAgent.AuditAsync(ctx, language, code, taskID, agentID)
	}
	return nil
}

// AuditAST 在代码注入沙箱前执行同步 AST 级前置拦截（Layer 0）。
// 当前实现：Go 代码走 go/parser；Python/Bash/TS 走增强正则 import 扫描。
// 返回第一个违规即 fast-fail，不扫描全文（性能优先）。
func (ga *GovernanceAgent) AuditAST(language string, code []byte, caps CapabilitySet) error {
	switch language {
	case "go":
		return auditGoAST(code, caps)
	case "python":
		return auditImportLines(code, pythonDangerousImports, caps)
	case "bash", "sh":
		return auditImportLines(code, bashDangerousCommands, caps)
	case "typescript", "javascript":
		return auditImportLines(code, tsDangerousImports, caps)
	default:
		return nil // 未知语言：宽松放行，正则层已覆盖
	}
}

// RecordExecution 记录执行成功的操作到 outbox（用于下次幂等命中）。
func (ga *GovernanceAgent) RecordExecution(ctx context.Context, operationHash string, response []byte) error {
	_, err := ga.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO outbox
		  (idempotency_key, target_engine, operation, scope, payload, status, created_at)
		VALUES
		  ('idem:' || ?, 'idempotent_gateway', 'record', 'execution', ?, 'done', ?)
	`, operationHash, response, time.Now().UnixMilli())
	return err
}

// probeMemory 探测系统空闲内存，更新 memPressure atomic。
func (ga *GovernanceAgent) probeMemory() {
	var freePct float64

	switch runtime.GOOS {
	case "linux":
		freePct = probeMemoryLinux()
	case "darwin":
		freePct = probeMemoryDarwin()
	default:
		freePct = probeMemoryFallback()
	}

	if freePct > 0.30 {
		ga.memPressure.Store(int32(MemPressureNormal))
	} else if freePct > 0.10 {
		ga.memPressure.Store(int32(MemPressureModerate))
	} else {
		ga.memPressure.Store(int32(MemPressureCritical))
	}
}

func probeMemoryLinux() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return probeMemoryFallback()
	}
	defer f.Close()

	var total, avail float64
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return probeMemoryFallback()
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "MemTotal:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				total, _ = strconv.ParseFloat(parts[1], 64)
			}
		} else if strings.HasPrefix(line, "MemAvailable:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				avail, _ = strconv.ParseFloat(parts[1], 64)
			}
		}
	}

	if total > 0 {
		return avail / total
	}
	return probeMemoryFallback()
}

func probeMemoryDarwin() float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// 1. 获取物理内存总量（hw.memsize，字节）
	cmdSys := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize")
	sysOut, err := cmdSys.Output()
	if err != nil {
		return probeMemoryFallback()
	}

	memsizeStr := strings.TrimSpace(string(sysOut))
	memsize, err := strconv.ParseFloat(memsizeStr, 64)
	if err != nil || memsize <= 0 {
		return probeMemoryFallback()
	}

	// 2. vm_stat 部分
	cmd := exec.CommandContext(ctx, "vm_stat")
	out, err := cmd.Output()
	if err != nil {
		return probeMemoryFallback()
	}

	var pagesFree, pagesInactive float64
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "Pages free:") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				val := strings.TrimRight(parts[2], ".")
				pagesFree, _ = strconv.ParseFloat(val, 64)
			}
		} else if strings.HasPrefix(line, "Pages inactive:") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				val := strings.TrimRight(parts[2], ".")
				pagesInactive, _ = strconv.ParseFloat(val, 64)
			}
		}
	}

	pageSize := float64(os.Getpagesize())

	// 3. 可用内存 = (pagesFree + pagesInactive) * pageSize
	avail := (pagesFree + pagesInactive) * pageSize

	// 4. freePct = 可用内存 / 物理内存总量
	if memsize > 0 {
		return avail / memsize
	}
	return probeMemoryFallback()
}

func probeMemoryFallback() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	alloc := float64(m.Alloc)
	// mock 8GB limit for the fallback calculation
	total := 8.0 * 1024 * 1024 * 1024
	return (total - alloc) / total
}
