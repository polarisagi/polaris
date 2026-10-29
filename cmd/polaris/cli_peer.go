package main

import (
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// runPeerCmd `polaris peer …`：跨节点委派的两两配对与令牌管理（ADR-0111 决策七第 4 条）。
// 全部经守护进程 HTTP（/v1/mesh/*），CLI 不直连数据库（ADR-0096 决策一）。
//
// 配对流程（以 laptop 与 desktop 为例，邀请串由人复制粘贴，人即确认者）：
//
//	laptop$  polaris peer invite desktop          → 打印邀请串 A
//	desktop$ polaris peer accept laptop <A>       → 核对指纹，打印回邀请串 B
//	laptop$  polaris peer accept desktop <B>      → 核对指纹，双向完成
func runPeerCmd(args []string) error {
	if len(args) == 0 {
		printPeerUsage()
		return nil
	}
	if err := cliCheckServer(); err != nil {
		return err
	}
	switch args[0] {
	case "whoami", "self":
		return runPeerWhoami()
	case "invite":
		return runPeerInvite(args[1:])
	case "accept":
		return runPeerAccept(args[1:])
	case "list", "ls":
		return runPeerList()
	case "remove", "rm":
		return runPeerRemove(args[1:])
	case "tokens":
		return runPeerTokens()
	case "revoke":
		if len(args) < 2 {
			return apperr.New(apperr.CodeInvalidInput, "usage: polaris peer revoke <token_id>")
		}
		return cliRequest("DELETE", "/v1/mesh/tokens/"+args[1], nil, nil)
	default:
		printPeerUsage()
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("peer: unknown subcommand %q", args[0]))
	}
}

func printPeerUsage() {
	fmt.Println(`polaris peer — 多台电脑上的 Polaris 互相委派任务（ADR-0111）

  polaris peer whoami                        本节点 ID、指纹与可被访问的地址
  polaris peer invite <对方名> [--url U]     为对方签发邀请串（U：对方访问本机的地址，缺省取 extra_listen）
  polaris peer accept <对方名> <邀请串>      接受对方邀请；未配对过时会打印回邀请串，交给对方 accept
  polaris peer list                          对端列表与在线状态
  polaris peer remove <对方名>               解除配对并吊销本机签发给它的令牌
  polaris peer tokens | revoke <token_id>    查看 / 吊销本机签发的令牌

前提：两台机器在同一 Tailscale 网络或局域网，且已在 config.toml 的 [interface] extra_listen
写入本机地址（如 "100.101.102.103:28888"）。`)
}

func runPeerWhoami() error {
	var resp struct {
		Name        string   `json:"name"`
		NodeID      string   `json:"node_id"`
		Fingerprint string   `json:"fingerprint"`
		URLs        []string `json:"urls"`
		ReadOnly    bool     `json:"peer_read_only"`
	}
	if err := cliGet("/v1/mesh/self", &resp); err != nil {
		return err
	}
	fmt.Printf("名称     %s\n节点 ID  %s\n指纹     %s\n", resp.Name, resp.NodeID, resp.Fingerprint)
	if len(resp.URLs) == 0 {
		fmt.Println(clr(ansiDim, "地址     （无）—— 在 [interface] extra_listen 写入本机 Tailscale/局域网地址后其他节点才能访问"))
	} else {
		fmt.Printf("地址     %s\n", strings.Join(resp.URLs, ", "))
	}
	fmt.Printf("对端任务只读执行：%v\n", resp.ReadOnly)
	return nil
}

// flagValue 取 --name value 形式的参数并返回其余位置参数。
func flagValue(args []string, name string) (string, []string) {
	var rest []string
	val := ""
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			val = args[i+1]
			i++
			continue
		}
		if v, ok := strings.CutPrefix(args[i], name+"="); ok {
			val = v
			continue
		}
		rest = append(rest, args[i])
	}
	return val, rest
}

func runPeerInvite(args []string) error {
	url, rest := flagValue(args, "--url")
	if len(rest) != 1 {
		return apperr.New(apperr.CodeInvalidInput, "usage: polaris peer invite <对方名> [--url http://本机地址:端口]")
	}
	var resp struct {
		Invite      string `json:"invite"`
		Fingerprint string `json:"fingerprint"`
		URL         string `json:"url"`
	}
	if err := cliPost("/v1/mesh/invites", map[string]any{"peer_name": rest[0], "url": url}, &resp); err != nil {
		return err
	}
	fmt.Printf("把下面这行交给 %s，在那台机器上运行：\n\n  polaris peer accept <给本机起的名字> %s\n\n", rest[0], resp.Invite)
	fmt.Printf("对方会看到本机指纹 %s 与地址 %s，请当面或电话核对。\n", clr(ansiOk, resp.Fingerprint), resp.URL)
	fmt.Println(clr(ansiDim, "邀请串含访问本机的令牌，等同密码：不要贴到聊天群或工单里。"))
	return nil
}

