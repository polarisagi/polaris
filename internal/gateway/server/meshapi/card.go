package meshapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/mesh"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/version"
)

// A2APath A2A 1.0 JSON-RPC 端点（路径无 /v1 前缀：A2A 自带版本协商，不靠路径区分版本）。
const A2APath = "/a2a/v1"

// MCPPath MCP 无状态 Streamable HTTP 端点。
const MCPPath = "/mcp"

// BuildAgentCard 构造公开 Agent Card（未签名）。baseURL 为对外可见的源（scheme://host[:port]）。
//
// 公开卡只含名称、技能、接口与鉴权方式，不列内部工具清单。节点身份（node_id 与签名公钥）
// 放在 Polaris 委派扩展的 params 里：对端首次配对时由人核对指纹后固定公钥，此后每次拉卡都用
// 固定公钥验签，不做 TOFU（ADR-0111 决策六第 3 条）。
func BuildAgentCard(cfg config.A2AConfig, self *mesh.Identity, baseURL string) *a2a.AgentCard {
	defs := cfg.CardSkills()
	skills := make([]a2a.AgentSkill, 0, len(defs))
	for _, s := range defs {
		tags := s.Tags
		if tags == nil {
			tags = []string{}
		}
		skills = append(skills, a2a.AgentSkill{ID: s.ID, Name: s.Name, Description: s.Description,
			Tags: tags, Examples: s.Examples})
	}
	desc := cfg.Description
	if desc == "" {
		desc = "Polaris agent node"
	}
	return &a2a.AgentCard{
		Name:        cfg.Name,
		Description: desc,
		Version:     version.Version,
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(strings.TrimRight(baseURL, "/")+A2APath, a2a.TransportProtocolJSONRPC),
		},
		Capabilities: a2a.AgentCapabilities{
			Streaming:         false,
			PushNotifications: false,
			ExtendedAgentCard: false,
			Extensions: []a2a.AgentExtension{{
				URI:         mesh.DelegationExtensionURI,
				Description: "Polaris cross-node delegation: carries spawn_depth and the node chain for loop prevention",
				Required:    false,
				Params: map[string]any{
					"node_id":     self.NodeID,
					"public_key":  self.PublicKeyB64(),
					"fingerprint": self.Fingerprint(),
				},
			}},
		},
		DefaultInputModes:  []string{"text/plain", "application/json"},
		DefaultOutputModes: []string{"text/plain"},
		SecuritySchemes: a2a.NamedSecuritySchemes{
			"bearer": a2a.HTTPAuthSecurityScheme{Scheme: "Bearer", Description: "Polaris peer token (pmt_…) or POLARIS_API_KEY"},
		},
		SecurityRequirements: a2a.SecurityRequirementsOptions{{"bearer": a2a.SecuritySchemeScopes{}}},
		Skills:               skills,
	}
}

// SignAgentCard 用节点身份密钥对卡片签名（JWS over JCS，签名范围不含 signatures 字段，
// 由 a2acrypto 实现）。kid = 节点 ID。
func SignAgentCard(ctx context.Context, card *a2a.AgentCard, self *mesh.Identity) error {
	signer, err := a2acrypto.NewSigner(a2acrypto.SignerConfig{PrivateKey: self.PrivateKey, KeyID: self.NodeID})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "meshapi: create card signer", err)
	}
	card.Signatures = nil
	raw, err := json.Marshal(card)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "meshapi: marshal agent card", err)
	}
	sig, err := signer.Sign(ctx, raw)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "meshapi: sign agent card", err)
	}
	card.Signatures = []a2a.AgentCardSignature{*sig}
	return nil
}

// cardHandler GET /.well-known/agent-card.json（免鉴权）。
// baseURL 优先取配置 advertised_url（反向代理 / TLS 终结场景），否则按请求 Host 推导。
func cardHandler(cfg config.A2AConfig, self *mesh.Identity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base := cfg.AdvertisedURL
		if base == "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			base = scheme + "://" + r.Host
		}
		card := BuildAgentCard(cfg, self, base)
		if err := SignAgentCard(r.Context(), card, self); err != nil {
			slog.Error("meshapi: sign agent card failed", "err", err)
			http.Error(w, "failed to sign agent card", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		if err := json.NewEncoder(w).Encode(card); err != nil {
			slog.Warn("meshapi: write agent card failed", "err", err)
		}
	}
}
