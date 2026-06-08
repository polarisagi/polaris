package policy

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// CapabilityToken — 短寿命 Ed25519 能力令牌（M11 §3.1 + D2 防线）。
// 架构文档: docs/arch/M11-Policy-Safety.md §3.1, §5.2
//
// TTL 默认值（来自 spec/state.yaml §thresholds.m11_policy）:
//   FS:      300s
//   Network: 60s
//   Shell:   30s
//   MCP:     120s
//   Process: 30s

// CapabilityType 能力令牌类型。
type CapabilityType string

const (
	CapFS      CapabilityType = "fs"
	CapNetwork CapabilityType = "network"
	CapShell   CapabilityType = "shell"
	CapMCP     CapabilityType = "mcp"
	CapProcess CapabilityType = "process"
)

// defaultTTLs 各能力类型的默认 TTL（来自架构规约）。
var defaultTTLs = map[CapabilityType]time.Duration{
	CapFS:      300 * time.Second,
	CapNetwork: 60 * time.Second,
	CapShell:   30 * time.Second,
	CapMCP:     120 * time.Second,
	CapProcess: 30 * time.Second,
}

// maxRevokedCap LRU 撤销列表上限（m11_policy.capability_revoke_lru）。
const maxRevokedCap = 1000

var (
	ErrTokenExpired   = perrors.New(perrors.CodeUnauthorized, "policy: capability token expired")
	ErrTokenRevoked   = perrors.New(perrors.CodeUnauthorized, "policy: capability token revoked")
	ErrTokenInvalid   = perrors.New(perrors.CodeUnauthorized, "policy: capability token signature invalid")
	ErrTokenMalformed = perrors.New(perrors.CodeInvalidInput, "policy: capability token malformed")
)

// TokenClaims 是令牌的 JSON 负载。
type TokenClaims struct {
	TokenID   string           `json:"tid"`
	AgentID   string           `json:"aid"`
	Caps      []CapabilityType `json:"caps"`
	IssuedAt  int64            `json:"iat"`
	ExpiresAt int64            `json:"exp"`
}

// Token 是签发后的完整令牌。
type Token struct {
	Claims    TokenClaims
	Signature []byte // Ed25519 签名
}

// TokenManager 管理能力令牌的签发、验证与撤销。
// 每个实例持有一对 Ed25519 密钥对（生产环境应从 OS Keychain 加载）。
type TokenManager struct {
	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey

	// 撤销列表：tokenID → struct{}，LRU 淘汰（FIFO 近似）
	mu      sync.RWMutex
	revoked map[string]struct{}
	revokeQ []string // 用于 LRU FIFO 淘汰
}

// NewTokenManager 创建一个新的令牌管理器，自动生成临时密钥对。
// 生产环境应替换为从 OS Keychain 确定性派生的密钥（persistent_key）。
func NewTokenManager() (*TokenManager, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "capability_token: failed to generate key pair", err)
	}
	return &TokenManager{
		pubKey:  pub,
		privKey: priv,
		revoked: make(map[string]struct{}),
	}, nil
}

// Mint 签发一个新的短寿命能力令牌。
// 若未指定 TTL（传 0），则根据 caps 中优先级最高的能力类型自动选择最短 TTL。
func (tm *TokenManager) Mint(agentID string, caps []CapabilityType, ttl time.Duration) (*Token, error) {
	if agentID == "" || len(caps) == 0 {
		return nil, perrors.New(perrors.CodeInvalidInput, "capability_token: agentID and caps are required")
	}
	if ttl <= 0 {
		// 选取所有能力中最短的 TTL（最小权限原则）
		ttl = tm.minTTL(caps)
	}

	tokenID := generateTokenID()
	claims := TokenClaims{
		TokenID:   tokenID,
		AgentID:   agentID,
		Caps:      caps,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(ttl).Unix(),
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "capability_token: marshal claims", err)
	}

	sig := ed25519.Sign(tm.privKey, payload)
	return &Token{Claims: claims, Signature: sig}, nil
}

// Verify 验证令牌的签名有效性、过期状态及撤销状态。
func (tm *TokenManager) Verify(tok *Token) error {
	if tok == nil {
		return ErrTokenMalformed
	}

	// 1. 验证签名
	payload, err := json.Marshal(tok.Claims)
	if err != nil {
		return ErrTokenMalformed
	}
	if !ed25519.Verify(tm.pubKey, payload, tok.Signature) {
		return ErrTokenInvalid
	}

	// 2. 验证过期
	if time.Now().Unix() > tok.Claims.ExpiresAt {
		return ErrTokenExpired
	}

	// 3. 验证撤销
	tm.mu.RLock()
	_, revoked := tm.revoked[tok.Claims.TokenID]
	tm.mu.RUnlock()
	if revoked {
		return ErrTokenRevoked
	}

	return nil
}

// Revoke 将指定 TokenID 加入撤销列表（LRU FIFO 容量 1000）。
func (tm *TokenManager) Revoke(tokenID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if _, exists := tm.revoked[tokenID]; exists {
		return // 幂等
	}

	// FIFO LRU 淘汰：超出容量时删除最旧的条目
	if len(tm.revokeQ) >= maxRevokedCap {
		oldest := tm.revokeQ[0]
		tm.revokeQ = tm.revokeQ[1:]
		delete(tm.revoked, oldest)
	}

	tm.revoked[tokenID] = struct{}{}
	tm.revokeQ = append(tm.revokeQ, tokenID)
}

// HasCap 检查令牌是否持有指定能力（先经 Verify 验证有效性）。
func (tm *TokenManager) HasCap(tok *Token, cap CapabilityType) (bool, error) {
	if err := tm.Verify(tok); err != nil {
		return false, err
	}
	for _, c := range tok.Claims.Caps {
		if c == cap {
			return true, nil
		}
	}
	return false, nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func (tm *TokenManager) minTTL(caps []CapabilityType) time.Duration {
	min := 300 * time.Second
	for _, c := range caps {
		if ttl, ok := defaultTTLs[c]; ok && ttl < min {
			min = ttl
		}
	}
	return min
}

// generateTokenID 生成唯一令牌 ID（使用随机 8 字节的十六进制编码）。
func generateTokenID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
