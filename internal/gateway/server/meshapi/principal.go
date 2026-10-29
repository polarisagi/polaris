// Package meshapi 是跨节点委派的两个协议门面（ADR-0111）：
//   - /a2a/v1  A2A 1.0 JSON-RPC 绑定（官方 SDK a2asrv 负责传输与错误编码），面向外部 Agent；
//   - /mcp     MCP 2026-07-28 无状态服务端，暴露 a2a_delegate / a2a_list_agents，面向其他 Polaris 节点
//     （ADR-0084 出站契约）与 Claude 等上层调度者。
//
// 两个门面都只做协议翻译，委派的受理、防环、留存、取消全部在 internal/gateway/mesh。
package meshapi

import (
	"context"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/mesh"
)

// principalFrom 把鉴权中间件写入的身份转成委派服务的调用方。
//
// 对端令牌（ClientTypePeer）→ 令牌所属节点与 scope；本机令牌 / POLARIS_API_KEY → 以 local:<user>
// 身份持全部 scope（运维与联调用：a2a-cli、A2A Inspector 可以直接拿 API Key 对本节点做发现与发消息）。
// 未通过鉴权的请求到不了这里（中间件已 401），这里仍按 fail-closed 返回 false。
func principalFrom(ctx context.Context) (mesh.Principal, bool) {
	ac := authcontext.FromContext(ctx)
	if !ac.Authenticated {
		return mesh.Principal{}, false
	}
	if ac.ClientType == authcontext.ClientTypePeer {
		return mesh.Principal{TokenID: ac.PeerTokenID, PeerName: ac.PeerName, PeerNodeID: ac.PeerNodeID,
			Scopes: ac.PeerScopes}, true
	}
	return mesh.Principal{PeerName: "local", PeerNodeID: "local:" + ac.UserID, Scopes: mesh.AllScopes()}, true
}
