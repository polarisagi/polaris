package skill

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

const (
	bronzeTTL               = 30 * time.Minute
	goldSuccessThreshold    = 0.9
	goldUsageThreshold      = 50
	silverSuccessThreshold  = 0.7
	silverDayUsageThreshold = 10
	bronzeLRUMaxSize        = 32 // Tier 0 铜牌 LRU 上限
)

// ProcessHandle 代表一个已就绪的技能进程句柄（由 M7 沙箱层实际管理）。
// ScriptSkillCache 仅持有 ID + 就绪标记，调用执行通过 M7 ExecuteSkill。
type ProcessHandle struct {
	SkillID string
	ReadyAt time.Time
	// Closer 释放进程池槽位，由 Tier 进程池注入
	Closer func()
}

// SkillStats 技能运行统计，由 M4/M6 ExecuteSkill 调用方填写后传给 promoteOrCache。
type SkillStats struct {
	SkillID     string
	SuccessRate float64 // 近期成功率
	TotalUsage  int     // 累计调用次数
	DayUsage7   int     // 近 7 天调用次数
}

// ScriptSkillCache 三级进程池缓存。
// Gold：启动时预热，常驻内存。
// Silver：首次调用后常驻。
// Bronze：按需启动，30min TTL + LRU 驱逐。
// 线程安全。
type ScriptSkillCache struct {
	mu          sync.Mutex
	goldCache   map[string]*ProcessHandle // skillID → 常驻句柄
	silverCache map[string]*ProcessHandle // skillID → 首调后常驻
	bronzeCache map[string]*bronzeEntry   // skillID → TTL+LRU

	// spawnFn 由调用方注入：给定 skillID 启动进程，返回句柄。
	// 实际调用 M7 wasm sandbox 或 os/exec；ScriptSkillCache 不感知沙箱细节。
	spawnFn func(ctx context.Context, skillID string) (*ProcessHandle, error)

	// goldMaxSize 各 Tier 上限（来自 M03 §5.3 TierParameterTable）
	goldMaxSize   int
	silverMaxSize int
	// bronzeMaxSize 铜牌 LRU 上限
	bronzeMaxSize int

	// LRU 顺序追踪（bronzeOrder 队列，头部最旧）
	bronzeOrder []string
}

type bronzeEntry struct {
	handle    *ProcessHandle
	expiresAt time.Time
	lastUsed  time.Time
}

// NewScriptSkillCache 创建缓存。
// spawnFn：由 M7 SkillSandbox 注入。goldMax/silverMax/bronzeMax：来自 spec/state.yaml Tier 参数。
func NewScriptSkillCache(
	spawnFn func(ctx context.Context, skillID string) (*ProcessHandle, error),
	goldMax, silverMax, bronzeMax int,
) *ScriptSkillCache {
	if bronzeMax <= 0 {
		bronzeMax = bronzeLRUMaxSize
	}
	return &ScriptSkillCache{
		goldCache:     make(map[string]*ProcessHandle),
		silverCache:   make(map[string]*ProcessHandle),
		bronzeCache:   make(map[string]*bronzeEntry),
		spawnFn:       spawnFn,
		goldMaxSize:   goldMax,
		silverMaxSize: silverMax,
		bronzeMaxSize: bronzeMax,
	}
}

// GetOrSpawn 查找或按需启动技能进程句柄。
// 查找顺序：gold → silver → bronze（TTL touch）→ 按需 spawn → promoteOrCache。
func (c *ScriptSkillCache) GetOrSpawn(ctx context.Context, skillID string) (*ProcessHandle, error) {
	c.mu.Lock()
	// 1. Gold 命中
	if h, ok := c.goldCache[skillID]; ok {
		c.mu.Unlock()
		return h, nil
	}
	// 2. Silver 命中
	if h, ok := c.silverCache[skillID]; ok {
		c.mu.Unlock()
		return h, nil
	}
	// 3. Bronze 命中（TTL 检查）
	if entry, ok := c.bronzeCache[skillID]; ok {
		if time.Now().Before(entry.expiresAt) {
			entry.lastUsed = time.Now()
			h := entry.handle
			c.mu.Unlock()
			return h, nil
		}
		// TTL 过期，驱逐
		delete(c.bronzeCache, skillID)
		c.removeBronzeOrderLocked(skillID)
	}
	c.mu.Unlock()

	// 4. 按需 spawn
	if c.spawnFn == nil {
		return nil, nil // 无 spawnFn（单元测试环境）
	}
	handle, err := c.spawnFn(ctx, skillID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ScriptSkillCache.GetOrSpawn", err)
	}

	// 5. 放入 bronze（等待 promoteOrCache 晋级）
	c.mu.Lock()
	c.putBronzeLocked(skillID, handle)
	c.mu.Unlock()

	return handle, nil
}

