package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/types"
)

// pendingAsk 是等待用户确认的一个节点。
type pendingAsk struct {
	node  protocol.ExecNode
	tool  types.Tool
	taint types.TaintLevel
	d     policy.Decision
	key   string
}

// 审批卡里展示的参数摘要上限（完整参数仍随事件的 input 字段送达，卡片只截取展示）。
const approvalInputMax = 2000

// askBatch 把本轮校验里全部需询问的节点合并成一张审批卡（ADR-0117 决策五）。
// 批准返回 nil；拒绝/不可用返回带原因与替代方案的错误（D6）。
func (a *Agent) askBatch(ctx context.Context, vCtx *protocol.DAGValidationContext, asks []pendingAsk, mode types.PermissionMode) error {
	first := asks[0]
	if a.hitl == nil {
		// 没有审批通道：宁可拒绝也不放行（fail-closed）。
		return denyError(first.node, noApprovalChannel(first))
	}
	p := a.buildApprovalPrompt(asks, mode)
	a.publishApproval(p, asks)
	resp, err := a.hitl.Prompt(ctx, p)
	if err != nil || resp == nil || !resp.Approved {
		for _, k := range asks {
			a.markPermDenied(k.key)
		}
		for _, k := range asks {
			a.auditPermission(ctx, vCtx, "permission_denied", k.node, mode, "user_denied")
		}
		return denyError(first.node, declineMessage(asks, err))
	}
	for _, k := range asks {
		a.grantExemption(vCtx, k.node, max(k.taint, types.TaintMedium), "user:"+resp.UserID)
		a.auditPermission(ctx, vCtx, "permission_user_approved", k.node, mode, "user:"+resp.UserID)
	}
	if resp.Remember {
		a.rememberRules(ctx, asks)
	}
	return nil
}

// buildApprovalPrompt 构造审批请求：单卡 + items 列表；含硬限制项时整卡不提供“记住”。
func (a *Agent) buildApprovalPrompt(asks []pendingAsk, mode types.PermissionMode) types.HITLPrompt {
	items := make([]types.ApprovalItem, 0, len(asks))
	hard := false
	canRemember := true
	var labels []string
	risk := types.RiskLow
	for _, k := range asks {
		items = append(items, types.ApprovalItem{
			Tool: k.node.ToolName, Input: truncateRunes(string(k.node.Args), approvalInputMax),
			Reason: askReasonText(k.d.Reason), HardLimit: k.d.HardLimit, RememberLabel: k.d.RememberLabel,
		})
		hard = hard || k.d.HardLimit
		if k.d.HardLimit || len(k.d.Rules) == 0 {
			canRemember = false
		}
		if k.d.RememberLabel != "" {
			labels = append(labels, k.d.RememberLabel)
		}
		risk = max(risk, k.tool.RiskLevel)
	}
	p := types.HITLPrompt{
		ID:             fmt.Sprintf("hitl_%d", time.Now().UnixNano()),
		AgentID:        a.sCtx.AgentID,
		CheckpointType: types.CheckpointToolApproval,
		DeadlineNs:     time.Now().Add(taintReviewWindow).UnixNano(),
		RiskLevel:      int(risk),
		PermissionMode: mode,
		HardLimit:      hard,
		Reason:         batchReason(items),
		Items:          items,
		Options: []types.HITLOption{
			{Key: "approve", Label: "Allow"},
			{Key: "deny", Label: "Deny"},
		},
	}
	if canRemember {
		p.RememberLabel = strings.Join(labels, "；")
	}
	p.PromptText = approvalPromptText(items)
	if a.isBackgroundTurn() {
		// 无人值守回合（ADR-0117 追记）：不拒绝，进待审批队列；等待期限放宽到小时级，超时仍视为拒绝。
		p.Unattended = true
		p.DeadlineNs = time.Now().Add(unattendedApprovalWindow()).UnixNano()
		a.sCtx.Mu.RLock()
		p.SessionID = a.sCtx.SessionID
		a.sCtx.Mu.RUnlock()
	}
	return p
}

// unattendedApprovalWindow 无人值守审批的等待期限：M13 hitl.default_deadline_min_long（默认 1440 分钟=24 小时）。
func unattendedApprovalWindow() time.Duration {
	if m := config.CurrentThresholds().M13Interface.HITLDefaultDeadlineMinLong; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 24 * time.Hour
}

