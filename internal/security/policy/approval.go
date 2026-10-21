package policy

import (
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// 工具授权判定（ADR-0117 决策四）：全部是确定性规则，不调用 LLM。
// Decide 是 S_VALIDATE 污点拦截、computer_use/browser_use HITL、bash/run_command 执行前
// 共用的唯一判定入口。硬限制（hardLimit）先于一切模式与已记住的授权求值：
// "完全访问"档与"以后不再询问"都不能绕过它。

// Verdict 判定结论。
type Verdict int

const (
	VerdictAllow Verdict = iota
	VerdictAsk
	// VerdictDeny 自保护路径（ADR-0121 决策四）：任何模式、记住的授权、审批都不能放开，
	// 调用方不弹卡，直接把原因与替代方案回给模型。
	VerdictDeny
)

// RuleKind 已记住授权的规则种类。
type RuleKind string

const (
	RuleCmd    RuleKind = "cmd"    // 命令前缀（逐词匹配）
	RuleDomain RuleKind = "domain" // 精确域名（不含子域）
	RuleDir    RuleKind = "dir"    // 工作目录外的写入目录（精确目录）
	RuleApp    RuleKind = "app"    // computer_use/browser_use 的应用名或站点域名
	RuleTool   RuleKind = "tool"   // 工具名（MCP 为 mcp__server__tool 全名）
)

// Rule 一条已记住的授权（作用域固定为某个项目，由调用方按项目取出后传入）。
type Rule struct {
	ID      string
	Kind    RuleKind
	Pattern string
}

// Context 判定所需的环境。
type Context struct {
	Roots []string // 工作区可信根：项目工作目录 + workspace
	Home  string
	Rules []Rule // 当前项目已记住的授权
	// SecretFlow 为真表示会话内读取过凭据（secret_get）或本次参数含 PII 令牌，
	// 此时任何可联网的操作都可能把敏感数据带出去。
	SecretFlow bool
}

// Decision 判定结果。
type Decision struct {
	Verdict   Verdict
	Reason    string // 机器可读原因码（审计与审批卡片文案共用）
	HardLimit bool   // 硬限制：任何档位都询问，且不可被记住
	Auto      string // 放行时的判定名，审计写 reason=auto:<Auto>
	RuleID    string // 因命中已记住的授权放行时的规则 ID
	// Rules 勾选"以后不再询问"时将创建的最窄规则；硬限制或无可记住项时为空。
	Rules         []Rule
	RememberLabel string
}

// 原因码。
const (
	ReasonOutsideWorkspaceWrite = "outside_workspace_write"
	ReasonUnknownCommand        = "unknown_command"
	ReasonDangerousCommand      = "dangerous_command"
	ReasonCatastrophicCommand   = "catastrophic_command"
	ReasonNetworkWrite          = "network_write"
	ReasonPrivileged            = "privileged"
	ReasonUIControl             = "ui_control"
	ReasonHighRiskTool          = "high_risk_tool"
	ReasonTaintedArgs           = "tainted_args"
	ReasonDataExfiltration      = "data_exfiltration"
	ReasonAskMode               = "ask_mode"
	ReasonSelfProtected         = "self_protected"
	ReasonSensitivePath         = "sensitive_path"
	ReasonPersistencePath       = "persistence_path"
	ReasonSystemPath            = "system_path"
)

type opKind int

const (
	opRead opKind = iota
	opFileWrite
	opShell
	opNetWrite
	opPrivileged
	opUI
	opOtherWrite
)

// operation 工具调用归一后的操作描述。
type operation struct {
	kind     opKind
	tool     string
	targets  []string // 写入目标（绝对、已解析符号链接）
	outside  []string // 其中位于可信根之外的目标
	cmd      CmdVerdict
	host     string // 网络写的目标主机
	app      string // UI 操控的应用或站点
	highRisk bool
	netCap   bool // 能力上可联网
	// selfHit / sensReason：读写目标（或 shell 中静态可解析的路径词）落入自保护 / 目录文件敏感类；
	// sensReason 为对应的硬限制原因码（sensitive_path / persistence_path / system_path），空串表示未命中。
	selfHit    bool
	sensReason string
}

// Decide 对一次工具调用给出授权判定。argTaint 为参数污点（取计划内最高污点）。
func Decide(mode types.PermissionMode, tool types.Tool, args []byte, argTaint types.TaintLevel, ctx Context) Decision {
	if _, ok := types.ParsePermissionMode(string(mode)); !ok {
		mode = types.ModeDefault // 未知模式取最严档
	}
	op := classifyOp(tool, args, ctx)
	if op.selfHit {
		return Decision{Verdict: VerdictDeny, Reason: ReasonSelfProtected}
	}
	if d, hard := hardLimit(op, argTaint, ctx); hard {
		return d
	}
	if op.kind == opRead {
		return Decision{Verdict: VerdictAllow, Auto: "read"}
	}
	if mode == types.ModeFullAccess {
		return Decision{Verdict: VerdictAllow, Auto: "full_access"}
	}
	if id, ok := op.coveredBy(ctx.Rules, mode); ok {
		return Decision{Verdict: VerdictAllow, Auto: "remembered", RuleID: id}
	}
	if mode == types.ModeAutoReview {
		if auto := op.autoAllow(); auto != "" {
			return Decision{Verdict: VerdictAllow, Auto: auto}
		}
	}
	return op.ask(mode)
}

// hardLimit 安全/隐私硬限制：与模式、已记住的授权无关，命中即询问且不可记住。
func hardLimit(op operation, taint types.TaintLevel, ctx Context) (Decision, bool) {
	hard := func(reason string) (Decision, bool) {
		return Decision{Verdict: VerdictAsk, Reason: reason, HardLimit: true}, true
	}
	if op.sensReason != "" {
		return hard(op.sensReason)
	}
	if op.kind == opShell && op.cmd.Class == CmdCatastrophic {
		return hard(catastropheReason(op.cmd.Reason))
	}
	if ctx.SecretFlow && op.reachesNetwork() {
		return hard(ReasonDataExfiltration)
	}
	if taint >= types.TaintHigh && op.sensitive() {
		return hard(ReasonTaintedArgs)
	}
	return Decision{}, false
}

// reachesNetwork 操作是否可能把数据送出本机。
func (o operation) reachesNetwork() bool {
	if o.kind == opShell {
		return o.cmd.Network
	}
	return o.netCap
}

// sensitive 参数被外部内容污染时不得自动执行的操作类别。
func (o operation) sensitive() bool {
	switch o.kind {
	case opNetWrite, opPrivileged:
		return true
	case opShell:
		return o.cmd.Class >= CmdDangerous
	case opFileWrite:
		return len(o.outside) > 0
	case opOtherWrite:
		return len(o.outside) > 0 || o.highRisk
	case opUI:
		return true // 键盘/鼠标输入类动作被外部内容驱动，等同被注入指令远程操作电脑
	}
	return false
}

// autoAllow 自动档下无需询问的操作；返回判定名，空串表示需询问。
func (o operation) autoAllow() string {
	switch o.kind {
	case opFileWrite:
		if len(o.outside) == 0 {
			return "workspace_write"
		}
	case opShell:
		if o.cmd.Class == CmdSafe {
			return "safe_command"
		}
	case opOtherWrite:
		if len(o.outside) == 0 && !o.highRisk {
			return "local_write"
		}
	}
	return ""
}

func splitPattern(p string) []string { return strings.Fields(p) }

// JoinPattern 把命令前缀词拼成规则 Pattern（空格分隔，展示友好）。
func JoinPattern(words []string) string { return strings.Join(words, " ") }