// PromoteOrCache 在技能执行后根据统计晋级缓存层级（由 M4/M6 ExecuteSkill 调用方调用）。
func (c *ScriptSkillCache) PromoteOrCache(stats SkillStats) {
	c.mu.Lock()
	defer c.mu.Unlock()

	skillID := stats.SkillID
	handle := c.getHandleLocked(skillID)
	if handle == nil {
		return
	}

	switch {
	case stats.SuccessRate >= goldSuccessThreshold && stats.TotalUsage >= goldUsageThreshold:
		if len(c.goldCache) < c.goldMaxSize || c.goldMaxSize == 0 {
			c.goldCache[skillID] = handle
			delete(c.silverCache, skillID)
			delete(c.bronzeCache, skillID)
			slog.Info("skill_cache: promoted to gold", "skill_id", skillID)
		}
	case stats.SuccessRate >= silverSuccessThreshold || stats.DayUsage7 >= silverDayUsageThreshold:
		if _, inGold := c.goldCache[skillID]; !inGold {
			if len(c.silverCache) < c.silverMaxSize || c.silverMaxSize == 0 {
				c.silverCache[skillID] = handle
				delete(c.bronzeCache, skillID)
				slog.Info("skill_cache: promoted to silver", "skill_id", skillID)
			}
		}
		// 其他情况保留 bronze（已存在则 TTL 续期）
	}
}

// WarmGold 启动时异步预热所有 gold 级技能（不阻塞 Agent 就绪）。
// goldSkillIDs 由 M5 ProceduralMemory 查询 skills 表 WHERE tier='gold' 提供。
func (c *ScriptSkillCache) WarmGold(ctx context.Context, goldSkillIDs []string) {
	concurrent.SafeGo(ctx, "skill_cache.warm_gold", func(sgCtx context.Context) {
		for _, id := range goldSkillIDs {
			if sgCtx.Err() != nil {
				return
			}
			_, err := c.GetOrSpawn(sgCtx, id)
			if err != nil {
				slog.Warn("skill_cache: gold warmup failed", "skill_id", id, "err", err)
			}
		}
	})
}

// Evict 主动驱逐（崩溃恢复前调用，清空进程池）。
func (c *ScriptSkillCache) Evict(skillID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.goldCache, skillID)
	delete(c.silverCache, skillID)
	delete(c.bronzeCache, skillID)
	c.removeBronzeOrderLocked(skillID)
}

func (c *ScriptSkillCache) getHandleLocked(skillID string) *ProcessHandle {
	if h, ok := c.goldCache[skillID]; ok {
		return h
	}
	if h, ok := c.silverCache[skillID]; ok {
		return h
	}
	if e, ok := c.bronzeCache[skillID]; ok {
		return e.handle
	}
	return nil
}

// putBronzeLocked 写入 bronze（LRU 驱逐最旧条目）。
func (c *ScriptSkillCache) putBronzeLocked(skillID string, handle *ProcessHandle) {
	// LRU 驱逐
	for len(c.bronzeCache) >= c.bronzeMaxSize && len(c.bronzeOrder) > 0 {
		oldest := c.bronzeOrder[0]
		c.bronzeOrder = c.bronzeOrder[1:]
		if e, ok := c.bronzeCache[oldest]; ok && e.handle.Closer != nil {
			e.handle.Closer()
		}
		delete(c.bronzeCache, oldest)
	}
	c.bronzeCache[skillID] = &bronzeEntry{
		handle:    handle,
		expiresAt: time.Now().Add(bronzeTTL),
		lastUsed:  time.Now(),
	}
	c.bronzeOrder = append(c.bronzeOrder, skillID)
}

func (c *ScriptSkillCache) removeBronzeOrderLocked(skillID string) {
	for i, id := range c.bronzeOrder {
		if id == skillID {
			c.bronzeOrder = append(c.bronzeOrder[:i], c.bronzeOrder[i+1:]...)
			return
		}
	}
}
