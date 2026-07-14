package hitl

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// taintExemptionTokenTTL 豁免令牌有效期，与 HITLPrompt 10 分钟审批窗口的量级
// 一致——豁免只覆盖"审批后立即重试"这一次，不做成长期免检通行证。
const taintExemptionTokenTTL = 10 * time.Minute

// GatewayImpl 实现了 protocol.HITL，管理人机交互网关 [ESCALATE]。
// 架构文档: docs/arch/M13-Interface-Scheduler.md §2.4

type Notifier interface {
	Notify(ctx context.Context, msg types.HITLNotification) error
}

type GatewayImpl struct {
	store    protocol.Store
	notifier Notifier

	// waiters 保存等待审批结果的 channel
	mu      sync.Mutex
	waiters map[string]chan types.HITLResponse

	currentPolicyEtag string // Cedar policy 当前 etag，由外部热更新时注入

	// Task 21: L3 Regression dependencies
	evalRunner protocol.EvalRunner
	regression RegressionDetector
	l3Cooldown time.Duration

	// exemptionVault 存放 M04 §3 TaintBlocked→HITL 审批→颁发豁免令牌 转义路径
	// 铸造出的 TaintExemptionToken，供 tool.InMemoryToolRegistry 下一次执行
	// 同一 Agent 的工具调用时查询。nil（未注入）时 Respond 跳过铸造，行为与
	// 改造前完全一致（此前只有一行 TODO 注释，从不真正铸造）。
	exemptionVault *token.ExemptionVault
}

// SetExemptionVault 注入豁免令牌存储（可选，与
// tool.InMemoryToolRegistry.WithExemptionVault 指向同一个实例，
// cmd/polaris/boot_tools.go 组装根构造）。
func (g *GatewayImpl) SetExemptionVault(v *token.ExemptionVault) {
	g.exemptionVault = v
}

var _ protocol.HITL = (*GatewayImpl)(nil)

func (g *GatewayImpl) SetNotifier(n Notifier) {
	g.notifier = n
}

func (g *GatewayImpl) SetL3RegressionDeps(runner protocol.EvalRunner, r RegressionDetector, cooldown time.Duration) {
	g.evalRunner = runner
	g.regression = r
	g.l3Cooldown = cooldown
}

func NewGateway(store protocol.Store) *GatewayImpl {
	return &GatewayImpl{
		store:   store,
		waiters: make(map[string]chan types.HITLResponse),
	}
}

