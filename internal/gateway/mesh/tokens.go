package mesh

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// TokenPrefix 对端令牌的固定前缀。鉴权中间件据此把请求路由到对端令牌校验，
// 不去和 POLARIS_API_KEY / 本地令牌做比较，也让泄露扫描工具能识别它。
const TokenPrefix = "pmt_"

// 对端令牌的权限范围（ADR-0111 决策三）。没有任何 scope 能触及 /a2a/v1、/mcp 之外的路由——
// 路由白名单在中间件层强制，scope 只区分门面内部的读/写/取消。
const (
	ScopeSend   = "a2a:send"
	ScopeRead   = "a2a:read"
	ScopeCancel = "a2a:cancel"
)

// AllScopes 默认签发的全部 scope。
func AllScopes() []string { return []string{ScopeSend, ScopeRead, ScopeCancel} }

// defaultTokenTTL 未指定有效期时的默认值。强制有限期：永不过期的凭证会在三台机器的
// 配置里一直躺着，丢一台就等于永久泄露。
const defaultTokenTTL = 365 * 24 * time.Hour

// 令牌校验失败的几类原因。对外一律 401，区分只为日志与测试。
var (
	ErrTokenUnknown = apperr.New(apperr.CodeUnauthorized, "mesh: unknown peer token")
	ErrTokenExpired = apperr.New(apperr.CodeUnauthorized, "mesh: peer token expired")
	ErrTokenRevoked = apperr.New(apperr.CodeUnauthorized, "mesh: peer token revoked")
)

// Principal 一次对端请求的已验证身份。
type Principal struct {
	TokenID    string
	PeerName   string
	PeerNodeID string
	Scopes     []string
}

// Has 判断是否持有某个 scope。
func (p Principal) Has(scope string) bool { return slices.Contains(p.Scopes, scope) }

// TokenInfo 令牌元数据（列表展示用，不含明文与摘要）。
type TokenInfo struct {
	TokenID    string    `json:"token_id"`
	PeerName   string    `json:"peer_name"`
	PeerNodeID string    `json:"peer_node_id"`
	Scopes     []string  `json:"scopes"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Revoked    bool      `json:"revoked"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
}

// TokenStore 对端令牌账本（mesh_peer_tokens）。库内只存 SHA-256 摘要，明文只在签发时出现一次。
type TokenStore struct {
	repo Repository
	now  func() time.Time
}

// NewTokenStore 构造令牌账本。
func NewTokenStore(r Repository) *TokenStore {
	return &TokenStore{repo: r, now: time.Now}
}

func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Issue 签发令牌，返回明文（只此一次）与元数据。peerNodeID 为空时按外部 Agent 处理（ext:<name>）。
func (s *TokenStore) Issue(ctx context.Context, peerName, peerNodeID string, scopes []string, ttl time.Duration) (string, *TokenInfo, error) {
	peerName = strings.TrimSpace(peerName)
	if peerName == "" {
		return "", nil, apperr.New(apperr.CodeInvalidInput, "mesh: peer name is required")
	}
	if peerNodeID == "" {
		peerNodeID = "ext:" + peerName
	}
	if len(scopes) == 0 {
		scopes = AllScopes()
	}
	for _, sc := range scopes {
		if !slices.Contains(AllScopes(), sc) {
			return "", nil, apperr.New(apperr.CodeInvalidInput, "mesh: unknown scope "+sc)
		}
	}
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, apperr.Wrap(apperr.CodeInternal, "mesh: generate token", err)
	}
	plain := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	info := &TokenInfo{
		TokenID: "ptk-" + uuid.NewString()[:8], PeerName: peerName, PeerNodeID: peerNodeID,
		Scopes: scopes, CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	row := repo.MeshTokenRow{TokenID: info.TokenID, PeerName: peerName, PeerNodeID: peerNodeID,
		Scopes: strings.Join(scopes, ","), CreatedAtMs: now.UnixMilli(), ExpiresAtMs: info.ExpiresAt.UnixMilli()}
	if err := s.repo.InsertMeshToken(ctx, row, hashToken(plain)); err != nil {
		return "", nil, apperr.Wrap(apperr.CodeOf(err), "mesh: issue peer token", err)
	}
	return plain, info, nil
}

