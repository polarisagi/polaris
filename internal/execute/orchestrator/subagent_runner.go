package orchestrator

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// maxSubagentStopContinuations SubagentStop hook 要求继续的最大次数（与会话 Stop 同一上限，
// HE-5：控制流在宿主，写错的 hook 不能把子 Agent 变成无限推理）。
const maxSubagentStopContinuations = 3

// SubagentHooks 子 Agent 生命周期 hook（hooks.json SubagentStart / SubagentStop，ADR-0103 决策六；
// 调用方定义接口，实现为 hook.Runner）。
type SubagentHooks interface {
	FireSubagentStart(ctx context.Context, sessionID, agentID, agentType string) string
	FireSubagentStop(ctx context.Context, sessionID, agentID, agentType, lastMessage string, stopHookActive bool) string
}

// SubagentRunner 子 Agent 执行的唯一实现：角色解析 → SubagentStart → AcquireHeadless →
// SubagentStop（有界续跑）。委派任务（DefaultTaskWorker）、用户调用的 context: fork 技能与
// agent 类型 hook 共用，保证三条入口的角色边界与 hook 语义一致。
type SubagentRunner struct {
	pool     protocol.AgentPool
	profiles AgentProfileResolver
	hooks    SubagentHooks
}

func NewSubagentRunner(pool protocol.AgentPool, profiles AgentProfileResolver, hooks SubagentHooks) *SubagentRunner {
	return &SubagentRunner{pool: pool, profiles: profiles, hooks: hooks}
}

// SubagentRequest 一次子 Agent 执行。Profile 非 nil 时直接使用，否则按 AgentName 解析。
type SubagentRequest struct {
	ParentSessionID string
	AgentID         string // 缺省生成；hook 输入的 agent_id
	AgentName       string // general-purpose / 空 = 通用 Agent
	Profile         *types.AgentProfileSpec
	Prompt          string
	Options         []types.HeadlessOption // SpawnDepth / Namespace / EventCallback 等透传
}

// Run 执行子 Agent 并返回最终输出。
func (r *SubagentRunner) Run(ctx context.Context, req SubagentRequest) (string, error) {
	if r == nil || r.pool == nil {
		return "", apperr.New(apperr.CodeInternal, "subagent runner: agent pool not configured")
	}
	profile := req.Profile
	if profile == nil {
		p, err := r.resolve(ctx, req.AgentName)
		if err != nil {
			return "", err
		}
		profile = p
	}
	agentType := firstNonEmptyString(req.AgentName, generalPurposeAgent)
	if profile != nil {
		agentType = profile.Name
	}
	if req.AgentID == "" {
		req.AgentID = "subagent-" + uuid.NewString()
	}
	withHooks := r.hooks != nil && (profile == nil || !profile.SuppressHooks)
	query := req.Prompt
	if withHooks {
		query = withHookContext(query, r.hooks.FireSubagentStart(ctx, req.ParentSessionID, req.AgentID, agentType))
	}
	opts := append(append([]types.HeadlessOption(nil), req.Options...), types.WithAgentProfile(profile))
	out, err := r.acquire(ctx, query, opts)
	for i := 0; err == nil && withHooks && i < maxSubagentStopContinuations; i++ {
		reason := r.hooks.FireSubagentStop(ctx, req.ParentSessionID, req.AgentID, agentType, out, i > 0)
		if reason == "" {
			break
		}
		// 每次 headless 执行都是全新内核实例（无跨轮历史），续跑须带上原任务与上一版输出。
		out, err = r.acquire(ctx, req.Prompt+"\n\n<previous_answer>\n"+out+"\n</previous_answer>\n\n<subagent-stop-hook>\n"+
			reason+"\n</subagent-stop-hook>", opts)
	}
	return out, err
}

// RunSubagent 按名称运行子 Agent（会话层 context: fork 技能的消费端接口形状）。
func (r *SubagentRunner) RunSubagent(ctx context.Context, parentSessionID, agent, prompt string) (string, error) {
	return r.Run(ctx, SubagentRequest{ParentSessionID: parentSessionID, AgentName: agent, Prompt: prompt})
}

func (r *SubagentRunner) acquire(ctx context.Context, query string, opts []types.HeadlessOption) (string, error) {
	res, err := r.pool.AcquireHeadless(ctx, types.Intent{Query: query}, opts...)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "subagent", err)
	}
	return res.Output, nil
}

// resolve 未知名称报错（Claude：未知 subagent_type 报错）——静默按通用 Agent 执行会让
// 委派方误以为专用角色生效。
func (r *SubagentRunner) resolve(ctx context.Context, name string) (*types.AgentProfileSpec, error) {
	if r.profiles == nil || name == "" || name == generalPurposeAgent {
		return nil, nil
	}
	p, err := r.profiles.ResolveAgentProfile(ctx, name)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "resolve agent "+name+" (call list_agents for valid targets)", err)
	}
	return p, nil
}

// withHookContext hook 附加上下文以显式标记块随任务下发（与会话层 UserPromptSubmit 同一形状）。
func withHookContext(input, hookContext string) string {
	if strings.TrimSpace(hookContext) == "" {
		return input
	}
	return input + "\n\n<hook-context>\n" + hookContext + "\n</hook-context>"
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