// Prompt 挂起当前任务并请求人工审批。
//
//nolint:gocyclo,nestif // 原因：HITL 审批流程涉及高风险拦截、强制冷却与上下文超时控制等多个不可分割的网关级拦截逻辑。
func (g *GatewayImpl) Prompt(ctx context.Context, p types.HITLPrompt) (*types.HITLResponse, error) {
	// Task 21+22 修复（2026-07-04 审计）：触发条件从 `p.RiskLevel >= 3` 收窄为
	// `p.CheckpointType == "l4_multi_sig"`。
	// 原因：RiskLevel>=3 会误捕获所有高风险 HITL 请求（如 code_act_warning、
	// security_review、logic_collapse_high_risk 等，见 internal/action/codeact/code_act.go、
	// internal/extension/native/extension_manager.go、cmd/polaris/adapters_misc.go 等调用方），
	// 导致这些与"L4 自我改进候选晋升"无关的审批被强制附加 P0/P1 全量回归 + 强制冷却期，
	// 阻塞正常的工具执行/扩展安装审批。L4 自我改进晋升是本回归门禁的唯一设计目标
	// （见 internal/learning/engine.go detectL4Trigger，CheckpointType 固定为 "l4_multi_sig"）。
	if p.CheckpointType == "l4_multi_sig" && g.evalRunner != nil && g.regression != nil {
		slog.Info("hitl_gateway: triggering L3 full regression", "checkpoint", p.ID)

		// Run P0+P1 regression
		report, err := g.evalRunner.RunSuite(context.Background(), "regression_p0_p1", "")
		if err == nil && report != nil {
			if report.P0Fail > 0 {
				slog.Warn("hitl_gateway: P0 regression failed, auto-denying patch", "checkpoint", p.ID)
				resp := types.HITLResponse{Approved: false, Reason: "auto_denied_p0_regression_failed"}
				if err := g.Respond(context.Background(), p.ID, resp); err != nil {
					slog.Error("hitl gateway: respond failed", "pending_id", p.ID, "err", err)
				}
				return &resp, nil
			}

			// P0 passed, generate shadow diff
			shadowReport, rErr := g.regression.DetectRegression(context.Background(), p.CheckpointType)
			if rErr == nil && shadowReport != nil {
				p.PromptText += "\n\n" + shadowReport.Markdown
			}

			// Apply cooldown
			cooldown := g.l3Cooldown
			if cooldown == 0 {
				cooldown = 10 * time.Minute
			}
			p.EligibleApproveTime = time.Now().Add(cooldown).Unix()
		}
	}

	// 若调用方未设置截止时间但 p.DeadlineNs > 0，用 DeadlineNs 建立截止上下文，
	// 防止因上层 context 无超时而无限阻塞。
	//
	// 2026-07-07 修复：DeadlineNs 全部调用方（agent_execute_util.go/effect.go、
	// learning/engine.go、cronadmin/cron_runner.go）都构造为
	// time.Now().Add(N).UnixNano()——即"绝对 Unix 纳秒时间戳"，与字段名
	// "Deadline"（截止时间点）语义一致。此前这里误当作"相对当前时刻的
	// time.Duration"又叠加了一次 time.Now()，导致把 N 分钟/N 小时后的绝对
	// 纳秒时间戳（约 10^18 量级）当作纳秒级 Duration 加到 now() 上，
	// 实际生成的 deadline 被推迟到约 56 年后——等价于超时机制完全失效，
	// 所有 HITL 请求实质上永不超时（resolveTimeoutAction 的
	// kill_pause/auto_deny/auto_approve 三条策略均从未被真正触发过，只能
	// 靠外层调用方自带的 ctx 超时兜底，而多数调用方并未额外设置）。
	if p.DeadlineNs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Unix(0, p.DeadlineNs))
		defer cancel()
	}

	// 1. 持久化 pending 状态
	key := []byte("hitl:pending:" + p.ID)
	data, err := json.Marshal(p)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "hitl_gateway: marshal failed", err)
	}
	if err := g.store.Put(ctx, key, data); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "hitl_gateway: put failed", err)
	}
	if g.notifier != nil {
		concurrent.SafeGo(context.Background(), "automation.hitl.notify", func(ctx context.Context) {
			if err := g.notifier.Notify(ctx, types.HITLNotification{
				CheckpointID: p.ID,
				// TaskID 此前硬编码为空字符串，Slack/Email 通知里永远看不出是哪个
				// Agent 发起的审批；HITLPrompt 补齐 AgentID 字段后一并带上
				// （无 AgentID 的场景如扩展安装审查仍为空，如实反映"不适用"）。
				TaskID:      p.AgentID,
				Description: p.PromptText,
				Risk:        p.CheckpointType,
				Timeout:     p.DeadlineNs,
				ReviewURL:   "/v1/hitl/review?id=" + p.ID,
			}); err != nil {
				slog.Error("hitl gateway: notify failed", "checkpoint", p.ID, "err", err)
			}
		})
	}

	// 2. 注册 waiter
	ch := make(chan types.HITLResponse, 1)
	g.mu.Lock()
	g.waiters[p.ID] = ch
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.waiters, p.ID)
		g.mu.Unlock()
	}()

	// 3. 阻塞等待或超时 (上下文控制)
	select {
	case <-ctx.Done():
		// 超时/取消
		action := g.resolveTimeoutAction(p)
		switch action {
		case "auto_approve":
			resp := types.HITLResponse{Approved: true, Reason: "auto_approved_on_timeout"}
			if err := g.Respond(context.Background(), p.ID, resp); err != nil {
				slog.Error("hitl gateway: respond failed", "pending_id", p.ID, "err", err)
			}
			return &resp, nil
		case "auto_deny":
			resp := types.HITLResponse{Approved: false, Reason: "auto_denied_on_timeout"}
			if err := g.Respond(context.Background(), p.ID, resp); err != nil {
				slog.Error("hitl gateway: respond failed", "pending_id", p.ID, "err", err)
			}
			return &resp, nil
		default: // "kill_pause" 或未配置
			return nil, ctx.Err()
		}
	case resp := <-ch:
		return &resp, nil
	}
}