func runPeerAccept(args []string) error {
	url, rest := flagValue(args, "--url")
	if len(rest) != 2 {
		return apperr.New(apperr.CodeInvalidInput, "usage: polaris peer accept <对方名> <邀请串> [--url http://本机地址:端口]")
	}
	name := rest[0]
	var resp struct {
		Fingerprint string         `json:"fingerprint"`
		RemoteName  string         `json:"remote_name"`
		Paired      bool           `json:"paired"`
		ReplyInvite string         `json:"reply_invite"`
		MCPServer   map[string]any `json:"mcp_server"`
		Peer        struct {
			NodeID string `json:"node_id"`
			URL    string `json:"url"`
		} `json:"peer"`
	}
	if err := cliPost("/v1/mesh/peers", map[string]any{"name": name, "invite": rest[1], "url": url}, &resp); err != nil {
		return err
	}
	fmt.Printf("%s  已验证 %s（%s）的 Agent Card 签名\n   节点 ID %s\n   指纹    %s ← 请与对方 `polaris peer whoami` 输出核对\n",
		clr(ansiOk, "✓"), name, resp.Peer.URL, resp.Peer.NodeID, clr(ansiOk, resp.Fingerprint))

	// 经既有 /v1/mcp-servers 登记（安装授权、持久化、连接与工具注册都走同一条链路）。先删后建，accept 可重复执行。
	if id, _ := resp.MCPServer["id"].(string); id != "" {
		// 重复 accept 时清掉上一次登记；首次 accept 时本就不存在（404），属预期，不报错。
		if err := cliRequest("DELETE", "/v1/mcp-servers/"+id, nil, nil); err != nil && !strings.Contains(err.Error(), "HTTP 404") {
			fmt.Println(clr(ansiDim, "  （清理旧的委派通道登记失败，将直接覆盖："+err.Error()+"）"))
		}
	}
	if err := cliPost("/v1/mcp-servers", resp.MCPServer, nil); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "对端已登记，但注册其 MCP 连接失败（若提示需要审批：权限模式为 default 时"+
			"需在控制台批准 mcp:"+name+"，或改用 auto_review；之后可重新运行同一条 accept 命令）", err)
	}
	fmt.Printf("%s  已添加委派目标 mcp:%s/default\n", clr(ansiOk, "✓"), name)

	if resp.Paired {
		fmt.Printf("%s  与 %s 的双向配对已完成。\n", clr(ansiOk, "✓"), name)
		return nil
	}
	fmt.Printf("\n还差一步：把下面这行交给 %s，在那台机器上运行：\n\n  polaris peer accept <给本机起的名字> %s\n\n", name, resp.ReplyInvite)
	fmt.Println(clr(ansiDim, "回邀请串含访问本机的令牌，等同密码：不要贴到聊天群或工单里。"))
	return nil
}

func runPeerList() error {
	var resp struct {
		Peers []struct {
			Name         string `json:"name"`
			NodeID       string `json:"node_id"`
			URL          string `json:"url"`
			LastSeenAt   string `json:"last_seen_at"`
			LastError    string `json:"last_error"`
			MCPConnected bool   `json:"mcp_connected"`
		} `json:"peers"`
	}
	if err := cliGet("/v1/mesh/peers?probe=1", &resp); err != nil {
		return err
	}
	if len(resp.Peers) == 0 {
		fmt.Println("还没有对端。用 `polaris peer invite <名字>` 开始配对。")
		return nil
	}
	for _, p := range resp.Peers {
		state := clr(ansiOk, "在线")
		if p.LastError != "" {
			state = clr(ansiDim, "离线（"+p.LastError+"）")
		} else if !p.MCPConnected {
			state = clr(ansiDim, "在线，委派通道未连接（下一分钟自动重连）")
		}
		fmt.Printf("%-12s %-22s %-34s %s\n", p.Name, p.NodeID, p.URL, state)
	}
	return nil
}

func runPeerRemove(args []string) error {
	if len(args) != 1 {
		return apperr.New(apperr.CodeInvalidInput, "usage: polaris peer remove <对方名>")
	}
	var resp struct {
		TokensRevoked int    `json:"tokens_revoked"`
		MCPServerID   string `json:"mcp_server_id"`
	}
	if err := cliRequest("DELETE", "/v1/mesh/peers/"+args[0], nil, &resp); err != nil {
		return err
	}
	if resp.MCPServerID != "" {
		if err := cliRequest("DELETE", "/v1/mcp-servers/"+resp.MCPServerID, nil, nil); err != nil && !strings.Contains(err.Error(), "HTTP 404") {
			fmt.Println(clr(ansiDim, "  委派通道未能自动删除，可在 MCP 设置里手动删除 "+resp.MCPServerID+"："+err.Error()))
		}
	}
	fmt.Printf("%s  已解除与 %s 的配对，吊销令牌 %d 个。对方那边也请运行 `polaris peer remove`。\n",
		clr(ansiOk, "✓"), args[0], resp.TokensRevoked)
	return nil
}

func runPeerTokens() error {
	var resp struct {
		Tokens []struct {
			TokenID    string `json:"token_id"`
			PeerName   string `json:"peer_name"`
			PeerNodeID string `json:"peer_node_id"`
			ExpiresAt  string `json:"expires_at"`
			Revoked    bool   `json:"revoked"`
		} `json:"tokens"`
	}
	if err := cliGet("/v1/mesh/tokens", &resp); err != nil {
		return err
	}
	for _, t := range resp.Tokens {
		state := "有效"
		if t.Revoked {
			state = "已吊销"
		}
		fmt.Printf("%-14s %-12s %-24s 到期 %s  %s\n", t.TokenID, t.PeerName, t.PeerNodeID, t.ExpiresAt, state)
	}
	return nil
}
