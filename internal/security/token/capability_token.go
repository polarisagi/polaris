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

	// 已签发列表：仅用于通过 ID 进行无状态回源查找（内存缓存）。
	// 2026-07-11 复核修复：原实现只 Mint/Delegate 时写入，从未清理，任何持续签发
	// 短寿命 token 的调用方（如 CodeAct 每次执行都 Lookup 一次已签发 token）会导致
	// 该 map 无界增长——是一个真实的内存泄漏/DoS 面，不是理论风险。仿照 revoked/
	// revokeQ 的 FIFO 上限模式加界，并在 Lookup 命中过期 token 时惰性清理。
	issued  map[string]*Token
	issuedQ []string // 用于 issued 的 FIFO 淘汰，容量同 maxRevokedCap
}

// maxIssuedCap issued 缓存 FIFO 上限，防止无界增长（与 maxRevokedCap 保持一致量级）。
const maxIssuedCap = 1000

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
		issued:  make(map[string]*Token),
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
	tok := &Token{Claims: claims, Signature: sig}

	tm.recordIssued(tokenID, tok)

	return tok, nil
}

// recordIssued 将新签发/委派的 token 计入 issued 缓存，超出 maxIssuedCap 时
// FIFO 淘汰最旧条目（与 Revoke 的 revokeQ 模式一致），避免无界内存增长。
func (tm *TokenManager) recordIssued(tokenID string, tok *Token) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if len(tm.issuedQ) >= maxIssuedCap {
		oldest := tm.issuedQ[0]
		tm.issuedQ = tm.issuedQ[1:]
		delete(tm.issued, oldest)
	}
	tm.issued[tokenID] = tok
	tm.issuedQ = append(tm.issuedQ, tokenID)
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

// Lookup 通过 tokenID 查找已签发的完整 Token（无状态回源）。
// 如果系统重启导致内存清空，或 token 已因 FIFO 容量淘汰/过期被清理，将返回
// Not Found，触发调用方重新申请或拦截执行（fail-closed，不做静默降级）。
func (tm *TokenManager) Lookup(tokenID string) (*Token, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tok, ok := tm.issued[tokenID]
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "capability_token: token not found")
	}
	// 惰性清理：命中已过期 token 时顺手从缓存移除，减轻长期运行下的内存占用，
	// 不依赖 FIFO 上限单独兜底。不影响返回语义——过期判断仍以 Verify 为准。
	if time.Now().Unix() > tok.Claims.ExpiresAt {
		delete(tm.issued, tokenID)
		return nil, apperr.New(apperr.CodeNotFound, "capability_token: token not found")
	}
	return tok, nil
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

// Validate 校验 Capability Token 的合法性，使用注入的校验闭包。
// 解决包循环依赖问题：由 boot 或 caller 注入 Verify 函数。
// 2026-07-14（ADR-0051）：Validate（包级函数）删除——生产代码统一走
// TokenManager.Verify(tok)，toolName 参数从未被函数体使用，是"为解决尚未发生的
// 循环依赖而预写、但从未真正需要"的投机性代码，全仓零调用点。
