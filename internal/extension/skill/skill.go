package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
// 强制约束: meta.Name 必须以 "skill:" 为前缀。
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
	_ protocol.SkillSelector = (*SelectorImpl)(nil)
	_ protocol.SkillExecutor = (*ScriptSkillExecutor)(nil)
)

// Register 注册技能。未通过 cosign 签名验证的技能拒绝注册。
func (r *RegistryImpl) Register(ctx context.Context, meta types.SkillMeta) error {
	if meta.Trust < types.TrustLocal {
		return errCosignVerifyFailed
	}
	if !strings.HasPrefix(meta.Name, "skill:") {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("skill name error: got %s", meta.Name), errInvalidSkillName)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, exists := r.skills[meta.Name]; exists {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("skill name collision: %s (existing version %s)", meta.Name, existing.Version))
	}

	allDeps := append(meta.DependsOn, meta.ComposesOf...)
	if err := r.detectSkillCycle(meta.Name, allDeps); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "skill dependency cycle detected", err)
	}

	// 存储 meta 副本——避免 caller 修改外部变量影响内部状态
	metaCopy := meta
	r.skills[meta.Name] = &metaCopy
	return nil
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

// ============================================================================
// SelectorImpl — protocol.SkillSelector 实现（启发式，不调 LLM）
// ============================================================================

// SelectorImpl 启发式排序: 能力匹配(0.4) + 复杂度匹配(0.3) + 通过率(0.2) + 延迟(0.1)。
// 符合 par_inv_05: Selector 不调 LLM。
type SelectorImpl struct {
	registry protocol.SkillRegistry
}

func NewSelector(reg protocol.SkillRegistry) *SelectorImpl {
	return &SelectorImpl{registry: reg}
}

// Select 启发式选择最佳技能（取 top 5）。
func (s *SelectorImpl) Select(ctx context.Context, hint types.TaskHint) ([]types.SkillMeta, error) {
	all, err := s.registry.List(ctx, types.SkillFilter{
		Capabilities:      hint.CapabilitiesNeeded,
		RiskLevelMax:      "high",
		IncludeDeprecated: false,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SelectorImpl.Select", err)
	}

	sort.Slice(all, func(i, j int) bool {
		return s.score(all[i], hint) > s.score(all[j], hint)
	})

	if len(all) > 5 {
		all = all[:5]
	}
	return all, nil
}

func (s *SelectorImpl) score(meta types.SkillMeta, hint types.TaskHint) float64 {
	capScore := 0.0
	for _, want := range hint.CapabilitiesNeeded {
		for _, has := range meta.Capabilities {
			if has == want {
				capScore += 1.0
			}
		}
	}
	if len(hint.CapabilitiesNeeded) > 0 {
		capScore /= float64(len(hint.CapabilitiesNeeded))
	}

	complexityScore := 1.0
	if hint.ComplexityScore > 0.8 && meta.RiskLevel == "low" {
		complexityScore = 0.3 // 复杂任务需要高阶技能
	}

	passScore := meta.Benchmarks.PassRate
	if passScore < 0 {
		passScore = 0
	}

	latencyScore := 1.0
	if meta.Benchmarks.AvgLatencyMs > 5000 {
		latencyScore = 0.3
	} else if meta.Benchmarks.AvgLatencyMs > 2000 {
		latencyScore = 0.7
	}

	return capScore*0.4 + complexityScore*0.3 + passScore*0.2 + latencyScore*0.1
}

// ============================================================================
// ScriptSkillExecutor — protocol.SkillExecutor 实现
// 架构文档: docs/arch/M06-Skill-Library.md §5
// ============================================================================

// ScriptRunner 执行 TypeScript/Python 技能脚本（由 pkg/action.ContainerSandbox 实现，接口注入避免循环依赖）。
type ScriptRunner interface {
	// trustTier 驱动隔离等级选择（经 AssignSandboxTier）
	RunScript(ctx context.Context, skillName, scriptPath string, input []byte, trustTier types.TrustTier) ([]byte, error)
}

// ScriptLoader 从存储层加载技能脚本路径。
type ScriptLoader interface {
	LoadScriptPath(skillID string) (string, error)
}

type ScriptSkillExecutor struct {
	registry protocol.SkillRegistry
	runner   ScriptRunner // nil → 返回输入原文（降级）
	loader   ScriptLoader // 可选兜底：meta.ScriptPath 不存在时尝试此加载器
	// policy 是脚本执行前的唯一 Cedar 授权门（M11 PolicyGate.IsAuthorized）。
	// 不使用 *sandbox.ExecEnvelope 全量执行（Policy→Tier→Route→Run）：物理隔离已由
	// runner.RunScript（ContainerSandbox，内部自行 AssignSandboxTier + Rust bwrap/Seatbelt 沙箱执行）
	// 提供，若再套一层 envelope.Execute 会试图把 meta.Name 当作已注册工具名二次路由执行，
	// 该名字从未注册进 InProcessSandbox，必然 "unknown tool" 出错——两次执行、两条判定逻辑，
	// 正是本次重构要消除的重复实现。
	policy protocol.PolicyGate
}

// NewScriptSkillExecutor 构造执行器。runner 可选（nil 时退化为仅元数据验证）。
func NewScriptSkillExecutor(reg protocol.SkillRegistry, runner ScriptRunner, loader ScriptLoader) *ScriptSkillExecutor {
	return &ScriptSkillExecutor{registry: reg, runner: runner, loader: loader}
}

// WithPolicy 注入 Cedar PolicyGate（M11），脚本执行前的唯一权限判定入口。
func (e *ScriptSkillExecutor) WithPolicy(gate protocol.PolicyGate) *ScriptSkillExecutor {
	e.policy = gate
	return e
}

// ExecuteSkill 执行技能。
// 加载优先级: meta.ScriptPath（marketplace 安装路径 / Logic Collapse 蒸馏脚本）
//
//	> loader.LoadScriptPath（文件系统兜底）> meta.Instructions（tool-mode 指令技能，无编译脚本）。
//
// 无脚本可执行时（纯 SKILL.md 指令技能，或 runner 未注入）: 返回 instructions 全文供 LLM 读取执行，
// 与 cmd/polaris/skill_loader.go 注册进 InProcessSandbox 的同名工具保持完全一致的语义（唯一实现，禁止重复）。
func (e *ScriptSkillExecutor) ExecuteSkill(ctx context.Context, skillID string, input []byte) ([]byte, error) {
	meta, err := e.registry.Get(ctx, skillID, "")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "skill_executor: registry.Get", err)
	}
	if meta.Deprecated {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("skill_executor: skill %s is deprecated", skillID))
	}

	// 优先从 extension_instances.install_path 读取（marketplace 安装路径 / Logic Collapse 蒸馏脚本）
	scriptPath := meta.ScriptPath
	if scriptPath == "" && e.loader != nil {
		if p, loadErr := e.loader.LoadScriptPath(skillID); loadErr == nil {
			scriptPath = p
		}
	}

	// 无编译脚本或 runner 未注入 → tool-mode 指令技能：返回 instructions，不执行任何代码。
	if scriptPath == "" || e.runner == nil {
		return renderInstructions(meta.Instructions, input), nil
	}

	if err := e.ValidateSkill([]byte(scriptPath)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "skill_executor: script validation", err)
	}

	if e.policy == nil {
		// fail-closed：脚本执行必须经 PolicyGate 授权，policy 未注入时拒绝而非静默放行。
		return nil, apperr.New(apperr.CodeForbidden, "skill_executor: policy gate not configured (deny-by-default)")
	}
	allowed, err := e.policy.IsAuthorized(ctx, "agent", "script_execute", meta.Name, map[string]any{
		"trust_tier":  int(meta.Trust),
		"tool_source": string(types.ToolSkill),
		"taint_level": int(types.TaintMedium), // skill 脚本至少具有 Medium Taint
	})
	if err != nil || !allowed {
		reason := "policy denied"
		if err != nil {
			reason = err.Error()
		}
		return nil, apperr.New(apperr.CodeForbidden, "skill_executor: "+reason)
	}

	return e.runner.RunScript(ctx, skillID, scriptPath, input, meta.Trust)
}

