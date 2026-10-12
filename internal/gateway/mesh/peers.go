package mesh

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// Peer 本节点登记的一个对端（mesh_peers）。
type Peer struct {
	Name          string    `json:"name"`
	NodeID        string    `json:"node_id"`
	URL           string    `json:"url"`
	PublicKey     string    `json:"public_key"`
	IssuedTokenID string    `json:"issued_token_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	LastSeenAt    time.Time `json:"last_seen_at,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
}

// PublicKeyBytes 解码固定的公钥。
func (p Peer) PublicKeyBytes() (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(p.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, apperr.New(apperr.CodeInvalidInput, "mesh: peer public key is not a base64url Ed25519 key")
	}
	return ed25519.PublicKey(raw), nil
}

// HostPort 规整后的 host:port（SafeDialer 精确放行的键）。
func (p Peer) HostPort() (string, error) {
	return hostPortOf(p.URL)
}

func hostPortOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", apperr.New(apperr.CodeInvalidInput, "mesh: peer url must be http(s)://host[:port], got "+raw)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port), nil
}

// PeerRegistry 对端登记表 + SafeDialer 放行缓存。Allowed 在每次出站拨号时调用，走内存缓存，
// 只在 Upsert/Remove/Refresh 时重建。
type PeerRegistry struct {
	repo    Repository
	mu      sync.RWMutex
	allowed map[string]struct{}
}

// NewPeerRegistry 构造登记表（调用方须随后 Refresh）。
func NewPeerRegistry(r Repository) *PeerRegistry {
	return &PeerRegistry{repo: r, allowed: map[string]struct{}{}}
}

// Refresh 从库重建放行缓存。
func (r *PeerRegistry) Refresh(ctx context.Context) error {
	peers, err := r.List(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]struct{}, len(peers))
	for _, p := range peers {
		if hp, err := p.HostPort(); err == nil {
			next[hp] = struct{}{}
		}
	}
	r.mu.Lock()
	r.allowed = next
	r.mu.Unlock()
	return nil
}

// Allowed SafeDialer 放行判定：只认登记过的精确 host:port（ADR-0111 决策六第 2 条）。
func (r *PeerRegistry) Allowed(host string, port int) bool {
	key := net.JoinHostPort(strings.ToLower(strings.Trim(host, "[]")), strconv.Itoa(port))
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.allowed[key]
	return ok
}

// Upsert 登记或更新对端（按 name）。node_id 已被另一个名字占用时拒绝：同一节点只登记一次。
func (r *PeerRegistry) Upsert(ctx context.Context, p Peer) error {
	if p.Name == "" || p.NodeID == "" {
		return apperr.New(apperr.CodeInvalidInput, "mesh: peer name and node_id are required")
	}
	if _, err := p.HostPort(); err != nil {
		return err
	}
	if _, err := p.PublicKeyBytes(); err != nil {
		return err
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	if err := r.repo.UpsertMeshPeer(ctx, repo.MeshPeerRow{Name: p.Name, NodeID: p.NodeID, URL: p.URL,
		PublicKey: p.PublicKey, IssuedTokenID: p.IssuedTokenID, CreatedAtMs: p.CreatedAt.UnixMilli()}); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "mesh: upsert peer", err)
	}
	return r.Refresh(ctx)
}

// Remove 删除对端登记（不吊销令牌：吊销由调用方显式决定）。
func (r *PeerRegistry) Remove(ctx context.Context, name string) error {
	if err := r.repo.DeleteMeshPeer(ctx, name); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "mesh: remove peer", err)
	}
	return r.Refresh(ctx)
}

// Get 按名字读取；不存在返回 NotFound。
func (r *PeerRegistry) Get(ctx context.Context, name string) (*Peer, error) {
	rows, err := r.repo.ListMeshPeers(ctx, name)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "mesh: get peer", err)
	}
	if len(rows) == 0 {
		return nil, apperr.New(apperr.CodeNotFound, "mesh: no such peer "+name)
	}
	p := peerFromRow(rows[0])
	return &p, nil
}

// List 全部对端（按名字排序）。
func (r *PeerRegistry) List(ctx context.Context) ([]Peer, error) {
	rows, err := r.repo.ListMeshPeers(ctx, "")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "mesh: list peers", err)
	}
	out := make([]Peer, 0, len(rows))
	for _, row := range rows {
		out = append(out, peerFromRow(row))
	}
	return out, nil
}

// MarkSeen 记录一次探活结果（errMsg 为空 = 成功）。探活状态只是展示信息，写失败只留痕。
func (r *PeerRegistry) MarkSeen(ctx context.Context, name, errMsg string) {
	if err := r.repo.UpdateMeshPeerSeen(ctx, name, time.Now().UnixMilli(), errMsg); err != nil {
		slog.Warn("mesh: record peer liveness failed", "peer", name, "err", err)
	}
}

func peerFromRow(row repo.MeshPeerRow) Peer {
	p := Peer{Name: row.Name, NodeID: row.NodeID, URL: row.URL, PublicKey: row.PublicKey,
		IssuedTokenID: row.IssuedTokenID, CreatedAt: time.UnixMilli(row.CreatedAtMs), LastError: row.LastError}
	if row.LastSeenAtMs > 0 {
		p.LastSeenAt = time.UnixMilli(row.LastSeenAtMs)
	}
	return p
}
