package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// ScriptSkillExecutor — protocol.SkillExecutor 实现（R7 拆分自 skill.go）。
// 架构文档: docs/arch/M06-Skill-Library.md §5
// RegistryImpl 见 skill.go；legacy 类型定义见 skill_types.go。
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

	// P1-8：幂等缓存与限流，与 InMemoryToolRegistry 保持能力对等。
	// PII 令牌还原：Skill 脚本的输入来自 Agent planning 层，不经过 LLM 对话的
	// PII 令牌化路径（令牌化仅发生在 M11 对话过滤器对 LLM 消息的处理中），
	// 因此 Skill 输入不含 ⟦PII:xxxx⟧ 令牌，无需 PIITokenVault.RestoreForTask。
	// 若未来 Skill 输入路径扩展到可能携带对话内容，需在此补充 tokenVault 注入。
	mu               sync.Mutex
	idempotencyCache *skillLRUCache    // 幂等缓存：LRU 上限 200 条 + TTL 5min
	skillLimiter     *skillRateLimiter // Skill 执行限速：默认 20 QPS
}

// NewScriptSkillExecutor 构造执行器。runner 可选（nil 时退化为仅元数据验证）。
func NewScriptSkillExecutor(reg protocol.SkillRegistry, runner ScriptRunner, loader ScriptLoader) *ScriptSkillExecutor {
	return &ScriptSkillExecutor{
		registry:         reg,
		runner:           runner,
		loader:           loader,
		idempotencyCache: newSkillLRUCache(200, 5*time.Minute),
		skillLimiter:     newSkillRateLimiter(20),
	}
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
	// P1-8 幂等缓存检查：与 InMemoryToolRegistry.checkIdempotency 行为对齐
	if key, ok := ctx.Value(protocol.CtxIdempotencyKey{}).(types.IdempotencyKey); ok && key != "" {
		e.mu.Lock()
		if cached, exists := e.idempotencyCache.get(string(key)); exists {
			e.mu.Unlock()
			return cached, nil
		}
		e.mu.Unlock()
	}

	// P1-8 限流：Skill 执行速率上限 20 QPS
	if !e.skillLimiter.Allow() {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("skill_executor: rate limit exceeded for skill %s", skillID))
	}

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

	out, err := e.runner.RunScript(ctx, skillID, scriptPath, input, meta.Trust)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "skill_executor: run script", err)
	}

	// P1-8 幂等缓存写入：成功结果按 key 缓存，下次同 key 命中直接返回
	if key, ok := ctx.Value(protocol.CtxIdempotencyKey{}).(types.IdempotencyKey); ok && key != "" {
		e.mu.Lock()
		e.idempotencyCache.set(string(key), out)
		e.mu.Unlock()
	}

	return out, nil
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