func (g *GatewayImpl) resolveTimeoutAction(p types.HITLPrompt) string {
	// TaintLevel >= TaintMedium(2) 时超时一律拒绝，禁止 auto_approve（M13 §2.4）。
	// 此地板不受下方权限模式判断影响——防止被污染/被提示注入的 Agent 拿用户的
	// 设备操控权限设置当挡箭牌，绕过对外部不可信内容的强制人工复核。
	if p.TaintLevel >= 2 {
		return "auto_deny"
	}

	// 电脑/浏览器操控 checkpoint 的超时兜底与"设置 → 设备操控"权限模式联动
	// （M13 §2.4 权限模式联动）：
	//   - full_access（完全访问/上帝模式）：interceptComputerUse 正常情况下压根
	//     不会为此模式发起 HITL（needHITL 恒为 false），这里是防御性兜底——
	//     万一未来调用路径变化导致仍然发起了审批且无人应答，超时行为也应与
	//     "无需确认自主执行任何操作"的产品承诺一致，而不是意外卡死在 kill_pause。
	//   - auto_review/default：不在此处特殊处理，继续走下方通用逻辑得到 kill_pause——
	//     这两个模式的产品语义就是"高危操作要等人审"，超时不该被默认放行，
	//     这是符合设计的行为，不是需要修的缺口。
	if p.CheckpointType == types.CheckpointDeviceControlReview && p.PermissionMode == types.ModeFullAccess {
		return "auto_approve"
	}

	if p.RiskLevel == 0 {
		// 白名单来自编译期不可变内核常量（internal/config/immutable_constants.go），
		// 不接受运行期注入/覆写——防止扩展或调用方在运行时扩大 auto_approve 范围绕过 HITL（M13 §2.4）。
		allowed := false
		for _, t := range config.AutoApproveAllowedActions() {
			if p.CheckpointType == t {
				allowed = true
				break
			}
		}
		if allowed {
			// auto_approve 前校验 Cedar policy etag 原子性（M13 §2.4）
			if !g.validateDecisionEtag(p.DecisionEtag) {
				// etag 不一致：策略已热更新，升级为 HITL 审批（不自动放行）
				return "kill_pause"
			}
			return "auto_approve"
		}
	}
	if p.CheckpointType == "high_risk" || p.RiskLevel >= 3 {
		return "auto_deny"
	}
	return "kill_pause"
}

// validateDecisionEtag 原子校验当前 Cedar policy etag 与决策时刻记录的 etag 是否一致。
// etag 为空（旧路径兼容）时放行；不一致时拒绝 auto_approve。
func (g *GatewayImpl) validateDecisionEtag(decisionEtag string) bool {
	if decisionEtag == "" {
		return true // 旧路径兼容：无 etag 字段，跳过校验
	}
	g.mu.Lock()
	currentEtag := g.currentPolicyEtag
	g.mu.Unlock()
	return currentEtag == "" || currentEtag == decisionEtag
}

// SetPolicyEtag 更新当前 Cedar policy etag（热更新时由 PolicyWatcher 调用）。
func (g *GatewayImpl) SetPolicyEtag(etag string) {
	g.mu.Lock()
	g.currentPolicyEtag = etag
	g.mu.Unlock()
}

