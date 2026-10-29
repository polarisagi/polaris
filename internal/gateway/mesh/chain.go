package mesh

import (
	"fmt"
	"slices"
)

// DelegationExtensionURI Polaris 委派扩展（ADR-0111 决策四）。A2A 门面在 message.metadata[本 URI]
// 里读写 {spawn_depth, chain}；MCP 门面用 a2a_delegate 的同名入参。两个门面共用 ValidateChain。
const DelegationExtensionURI = "https://polarisagi.online/a2a/ext/delegation/v1"

// maxChainLen 委派链长度的硬上限，与深度上限无关：防止对端塞一个上万元素的数组
// 把每次校验和落库都变成放大攻击。
const maxChainLen = 16

// Delegation 跨网络传递的委派元数据。
type Delegation struct {
	SpawnDepth int      `json:"spawn_depth"`
	Chain      []string `json:"chain"`
}

// RejectReason 拒绝原因（也是 Prometheus a2a_rejected_total 的 reason 标签取值）。
type RejectReason string

const (
	RejectDepth  RejectReason = "depth"
	RejectChain  RejectReason = "chain"
	RejectPolicy RejectReason = "policy"
	RejectInput  RejectReason = "input"
)

// Rejection 入站委派被拒绝的结构化原因。
type Rejection struct {
	Reason  RejectReason
	Message string
}

func (r *Rejection) Error() string { return string(r.Reason) + ": " + r.Message }

// NormalizeDelegation 规整入站委派元数据：调用方未携带扩展（外部 Agent）时按
// depth=1、chain=[调用方] 处理（ADR-0111 决策四第 2 条）。
func NormalizeDelegation(d *Delegation, callerNodeID string) Delegation {
	if d == nil {
		return Delegation{SpawnDepth: 1, Chain: []string{callerNodeID}}
	}
	out := Delegation{SpawnDepth: d.SpawnDepth, Chain: slices.Clone(d.Chain)}
	if out.SpawnDepth < 1 {
		out.SpawnDepth = 1
	}
	if len(out.Chain) == 0 {
		out.Chain = []string{callerNodeID}
	}
	return out
}

// ValidateChain 入站校验。规则按顺序：
//  1. 链长度硬上限；
//  2. 链尾必须是令牌所属节点——对端可以如实转述上游，但不能冒充自己是别人
//     （否则 A 可以伪造 chain=[X] 让本节点以为请求来自 X，绕开环检测）；
//  3. 本节点已在链中 = 成环；
//  4. spawn_depth 超过上限。
func ValidateChain(d Delegation, selfNodeID, callerNodeID string, maxDepth int) *Rejection {
	if len(d.Chain) > maxChainLen {
		return &Rejection{RejectChain, fmt.Sprintf("delegation chain too long (%d > %d)", len(d.Chain), maxChainLen)}
	}
	if last := d.Chain[len(d.Chain)-1]; last != callerNodeID {
		return &Rejection{RejectChain, fmt.Sprintf("chain tail %q does not match authenticated caller %q", last, callerNodeID)}
	}
	if slices.Contains(d.Chain, selfNodeID) {
		return &Rejection{RejectChain, fmt.Sprintf("delegation loop: node %s already in chain %v", selfNodeID, d.Chain)}
	}
	if d.SpawnDepth > maxDepth {
		return &Rejection{RejectDepth, fmt.Sprintf("spawn_depth %d exceeds max %d", d.SpawnDepth, maxDepth)}
	}
	return nil
}

// OutboundDelegation 出站时追加本节点：depth 由调用方传入（已是子任务深度），chain = 上游链 + 本节点。
func OutboundDelegation(upstream []string, selfNodeID string, depth int) Delegation {
	chain := append(slices.Clone(upstream), selfNodeID)
	return Delegation{SpawnDepth: depth, Chain: chain}
}
