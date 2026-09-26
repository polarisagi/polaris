package hook

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/classifier"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// MCPToolCaller mcp_tool 处理器调用已连接 MCP 服务器的工具（consumer-side；实现为 MCPManager 适配器）。
type MCPToolCaller interface {
	CallHookTool(ctx context.Context, server, tool string, args map[string]any) (string, error)
}

// PromptEvaluator prompt / agent 处理器：把判定提示交给模型，返回模型原始输出
// （期望 {"ok": bool, "reason": "..."}）。consumer-side；实现经 Provider 路由（R1.11）。
type PromptEvaluator interface {
	EvaluateHookPrompt(ctx context.Context, prompt, model string, agent bool) (string, error)
}

// Deps Runner 依赖。Policy 与 Wrapper 为安全门：缺失时相应处理器 fail-closed 不执行。
type Deps struct {
	Registry   *Registry
	Policy     protocol.PolicyGate
	Wrapper    sandbox.ArgvWrapper
	HTTPClient network.SafeHTTPClient
	MCP        MCPToolCaller
	Prompt     PromptEvaluator
	PII        *guard.PIIDetector
	PIIDesens  *guard.PIIDesensitizer
}

// Runner 按事件分发 hook。
type Runner struct {
	deps Deps
	risk *classifier.CommandRiskClassifier
}

func NewRunner(deps Deps) *Runner {
	if deps.PII == nil {
		deps.PII = guard.NewPIIDetector()
	}
	return &Runner{deps: deps, risk: classifier.NewDefaultClassifier()}
}

// SetMCPToolCaller / SetPromptEvaluator 启动期注入（MCPManager 与 LLM 路由晚于执行信封构造）。
func (r *Runner) SetMCPToolCaller(c MCPToolCaller)     { r.deps.MCP = c }
func (r *Runner) SetPromptEvaluator(p PromptEvaluator) { r.deps.Prompt = p }

// hookActiveKey 标记 hook 处理器自身发起的调用（mcp_tool / prompt），这些调用不再触发 hook，
// 否则 matcher 覆盖到被调用工具时会无限递归。
type hookActiveKey struct{}

// Registry 返回来源注册表（供管理接口 Reload）。
func (r *Runner) Registry() *Registry { return r.deps.Registry }

// Dispatch 执行匹配 in.HookEventName 的全部已信任处理器并合并结果。
func (r *Runner) Dispatch(ctx context.Context, in Input) Outcome {
	if r == nil || r.deps.Registry == nil || ctx.Value(hookActiveKey{}) != nil || ctx.Value(protocol.CtxHooksSuppressedKey{}) != nil {
		return Outcome{}
	}
	if in.PermissionMode == "" {
		in.PermissionMode = "default"
	}
	handlers := r.deps.Registry.match(in.HookEventName, in.MatchSubject())
	handlers = filterIf(handlers, in)
	if len(handlers) == 0 {
		return Outcome{}
	}
	if err := r.authorize(ctx, in.HookEventName); err != nil {
		return failClosedOutcome(in.HookEventName, err)
	}
	payload, err := r.redactedPayload(ctx, in)
	if err != nil {
		return failClosedOutcome(in.HookEventName, err)
	}
	results := r.runAll(ctx, handlers, in, payload)
	var out Outcome
	for _, res := range results {
		out.merge(in.HookEventName, res)
	}
	for _, e := range out.Errors {
		slog.WarnContext(ctx, "hook: handler error", "event", in.HookEventName, "err", e)
	}
	return out
}

// authorize 事件级策略门（deny-by-default；policy 缺失即拒绝，R1.14）。
func (r *Runner) authorize(ctx context.Context, ev Event) error {
	if r.deps.Policy == nil {
		return apperr.New(apperr.CodeForbidden, "hook: policy gate not configured (fail-closed)")
	}
	ok, err := r.deps.Policy.IsAuthorized(ctx, "agent", "hook_execute", string(ev), map[string]any{"event": string(ev)})
	if err != nil || !ok {
		return apperr.New(apperr.CodeForbidden, "hook: hook_execute denied by policy")
	}
	return nil
}

// failClosedOutcome 安全门失败：守卫型事件（工具与权限）按阻断处理——用户依赖这些 hook 作为护栏，
// 护栏无法运行时不能当作放行；其余事件只记错误，不阻断主流程。
func failClosedOutcome(ev Event, err error) Outcome {
	out := Outcome{Errors: []string{err.Error()}}
	if ev == EventPreToolUse || ev == EventPermissionRequest || ev == EventUserPromptSubmit {
		out.block(ev, "hook guard unavailable: "+err.Error())
	}
	return out
}

// redactedPayload 序列化前 PII 脱敏（hook 进程可把输入写到任意本地文件，脱敏是最后一道纵深防御）。
func (r *Runner) redactedPayload(ctx context.Context, in Input) ([]byte, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "hook: marshal input", err)
	}
	redacted, n, err := r.deps.PII.RedactWithMode(ctx, string(raw), "replace", in.SessionID, r.deps.PIIDesens, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "hook: PII redact failed", err)
	}
	if n > 0 {
		slog.WarnContext(ctx, "hook: redacted PII from hook input", "event", in.HookEventName, "matches", n)
	}
	return []byte(redacted), nil
}

// runAll 同步处理器并发执行，结果按声明顺序返回（合并语义与顺序无关，顺序只为日志稳定）；
// async 命令处理器后台执行、结果丢弃（两家语义）。
func (r *Runner) runAll(ctx context.Context, handlers []boundHandler, in Input, payload []byte) []handlerResult {
	results := make([]handlerResult, len(handlers))
	var wg sync.WaitGroup
	for i, bh := range handlers {
		if bh.Handler.Async && bh.Handler.Type == TypeCommand {
			concurrent.SafeGo(context.WithoutCancel(ctx), "hook.async_handler", func(bg context.Context) {
				// L3：async 处理器结果不参与决策（两家语义），失败只留痕。
				if res := r.runOne(bg, bh, in, payload); res.Err != nil || (res.ExitCode != 0 && res.ExitCode != 2) {
					slog.WarnContext(bg, "hook: async handler failed", "event", in.HookEventName, "exit", res.ExitCode, "err", res.Err)
				}
			})
			continue
		}
		wg.Add(1)
		concurrent.SafeGo(ctx, "hook.handler", func(c context.Context) {
			defer wg.Done()
			results[i] = r.runOne(c, bh, in, payload)
		})
	}
	wg.Wait()
	out := results[:0]
	for i, bh := range handlers {
		if bh.Handler.Async && bh.Handler.Type == TypeCommand {
			continue
		}
		out = append(out, results[i])
	}
	return out
}

func (r *Runner) runOne(ctx context.Context, bh boundHandler, in Input, payload []byte) handlerResult {
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, hookActiveKey{}, true), bh.Handler.Timeout())
	defer cancel()
	switch bh.Handler.Type {
	case TypeCommand:
		return r.runCommand(ctx, bh, in, payload)
	case TypeHTTP:
		return r.runHTTP(ctx, bh, payload)
	case TypeMCPTool:
		return r.runMCPTool(ctx, bh, payload)
	case TypePrompt, TypeAgent:
		return r.runPrompt(ctx, bh, payload)
	}
	return handlerResult{Err: apperr.New(apperr.CodeInvalidInput, "hook: unsupported handler type "+bh.Handler.Type)}
}