// Respond 提交人工审批决策。
//
//nolint:nestif // 原因：审批时需要解包历史状态并校验强制冷却时间，嵌套较深但逻辑单一，无需强行拆分。
func (g *GatewayImpl) Respond(ctx context.Context, checkpointID string, response types.HITLResponse) error {
	// Task 21: Check mandatory cooldown
	key := []byte("hitl:pending:" + checkpointID)
	if response.Approved {
		data, err := g.store.Get(ctx, key)
		if err == nil {
			var p types.HITLPrompt
			if json.Unmarshal(data, &p) == nil {
				if p.EligibleApproveTime > 0 {
					if time.Now().Unix() < p.EligibleApproveTime {
						return apperr.New(apperr.CodeForbidden, "hitl_gateway: mandatory cooldown active, please carefully read the shadow regression report before approving")
					}
				}

				// Task 8（2026-07-14 补齐）: Mint TaintExemptionToken on human approval。
				// 此前只有日志 + TODO 注释，令牌从未真正铸造——即便 tool 层的出口污点
				// 检查触发了 HITL 审批且人工批准，下一次重试仍会撞上同一个拦截，
				// M04 §3 转义路径整体形同虚设。
				//
				// fail-closed 而非 best-effort：ExemptionFieldContent 为空（可能是
				// 发起侧未能从错误链取出被拦截数据，或该 checkpoint 根本不是出口污点
				// 转义场景）或未注入 exemptionVault 时，明确跳过铸造并记录原因，
				// 不铸造一个内容为空、Valid() 对任意 data 都可能误判通过的令牌。
				switch {
				case p.TaintLevel <= 0:
					// 非出口污点转义场景（其余 HITL checkpoint 类型），无需铸造。
				case len(p.ExemptionFieldContent) == 0:
					slog.Warn("hitl_gateway: approved high-taint checkpoint has empty ExemptionFieldContent, skipping token mint (fail-closed)",
						"checkpoint", checkpointID, "checkpoint_type", p.CheckpointType)
				case g.exemptionVault == nil:
					slog.Warn("hitl_gateway: exemptionVault not configured, TaintExemptionToken minted but not stored, next retry will not find it",
						"checkpoint", checkpointID)
				case p.AgentID == "":
					slog.Warn("hitl_gateway: approved high-taint checkpoint has empty AgentID, cannot key exemption vault, skipping mint (fail-closed)",
						"checkpoint", checkpointID)
				default:
					tok := token.NewTaintExemptionToken(p.ExemptionFieldContent, taintExemptionTokenTTL, response.UserID)
					g.exemptionVault.Store(p.AgentID, tok)
					slog.Info("hitl_gateway: minted and stored TaintExemptionToken for approved high-taint operation",
						"checkpoint", checkpointID, "agent_id", p.AgentID, "summary", tok.Summary())
				}
			}
		}
	}

	// 1. 清理 pending
	// （可选：可以验证记录是否存在）
	if err := g.store.Delete(ctx, key); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "hitl_gateway: delete pending failed", err)
	}

	// 2. 持久化归档记录 (audit)
	archiveKey := []byte(fmt.Sprintf("hitl:archive:%s:%d", checkpointID, time.Now().UnixNano()))
	archiveData, _ := json.Marshal(response)
	_ = g.store.Put(ctx, archiveKey, archiveData)

	// 3. 通知等待中的任务
	g.mu.Lock()
	ch, ok := g.waiters[checkpointID]
	if ok {
		ch <- response
		delete(g.waiters, checkpointID)
	}
	g.mu.Unlock()

	if !ok {
		// 任务可能已经因为超时被取消（或跨节点等原因，当前只做本地分发）
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("hitl_gateway: no active waiter for %s (possibly timed out)", checkpointID))
	}
	return nil
}

// Pending 返回当前所有待审批请求。
func (g *GatewayImpl) Pending(ctx context.Context) ([]types.HITLPrompt, error) {
	iter, err := g.store.Scan(ctx, []byte("hitl:pending:"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "GatewayImpl.Pending", err)
	}
	defer iter.Close()

	var prompts []types.HITLPrompt
	for iter.Next() {
		var p types.HITLPrompt
		if err := json.Unmarshal(iter.Value(), &p); err == nil {
			prompts = append(prompts, p)
		}
	}
	return prompts, nil
}

// ChannelNotifier/NewChannelNotifier/SetDispatcher/Notify/BroadcastTainted
// 见 gateway_notify.go（R7 拆分）。
