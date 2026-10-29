package mesh

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// InvitePrefix 邀请串前缀，便于用户与日志识别。
const InvitePrefix = "polaris-invite:"

// Invite 两两配对的邀请（ADR-0111 决策七第 4 条）。邀请方把它交给对方（由人复制粘贴，
// 人就是这一步的确认者），对方据此：固定邀请方公钥、登记邀请方地址、拿到访问邀请方的令牌。
//
// 邀请串里含明文令牌，等同于凭证：只经人手传递，不落日志，不经第三方服务。
type Invite struct {
	Version   int    `json:"v"`
	Name      string `json:"name"`        // 邀请方自称（Agent Card name），仅供展示
	NodeID    string `json:"node_id"`     // 邀请方节点 ID
	PublicKey string `json:"public_key"`  // 邀请方公钥（base64url）
	URL       string `json:"url"`         // 对方应如何访问邀请方
	Token     string `json:"token"`       // 邀请方签发给对方的令牌
	ForNodeID string `json:"for_node_id"` // 令牌已绑定的对方节点 ID；空 = 待对方 accept 后由邀请方绑定
}

// Encode 编码为可复制粘贴的单行串。
func (inv Invite) Encode() string {
	raw, _ := json.Marshal(inv) //nolint:errchkjson // 固定结构
	return InvitePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// Fingerprint 邀请方公钥指纹（给人核对）。
func (inv Invite) Fingerprint() string {
	raw, err := base64.RawURLEncoding.DecodeString(inv.PublicKey)
	if err != nil {
		return "?"
	}
	return Fingerprint(raw)
}

// DecodeInvite 解码并做结构校验：节点 ID 必须由公钥派生（防止邀请里谎报 ID）。
func DecodeInvite(s string) (*Invite, error) {
	body, ok := strings.CutPrefix(strings.TrimSpace(s), InvitePrefix)
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidInput, "mesh: not a polaris invite (missing "+InvitePrefix+" prefix)")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "mesh: invite is corrupted (base64)")
	}
	var inv Invite
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "mesh: invite is corrupted (json)")
	}
	if inv.Version != 1 {
		return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("mesh: unsupported invite version %d", inv.Version))
	}
	pub, err := base64.RawURLEncoding.DecodeString(inv.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, apperr.New(apperr.CodeInvalidInput, "mesh: invite public key invalid")
	}
	if NodeIDFromPublicKey(pub) != inv.NodeID {
		return nil, apperr.New(apperr.CodeInvalidInput, "mesh: invite node_id does not match its public key")
	}
	if !strings.HasPrefix(inv.Token, TokenPrefix) {
		return nil, apperr.New(apperr.CodeInvalidInput, "mesh: invite carries no peer token")
	}
	if _, err := hostPortOf(inv.URL); err != nil {
		return nil, err
	}
	return &inv, nil
}

var peerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidPeerName 对端名同时是 MCP Server 名（mcp:<name>/<agent> 寻址），限定为小写字母数字与连字符。
func ValidPeerName(name string) error {
	if !peerNameRe.MatchString(name) {
		return apperr.New(apperr.CodeInvalidInput, "mesh: peer name must match [a-z0-9][a-z0-9-]{0,31}, got "+name)
	}
	return nil
}

// MCPServerID 对端在 mcp_servers 表里的固定 ID（配对与解除配对两端据此找到同一行）。
func MCPServerID(peerName string) string { return "mesh_" + peerName }

// GetVerifiedCard 拉取对端 Agent Card，并用**固定公钥**验签（ADR-0111 决策六第 3 条，不做 TOFU）：
// 签名不符、卡上扩展声明的 node_id 与固定公钥派生值不符，一律拒绝。
// client 必须是经 SafeDialer 的客户端，且对端 host:port 已在登记表中（否则会被 SSRF 防护拦截）。
func GetVerifiedCard(ctx context.Context, client *http.Client, baseURL string, pinned ed25519.PublicKey) (*a2a.AgentCard, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/.well-known/agent-card.json", nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mesh: build card request", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNetworkUnavailable, "mesh: peer unreachable", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeNetworkUnavailable, fmt.Sprintf("mesh: peer card http %d", resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNetworkUnavailable, "mesh: read peer card", err)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mesh: peer card is not A2A 1.0 JSON", err)
	}
	if len(card.Signatures) == 0 {
		return nil, apperr.New(apperr.CodeForbidden, "mesh: peer card is unsigned")
	}
	verifier := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: a2acrypto.KeyResolverFunc(
		func(context.Context, string) (crypto.PublicKey, error) { return pinned, nil })})
	if err := verifier.Verify(ctx, raw, &card.Signatures[0]); err != nil {
		return nil, apperr.Wrap(apperr.CodeForbidden, "mesh: peer card signature does not match the pinned key", err)
	}
	want := NodeIDFromPublicKey(pinned)
	for _, ext := range card.Capabilities.Extensions {
		if ext.URI == DelegationExtensionURI {
			if id, _ := ext.Params["node_id"].(string); id != want {
				return nil, apperr.New(apperr.CodeForbidden, "mesh: peer card node_id does not match the pinned key")
			}
			return &card, nil
		}
	}
	return nil, apperr.New(apperr.CodeForbidden, "mesh: peer card lacks the Polaris delegation extension")
}
