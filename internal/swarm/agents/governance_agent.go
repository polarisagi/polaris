package agents

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

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
	policyGate     protocol.PolicyGate // 现有 Cedar PolicyGate，保持不变
	db             protocol.SQLQuerier // 读写 outbox 幂等键
	memPressure    *atomic.Int32       // 共享内存压力等级（Memory Agent 也读这个）
	probeInterval  time.Duration       // 内存探测间隔，默认 5s
	validatorRules *codeValidatorRules
}

// MemPressureLevel 内存压力等级。
type MemPressureLevel int32

const (
	MemPressureNormal   MemPressureLevel = 0 // 空闲内存 > 30%
	MemPressureModerate MemPressureLevel = 1 // 空闲内存 10%-30%
	MemPressureCritical MemPressureLevel = 2 // 空闲内存 < 10%
)

// NewGovernanceAgent 构造函数。
func NewGovernanceAgent(policyGate protocol.PolicyGate, db protocol.SQLQuerier) (*GovernanceAgent, *atomic.Int32) {
	pressure := &atomic.Int32{}
	pressure.Store(int32(MemPressureNormal))
	return &GovernanceAgent{
		policyGate:     policyGate,
		db:             db,
		memPressure:    pressure,
		probeInterval:  5 * time.Second,
		validatorRules: newCodeValidatorRules(),
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
		SELECT payload FROM idempotent_cache 
		WHERE operation_hash = ? 
		LIMIT 1
	`, operationHash).Scan(&payload)

	if err != nil {
		return nil, false
	}
	return payload, true
}

// AuditAST 在代码注入沙箱前执行同步 AST 级前置拦截（Layer 0）。
// 当前实现：Go 代码走 go/parser；Python/Bash/TS 走增强正则 import 扫描。
// 返回第一个违规即 fast-fail，不扫描全文（性能优先）。
func (ga *GovernanceAgent) AuditAST(language string, code []byte, caps CapabilitySet) error {
	switch language {
	case "go":
		return ga.auditGoAST(code, caps)
	case "python":
		return auditImportLines(code, ga.validatorRules.pythonDangerousImports, caps)
	case "bash", "sh":
		return auditImportLines(code, ga.validatorRules.bashDangerousCommands, caps)
	case "typescript", "javascript":
		return auditImportLines(code, ga.validatorRules.tsDangerousImports, caps)
	default:
		return nil // 未知语言：宽松放行，正则层已覆盖
	}
}

// RecordExecution 记录执行成功的操作到 outbox（用于下次幂等命中）。
func (ga *GovernanceAgent) RecordExecution(ctx context.Context, operationHash string, response []byte) error {
	_, err := ga.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO idempotent_cache
		  (operation_hash, payload, created_at)
		VALUES
		  (?, ?, ?)
	`, operationHash, response, time.Now().UnixMilli())
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GovernanceAgent.RecordExecution", err)
	}
	return nil
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
		// 解析失败留 Warn 后走 fallback：/proc/meminfo 格式变化会让 total 保持 0，
		// 下方 `if total > 0` 已挡住除零，但静默降级意味着治理 Agent 长期用
		// 兜底估值做内存压力判断而没人知道。
		v, ok, matched := parseMemInfoLine(line)
		if !matched {
			continue
		}
		if !ok {
			return probeMemoryFallback()
		}
		if strings.HasPrefix(line, "MemTotal:") {
			total = v
		} else {
			avail = v
		}
	}

	if total > 0 {
		return avail / total
	}
	return probeMemoryFallback()
}

// parseMemInfoLine 解析 /proc/meminfo 的 MemTotal/MemAvailable 行。
// 返回 (值, 解析成功, 是否是关注的行)——三返回值是为了让调用方区分
// "这行不关我事"（跳过）与"这行该解析但解析失败"（走 fallback）两种情况。
func parseMemInfoLine(line string) (val float64, ok, matched bool) {
	if !strings.HasPrefix(line, "MemTotal:") && !strings.HasPrefix(line, "MemAvailable:") {
		return 0, false, false
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0, false, true
	}
	v, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		slog.Warn("governance_agent: /proc/meminfo 数值解析失败，转用兜底内存探测", "raw", line, "err", err)
		return 0, false, true
	}
	return v, true, true
}

func probeMemoryDarwin() float64 {
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

// PruneStaleCache 清理 30 天以上的幂等缓存记录。
func (ga *GovernanceAgent) PruneStaleCache(ctx context.Context) error {
	cutoff := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	_, err := ga.db.ExecContext(ctx, `DELETE FROM idempotent_cache WHERE created_at < ?`, cutoff)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GovernanceAgent.PruneStaleCache", err)
	}
	return nil
}
