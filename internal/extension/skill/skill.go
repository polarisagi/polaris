package skill

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件实现 protocol.SkillRegistry / SkillSelector / SkillExecutor 的内存版本。
// 持久化版本见 sqlite_registry.go (SQLiteRegistryImpl)。
// 历史决策见 docs/arch/decisions/ADR-0002-skill-registry-consolidation.md
//   —— 已消除本地 Registry/Skill/LogicCollapse/Trajectory/Step/LifecycleState 类型，
//   统一直接使用 types.SkillMeta 存储与传递。

// ============================================================================
// RegistryImpl — protocol.SkillRegistry 实现（内存版）
// ============================================================================

// RegistryImpl 直接以 types.SkillMeta 为存储单元。
// 强制约束: meta.Name 必须以 types.SkillPrefix 为前缀。
// 重名注册 → name collision 错误，记录审计事件。
type RegistryImpl struct {
	skills map[string]*types.SkillMeta // name → meta
	mu     sync.RWMutex
	audit  []string // 审计日志
}

func NewRegistry() *RegistryImpl {
	return &RegistryImpl{
		skills: make(map[string]*types.SkillMeta),
	}
}

// 编译期接口合规验证
var (
	_ protocol.SkillRegistry = (*RegistryImpl)(nil)
	_ protocol.SkillSelector = (*HybridRetriever)(nil)
	_ protocol.SkillExecutor = (*ScriptSkillExecutor)(nil)
)

// Register 注册技能。未通过 cosign 签名验证的技能拒绝注册。
// 同名同版本重复注册视为误操作，拒绝（collision）；同名不同版本视为升级，
// 允许覆盖并触发反向依赖兼容性扫描（与 SQLiteRegistryImpl.Register 的
// isUpgrade 分支语义对齐，见 sqlite_registry.go markReverseDependenciesCompatCheck）。
func (r *RegistryImpl) Register(ctx context.Context, meta types.SkillMeta) error {
	if meta.Trust < types.TrustLocal {
		return errCosignVerifyFailed
	}
	if !strings.HasPrefix(meta.Name, types.SkillPrefix) {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("skill name error: got %s", meta.Name), errInvalidSkillName)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	isUpgrade := false
	if existing, exists := r.skills[meta.Name]; exists {
		if existing.Version == meta.Version {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("skill name collision: %s (existing version %s)", meta.Name, existing.Version))
		}
		isUpgrade = true
	}

	allDeps := append(meta.DependsOn, meta.ComposesOf...)
	if err := r.detectSkillCycle(meta.Name, allDeps); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "skill dependency cycle detected", err)
	}

	// 存储 meta 副本——避免 caller 修改外部变量影响内部状态
	metaCopy := meta
	r.skills[meta.Name] = &metaCopy

	if isUpgrade {
		affected := r.markReverseDependenciesCompatCheck(meta.Name)
		if len(affected) > 0 {
			r.audit = append(r.audit, fmt.Sprintf("upgrade %s -> %s: marked %d dependent skill(s) needs_compat_check", meta.Name, meta.Version, len(affected)))
		}
	}
	return nil
}

// markReverseDependenciesCompatCheck 对 targetSkill 的所有直接/传递依赖方
// （DependsOn 或 ComposesOf 含 targetSkill 的技能）标记 NeedsCompatCheck=true，
// 与 SQLiteRegistryImpl 的同名方法做同一件事（BFS 反向依赖遍历），只是数据源
// 从 SQL 查询换成内存 map 扫描。调用方必须已持有 r.mu 写锁。返回被标记的技能名列表。
func (r *RegistryImpl) markReverseDependenciesCompatCheck(targetSkill string) []string {
	visited := make(map[string]bool)
	var affected []string
	queue := []string{targetSkill}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true

		for name, m := range r.skills {
			if visited[name] {
				continue
			}
			dependsOnCur := false
			for _, d := range m.DependsOn {
				if d == cur {
					dependsOnCur = true
					break
				}
			}
			if !dependsOnCur {
				for _, c := range m.ComposesOf {
					if c == cur {
						dependsOnCur = true
						break
					}
				}
			}
			if dependsOnCur {
				m.NeedsCompatCheck = true
				affected = append(affected, name)
				queue = append(queue, name)
			}
		}
	}
	return affected
}