// isBackgroundTurn 本回合是否经 AcquireHeadless 获取（无人在对话界面前）。
func (a *Agent) isBackgroundTurn() bool {
	a.sCtx.Mu.RLock()
	defer a.sCtx.Mu.RUnlock()
	return a.sCtx.BackgroundTurn
}

func batchReason(items []types.ApprovalItem) string {
	if len(items) == 1 {
		return items[0].Reason
	}
	return fmt.Sprintf("本轮有 %d 项操作需要你确认", len(items))
}

func approvalPromptText(items []types.ApprovalItem) string {
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s: %s (%s)", it.Tool, it.Input, it.Reason)
	}
	return b.String()
}

func (a *Agent) publishApproval(p types.HITLPrompt, asks []pendingAsk) {
	first := asks[0]
	a.publishStreamEvent(types.AgentStreamEvent{
		Type:          types.AgentStreamEventApproval,
		Content:       p.ID,
		ToolName:      first.node.ToolName,
		ToolInput:     first.node.Args,
		DeadlineNs:    p.DeadlineNs,
		Reason:        p.Reason,
		HardLimit:     p.HardLimit,
		RememberLabel: p.RememberLabel,
		Items:         p.Items,
	})
}

// rememberRules 把用户勾选“以后不再询问”的各项最窄规则落到当前项目；硬限制项不产生规则。
func (a *Agent) rememberRules(ctx context.Context, asks []pendingAsk) {
	if a.perm.rules == nil {
		return
	}
	project := a.currentProjectID()
	for _, k := range asks {
		if k.d.HardLimit {
			continue
		}
		for _, r := range k.d.Rules {
			if _, err := a.perm.rules.AddRule(ctx, project, string(r.Kind), r.Pattern); err != nil {
				slog.WarnContext(ctx, "permission: remember approval rule failed", "kind", r.Kind, "pattern", r.Pattern, "err", err)
			}
		}
	}
}

// askReasonText 把判定原因码翻译成审批卡上的一句话。
func askReasonText(code string) string {
	switch code {
	case policy.ReasonOutsideWorkspaceWrite:
		return "要写入项目工作目录之外的位置"
	case policy.ReasonUnknownCommand:
		return "这条命令不在已知的安全命令之内"
	case policy.ReasonDangerousCommand:
		return "命令可能修改或删除文件、进程或系统状态"
	case policy.ReasonCatastrophicCommand:
		return "命令可能造成不可恢复的破坏（安全硬限制，任何权限模式都会询问）"
	case policy.ReasonNetworkWrite:
		return "要向外部网络发送或修改数据"
	case policy.ReasonPrivileged:
		return "需要特权能力"
	case policy.ReasonUIControl:
		return "要操控你的电脑界面"
	case policy.ReasonHighRiskTool:
		return "该工具风险较高"
	case policy.ReasonTaintedArgs:
		return "操作的参数来自不可信内容（安全硬限制，任何权限模式都会询问）"
	case policy.ReasonDataExfiltration:
		return "本回合读取过凭据，联网操作可能把敏感数据带出去（安全硬限制，任何权限模式都会询问）"
	case policy.ReasonAskMode:
		return "当前权限模式为“每次询问”"
	}
	return "该操作需要你确认"
}

// declineMessage 用户拒绝（或审批不可用）后回灌给模型的话：原因 + 替代方案，不要求模型重试（D6）。
func declineMessage(asks []pendingAsk, err error) string {
	parts := make([]string, 0, len(asks))
	hard := false
	for _, k := range asks {
		parts = append(parts, fmt.Sprintf("%q (%s)", k.node.ToolName, askReasonText(k.d.Reason)))
		hard = hard || k.d.HardLimit
	}
	head := "the user declined: " + strings.Join(parts, "; ")
	if err != nil {
		head = "approval was not given in time (" + err.Error() + "): " + strings.Join(parts, "; ")
	}
	tail := " Do not propose the same call again. Explain to the user what you wanted to do, or take a safer route: read-only inspection, or work inside the project directory."
	if hard {
		tail = " This is a safety hard limit that applies in every permission mode. Do not retry it; tell the user what is blocked and what they can do themselves."
	}
	return head + "." + tail
}

func noApprovalChannel(k pendingAsk) string {
	return fmt.Sprintf("tool %q needs the user's approval (%s) but no approval channel is available in this session; do not retry — explain to the user what is needed", k.node.ToolName, askReasonText(k.d.Reason))
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
