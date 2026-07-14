package skill

import (
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// 本文件历史上实现过 protocol.SkillRegistry 的内存版本（RegistryImpl）。
// 持久化版本见 sqlite_registry.go (SQLiteRegistryImpl)，是当前唯一生产实现
// （boot_tools.go 构造 skill.NewSQLiteRegistry，内存版从未被生产代码调用）。
// 2026-07-14（ADR-0051）：内存版 RegistryImpl/NewRegistry/Register/Get/List/
// Deprecate/AuditLog/detectSkillCycle 删除——与本会话已删除的
// internal/execute/orchestrator 内存版 Blackboard 同构的"两套实现，一套从未被
// 采纳"模式：SQLiteRegistryImpl 独立实现了同名的 markReverseDependenciesCompatCheck
// 反向依赖扫描，证明内存版不是生产依赖的唯一来源。
//
// 历史决策见 docs/arch/decisions/ADR-0002-skill-registry-consolidation.md
//   —— 已消除本地 Registry/Skill/LogicCollapse/Trajectory/Step/LifecycleState 类型，
//   统一直接使用 types.SkillMeta 存储与传递。

// 编译期接口合规验证
var (
	_ protocol.SkillSelector = (*HybridRetriever)(nil)
	_ protocol.SkillExecutor = (*ScriptSkillExecutor)(nil)
)

// ScriptSkillExecutor（protocol.SkillExecutor 实现）见 skill_executor.go（R7 拆分）。

// ============================================================================
// 辅助函数（供 sqlite_registry.go 复用）
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
