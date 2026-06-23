package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
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

// maxRevokedCap LRU 撤销列表上限（m11_policy.capability_revoke_lru）。
const maxRevokedCap = 1000

var (
	ErrTokenExpired   = apperr.New(apperr.CodeUnauthorized, "policy: capability token expired")
	ErrTokenRevoked   = apperr.New(apperr.CodeUnauthorized, "policy: capability token revoked")
	ErrTokenInvalid   = apperr.New(apperr.CodeUnauthorized, "policy: capability token signature invalid")
	ErrTokenMalformed = apperr.New(apperr.CodeInvalidInput, "policy: capability token malformed")
)

// TokenClaims 是令牌的 JSON 负载。
type TokenClaims struct {
	TokenID         string           `json:"tid"`
	AgentID         string           `json:"aid"`
	Caps            []CapabilityType `json:"caps"`
	SandboxTier     int              `json:"sandbox_tier"`
	IssuedAt        int64            `json:"iat"`
	ExpiresAt       int64            `json:"exp"`
	MaxCallsPerTask int              `json:"max_calls_per_task,omitempty"` // 0 = 无限制
	DelegatedFrom   string           `json:"delegated_from,omitempty"`     // 父令牌 TokenID，空 = 根令牌
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
		return nil, apperr.Wrap(apperr.CodeInternal, "capability_token: failed to generate key pair", err)
	}
	return &TokenManager{
		pubKey:  pub,
		privKey: priv,
		revoked: make(map[string]struct{}),
	}, nil
}

// Mint 签发新的能力令牌。
func (tm *TokenManager) Mint(agentID string, caps []CapabilityType, sandboxTier int, ttl time.Duration, maxCallsPerTask int) (*Token, error) {
	if len(caps) == 0 {
		return nil, apperr.New(apperr.CodeInvalidInput, "policy: empty capabilities")
	}
	if agentID == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "capability_token: agentID is required")
	}
	if ttl <= 0 {
		// 选取所有能力中最短的 TTL（最小权限原则）
		ttl = tm.minTTL(caps)
	}

	tokenID := generateTokenID()
	claims := TokenClaims{
		TokenID:         tokenID,
		AgentID:         agentID,
		Caps:            caps,
		SandboxTier:     sandboxTier,
		IssuedAt:        time.Now().Unix(),
		ExpiresAt:       time.Now().Add(ttl).Unix(),
		MaxCallsPerTask: maxCallsPerTask,
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "capability_token: marshal claims", err)
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
		return false, apperr.Wrap(apperr.CodeInternal, "TokenManager.HasCap", err)
	}
	for _, c := range tok.Claims.Caps {
		if c == cap {
			return true, nil
		}
	}
	return false, nil
}