// Get 按名称和版本查询技能；返回副本，调用方修改不影响内部状态。
func (r *RegistryImpl) Get(ctx context.Context, name, version string) (*types.SkillMeta, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	meta, ok := r.skills[name]
	if !ok {
		return nil, errSkillNotFound
	}
	if version != "" && meta.Version != version {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("version mismatch: want %s, got %s", version, meta.Version))
	}
	out := *meta
	return &out, nil
}

// List 按过滤条件列出技能。
func (r *RegistryImpl) List(ctx context.Context, filter types.SkillFilter) ([]types.SkillMeta, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []types.SkillMeta //nolint:prealloc
	for _, m := range r.skills {
		if m.Deprecated && !filter.IncludeDeprecated {
			continue
		}
		if filter.RiskLevelMax != "" && riskGT(m.RiskLevel, filter.RiskLevelMax) {
			continue
		}
		if len(filter.Capabilities) > 0 && !hasCapability(m.Capabilities, filter.Capabilities) {
			continue
		}
		result = append(result, *m)
	}
	return result, nil
}

// Deprecate 标记技能为废弃；记录审计。RegistryImpl 扩展方法（非 SkillRegistry 接口成员）。
func (r *RegistryImpl) Deprecate(ctx context.Context, name, version string, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	meta, ok := r.skills[name]
	if !ok {
		return errSkillNotFound
	}
	meta.Deprecated = true
	if version != "" {
		meta.Version = version
	}
	r.audit = append(r.audit, fmt.Sprintf("deprecate %s: %s", name, reason))
	return nil
}

// AuditLog 返回审计日志副本。
func (r *RegistryImpl) AuditLog() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.audit))
	copy(out, r.audit)
	return out
}

func (r *RegistryImpl) detectSkillCycle(skillName string, deps []string) error {
	visited := make(map[string]bool)
	queue := make([]string, len(deps))
	copy(queue, deps)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == skillName {
			return apperr.New(apperr.CodeInternal,
				fmt.Sprintf("cyclic skill dependency: %s", skillName))
		}
		if visited[cur] {
			continue
		}
		visited[cur] = true
		if m, ok := r.skills[cur]; ok {
			queue = append(queue, m.DependsOn...)
			queue = append(queue, m.ComposesOf...)
		}
	}
	return nil
}

// ScriptSkillExecutor（protocol.SkillExecutor 实现）见 skill_executor.go（R7 拆分）。

// ============================================================================
// 辅助函数
// ============================================================================

// riskGT 比较风险等级，返回 a > b。等级序: low < medium < high < critical。
func riskGT(a, b string) bool {
	order := map[string]int{"low": 0, "medium": 1, "high": 2, "critical": 3}
	return order[a] > order[b]
}

// hasCapability 检查 caps 是否包含 required 中所有项（顺序无关，大小写/空白容错）。
func hasCapability(caps []string, required []string) bool {
	for _, want := range required {
		found := false
		for _, c := range caps {
			if strings.EqualFold(strings.TrimSpace(c), strings.TrimSpace(want)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ============================================================================
// 错误类型（同时被 sqlite_registry.go 使用）
// ============================================================================

var (
	errCosignVerifyFailed = apperr.New(apperr.CodeInternal, "skill: cosign signature verification failed")
	errSkillNotFound      = apperr.New(apperr.CodeInternal, "skill: not found")
	errInvalidSkillName   = apperr.New(apperr.CodeInternal, "skill: name must start with 'skill:'")
)

// 技能库 legacy 类型定义 (Skill/JSONSchema/Condition/SkillSource) 见
// skill_types.go（R7 拆分）。
