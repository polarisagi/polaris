package session

import (
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ownerChannels 是由机器主人本人在本机直接操作的客户端渠道。外部渠道（telegram、webhook、
// cron、plugin_channel、workflow 等）永远不在其中——其输入可能来自任意第三方。
func ownerChannels() map[string]bool {
	return map[string]bool{"web": true, "cli": true, "desktop": true}
}

// inputTaintLevel 决定本轮用户输入的污点等级（ADR-0007 修订：owner 本人输入不再视为外部不可信）。
// 必须同时满足：HTTP 边界依据鉴权上下文置位 OwnerTrusted，且渠道是本地客户端；否则一律 TaintHigh。
// 信任只描述“这段话出自 owner”，工具输出、网页内容、记忆召回等仍按各自来源定级。
func inputTaintLevel(req Request) types.TaintLevel {
	if req.OwnerTrusted && ownerChannels()[req.Channel] {
		return types.TaintLow
	}
	return types.TaintHigh
}

// ownerSteerer 是 Agent / Pool 可选实现的 owner 插话入口（保持 protocol 接口与既有 mock 不变）。
type ownerSteerer interface{ SteerOwner(text string) bool }
type ownerPoolSteerer interface {
	TrySteerOwner(sessionID, text string) bool
}

// steerAgent 向运行中的 Agent 插话：owner 本人的插话走 owner 入口（TaintLow），其余 TaintHigh。
func steerAgent(a protocol.AgentController, req Request) bool {
	if inputTaintLevel(req) == types.TaintLow {
		if os, ok := a.(ownerSteerer); ok {
			return os.SteerOwner(req.Input)
		}
	}
	return a.Steer(req.Input)
}

// trySteerPool 尝试经 Pool 把输入作为插话送入会话内运行中的循环。
func (o *orchestrator) trySteerPool(sessionID string, req Request) bool {
	if inputTaintLevel(req) == types.TaintLow {
		if p, ok := o.agentPool.(ownerPoolSteerer); ok {
			return p.TrySteerOwner(sessionID, req.Input)
		}
	}
	if p, ok := o.agentPool.(interface{ TrySteer(string, string) bool }); ok {
		return p.TrySteer(sessionID, req.Input)
	}
	return false
}