// Delegate 从父令牌派生子令牌（能力衰减原则，M11 §3.1）：
//   - 能力集：caps ∩ parent.Caps（子不可超出父授权范围）
//   - TTL：min(父剩余 TTL / 2, 请求 TTL)（每层委派缩短有效期）
//   - SandboxTier：继承父值（只升不降）
//   - MaxCallsPerTask：继承父值（0=无限制时同步）
func (tm *TokenManager) Delegate(parent *Token, agentID string, caps []CapabilityType, targetSandboxTier int, ttl time.Duration) (*Token, error) {
	if err := tm.Verify(parent); err != nil {
		return nil, apperr.Wrap(apperr.CodeUnauthorized, "capability_token: delegate parent invalid", err)
	}
	if agentID == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "capability_token: delegate agentID required")
	}

	// 沙箱单调：如果目标小于父级，至少维持父级
	if targetSandboxTier < parent.Claims.SandboxTier {
		targetSandboxTier = parent.Claims.SandboxTier
	}

	// 能力衰减：取交集
	parentCapSet := make(map[CapabilityType]struct{}, len(parent.Claims.Caps))
	for _, c := range parent.Claims.Caps {
		parentCapSet[c] = struct{}{}
	}
	var delegatedCaps []CapabilityType
	for _, c := range caps {
		if _, ok := parentCapSet[c]; ok {
			delegatedCaps = append(delegatedCaps, c)
		}
	}
	if len(delegatedCaps) == 0 {
		return nil, apperr.New(apperr.CodeForbidden, "capability_token: no capabilities in intersection with parent")
	}

	// TTL 衰减：最多取父剩余时间的一半
	remaining := time.Duration(parent.Claims.ExpiresAt-time.Now().Unix()) * time.Second
	maxTTL := remaining / 2
	if ttl <= 0 || ttl > maxTTL {
		ttl = maxTTL
	}
	if ttl <= 0 {
		return nil, apperr.New(apperr.CodeUnauthorized, "capability_token: parent token nearly expired, delegation refused")
	}

	tokenID := generateTokenID()
	claims := TokenClaims{
		TokenID:         tokenID,
		AgentID:         agentID,
		Caps:            delegatedCaps,
		SandboxTier:     targetSandboxTier,
		IssuedAt:        time.Now().Unix(),
		ExpiresAt:       time.Now().Add(ttl).Unix(),
		MaxCallsPerTask: parent.Claims.MaxCallsPerTask,
		DelegatedFrom:   parent.Claims.TokenID,
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "capability_token: marshal delegate claims", err)
	}
	sig := ed25519.Sign(tm.privKey, payload)
	return &Token{Claims: claims, Signature: sig}, nil
}

// ValidateDelegation 验证 child 确实是从 parent 合法派生的子令牌（M11 §3.1）：
//  1. parent 和 child 签名均合法、均未过期未撤销
//  2. child.DelegatedFrom == parent.TokenID
//  3. child.Caps ⊆ parent.Caps（子不可超出父授权范围）
//  4. child.ExpiresAt ≤ parent.ExpiresAt（子生命周期不超过父）
//  5. child.SandboxTier ≥ parent.SandboxTier（沙箱级别只升不降）
func (tm *TokenManager) ValidateDelegation(parent, child *Token) error {
	if err := tm.Verify(parent); err != nil {
		return apperr.Wrap(apperr.CodeUnauthorized, "capability_token: delegation parent invalid", err)
	}
	if err := tm.Verify(child); err != nil {
		return apperr.Wrap(apperr.CodeUnauthorized, "capability_token: delegation child invalid", err)
	}

	if child.Claims.DelegatedFrom != parent.Claims.TokenID {
		return apperr.New(apperr.CodeUnauthorized,
			fmt.Sprintf("capability_token: child.DelegatedFrom=%q != parent.TokenID=%q",
				child.Claims.DelegatedFrom, parent.Claims.TokenID))
	}

	parentCapSet := make(map[CapabilityType]struct{}, len(parent.Claims.Caps))
	for _, c := range parent.Claims.Caps {
		parentCapSet[c] = struct{}{}
	}
	for _, c := range child.Claims.Caps {
		if _, ok := parentCapSet[c]; !ok {
			return apperr.New(apperr.CodeForbidden,
				fmt.Sprintf("capability_token: child capability %q not in parent scope", c))
		}
	}

	if child.Claims.ExpiresAt > parent.Claims.ExpiresAt {
		return apperr.New(apperr.CodeForbidden, "capability_token: child expiry exceeds parent expiry")
	}

	if child.Claims.SandboxTier < parent.Claims.SandboxTier {
		return apperr.New(apperr.CodeForbidden, "capability_token: child sandbox tier is less restrictive than parent")
	}

	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func (tm *TokenManager) minTTL(caps []CapabilityType) time.Duration {
	// defaultTTLs 各能力类型的默认 TTL（来自架构规约）。
	defaultTTLs := map[CapabilityType]time.Duration{
		CapFS:      300 * time.Second,
		CapNetwork: 60 * time.Second,
		CapShell:   30 * time.Second,
		CapMCP:     120 * time.Second,
		CapProcess: 30 * time.Second,
	}
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
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return fmt.Sprintf("%x", b)
}