// renderInstructions 将 tool-mode 指令技能的 SKILL.md 正文与调用方输入拼接返回。
// 唯一实现：cmd/polaris/skill_loader.go 的 InProcessSandbox 注册闭包委托至此，禁止重复实现。
func renderInstructions(instructions string, input []byte) []byte {
	var req struct {
		Input string `json:"input"`
	}
	_ = json.Unmarshal(input, &req) //nolint:errcheck // 非法/空 input 时按无附加输入处理
	out := instructions
	if req.Input != "" {
		out += "\n\n---\n\n输入：" + req.Input
	}
	return []byte(out)
}

// ValidateSkill 校验脚本路径合规性。
func (e *ScriptSkillExecutor) ValidateSkill(scriptBytes []byte) error {
	if len(scriptBytes) == 0 {
		return apperr.New(apperr.CodeInternal, "skill_executor: empty script path")
	}
	return nil
}

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

// 技能库类型定义。
// 架构文档: docs/arch/06-Skill-Library-深度选型.md §1

// Skill 是可命名、可参数化、可索引的复用技能。
type Skill struct {
	ID      string
	Name    string
	Version int

	Description  string
	Instructions string

	InputSchema   *JSONSchema
	OutputSchema  *JSONSchema
	Precondition  *Condition
	Postcondition *Condition

	WasmBytes []byte
	WasmHash  string

	Embedding []float32
	Signature string
	Tags      []string

	SuccessRate  float64
	AvgLatencyUs int64
	UseCount     int64
	LastUsedAt   int64

	RiskLevel   int
	SandboxTier int
	Source      string // builtin | llm_generated | user_defined
	SourceTrace string

	Deprecated       bool
	DeprecationLevel int
	NeedsRevalidate  bool

	DependsOn  []string
	ComposesOf []string
}

// JSONSchema 是 JSON Schema 定义。
type JSONSchema struct {
	Type       string
	Properties map[string]*JSONSchema
	Required   []string
}

// Condition 前置/后置条件。
type Condition struct {
	Description string
	Schema      *JSONSchema
}

// SkillSource 技能来源。
type SkillSource string

const (
	SkillBuiltin      SkillSource = "builtin"
	SkillLLMGenerated SkillSource = "llm_generated"
	SkillUserDefined  SkillSource = "user_defined"
)