// Verify 校验明文令牌并返回身份。成功时顺带刷新 last_used_at（失败只记不阻断）。
func (s *TokenStore) Verify(ctx context.Context, plain string) (*Principal, error) {
	if !strings.HasPrefix(plain, TokenPrefix) {
		return nil, ErrTokenUnknown
	}
	row, err := s.repo.FindMeshTokenByHash(ctx, hashToken(plain))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mesh: lookup peer token", err)
	}
	if row == nil {
		return nil, ErrTokenUnknown
	}
	now := s.now()
	if row.RevokedAtMs != 0 {
		return nil, ErrTokenRevoked
	}
	if now.UnixMilli() >= row.ExpiresAtMs {
		return nil, ErrTokenExpired
	}
	if err := s.repo.TouchMeshToken(ctx, row.TokenID, now.UnixMilli()); err != nil {
		// last_used_at 仅供 `polaris peer tokens` 展示，写失败不应拒绝一次合法鉴权。
		slog.Warn("mesh: update token last_used_at failed", "token_id", row.TokenID, "err", err)
	}
	if row.LastUsedAtMs == 0 && !strings.HasPrefix(row.PeerNodeID, "pending:") {
		s.revokeOlderOnFirstUse(ctx, row)
	}
	return &Principal{TokenID: row.TokenID, PeerName: row.PeerName, PeerNodeID: row.PeerNodeID,
		Scopes: strings.Split(row.Scopes, ",")}, nil
}

// revokeOlderOnFirstUse 某节点第一次用上一枚新令牌，即证明它已换用新令牌：签发给同一节点、
// 更早的令牌随之作废（重新配对后旧令牌不再悬空到期满）。失败只记日志，不影响本次鉴权。
func (s *TokenStore) revokeOlderOnFirstUse(ctx context.Context, used *repo.MeshTokenRow) {
	rows, err := s.repo.ListMeshTokens(ctx)
	if err != nil {
		slog.Warn("mesh: list tokens for supersede failed", "err", err)
		return
	}
	for _, r := range rows {
		if r.RevokedAtMs != 0 || r.PeerNodeID != used.PeerNodeID || r.TokenID == used.TokenID || r.CreatedAtMs >= used.CreatedAtMs {
			continue
		}
		if err := s.Revoke(ctx, r.TokenID); err != nil {
			slog.Warn("mesh: revoke superseded token failed", "token_id", r.TokenID, "err", err)
			continue
		}
		slog.Info("mesh: superseded token revoked on first use of a newer one", "token_id", r.TokenID, "peer", r.PeerName)
	}
}

// Revoke 吊销令牌（幂等）。没有这个 token_id 时返回 NotFound。
func (s *TokenStore) Revoke(ctx context.Context, tokenID string) error {
	if err := s.repo.RevokeMeshToken(ctx, tokenID, s.now().UnixMilli()); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "mesh: revoke peer token", err)
	}
	return nil
}

// List 列出全部令牌（含已吊销/过期，按签发时间倒序）。
func (s *TokenStore) List(ctx context.Context) ([]TokenInfo, error) {
	rows, err := s.repo.ListMeshTokens(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "mesh: list peer tokens", err)
	}
	out := make([]TokenInfo, 0, len(rows))
	for _, r := range rows {
		t := TokenInfo{TokenID: r.TokenID, PeerName: r.PeerName, PeerNodeID: r.PeerNodeID,
			Scopes: strings.Split(r.Scopes, ","), CreatedAt: time.UnixMilli(r.CreatedAtMs),
			ExpiresAt: time.UnixMilli(r.ExpiresAtMs), Revoked: r.RevokedAtMs != 0}
		if r.LastUsedAtMs > 0 {
			t.LastUsedAt = time.UnixMilli(r.LastUsedAtMs)
		}
		out = append(out, t)
	}
	return out, nil
}

// BindPendingToken 把配对时预签发、尚未绑定节点的令牌（peer_node_id = pending:<name>）
// 绑定到对方 accept 后才知道的节点 ID。返回被绑定的 token_id 与是否发生了绑定。
func (s *TokenStore) BindPendingToken(ctx context.Context, peerName, nodeID string) (string, bool, error) {
	tokenID, err := s.repo.RebindMeshToken(ctx, peerName, PendingNodeID(peerName), nodeID)
	if err != nil {
		return "", false, apperr.Wrap(apperr.CodeOf(err), "mesh: bind pending token", err)
	}
	return tokenID, tokenID != "", nil
}

// Supersede 吊销签发给 peerName 的其余有效令牌（keep 除外），返回吊销数。pendingOnly 时只动
// 尚未绑定节点的令牌。重复 invite / accept 时旧令牌不再有人持有却一直有效到期满——双实例实测里
// 两轮配对后各留下 2~3 枚孤儿令牌，故在新令牌生效的时刻收口。
func (s *TokenStore) Supersede(ctx context.Context, peerName, keep string, pendingOnly bool) (int, error) {
	tokens, err := s.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tokens {
		if t.Revoked || t.TokenID == keep || t.PeerName != peerName {
			continue
		}
		if pendingOnly && t.PeerNodeID != PendingNodeID(peerName) {
			continue
		}
		if err := s.Revoke(ctx, t.TokenID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// PendingNodeID 待绑定令牌的占位节点 ID。持有它的令牌发起的委派若携带委派链，链尾不可能等于
// 占位值，会被 ValidateChain 拒绝——即未完成双向配对前，Polaris 节点间的委派不会被受理。
func PendingNodeID(peerName string) string { return "pending:" + peerName }
