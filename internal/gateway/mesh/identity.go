// Package mesh 实现跨节点委派的协议无关核心（ADR-0111）：节点身份、对端令牌、
// 入站委派任务账本与防环校验。A2A 1.0 与 MCP 两个门面（internal/gateway/server/meshapi）
// 都只是本包的协议翻译层——防环、污点、审批边界只在这里实现一次。
package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// nodeKeyFile 节点身份私钥（Ed25519 seed，32 字节）相对数据目录的路径。
//
// 为什么不从 Vault 主密钥派生：`/v1/vault/rotate-master-key` 会换掉主密钥，派生出的
// 节点身份随之改变，所有对端固定（pin）的公钥同时失效，三台机器要重新配对。身份密钥
// 与数据加密密钥生命周期不同，必须独立存放。
const nodeKeyFile = "mesh/node_ed25519.seed"

// Identity 本节点的长期身份：Agent Card 签名密钥 + 由公钥派生的节点 ID。
type Identity struct {
	NodeID     string
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// Fingerprint 公钥指纹（配对时给人核对用，见 ADR-0111 决策七第 4 条）。
func (id *Identity) Fingerprint() string {
	return Fingerprint(id.PublicKey)
}

// PublicKeyB64 公钥的 base64url 形式（写进 Agent Card 扩展参数，供对端首次配对时固定）。
func (id *Identity) PublicKeyB64() string {
	return base64.RawURLEncoding.EncodeToString(id.PublicKey)
}

// Fingerprint 返回公钥 SHA-256 的前 16 字节十六进制，按 4 位分组便于人眼比对。
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	h := hex.EncodeToString(sum[:16])
	out := make([]byte, 0, len(h)+len(h)/4)
	for i := 0; i < len(h); i += 4 {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, h[i:i+4]...)
	}
	return string(out)
}

// NodeIDFromPublicKey 节点 ID = "pn-" + hex(SHA-256(公钥))[:16]。
// 由公钥派生而不是随机生成：拿到对端 Agent Card 的公钥就能独立算出它的节点 ID，
// 对端无法在卡片里谎报一个与签名密钥不符的 ID。
func NodeIDFromPublicKey(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "pn-" + hex.EncodeToString(sum[:8])
}

// GetOrCreateIdentity 读取数据目录下的节点身份，不存在则生成并以 0600 写盘。
func GetOrCreateIdentity(dataDir string) (*Identity, error) {
	path := filepath.Join(dataDir, nodeKeyFile)
	seed, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		seed = make([]byte, ed25519.SeedSize)
		if _, rerr := rand.Read(seed); rerr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "mesh: generate node key", rerr)
		}
		if merr := os.MkdirAll(filepath.Dir(path), 0o700); merr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "mesh: mkdir node key dir", merr)
		}
		if werr := os.WriteFile(path, seed, 0o600); werr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "mesh: write node key", werr)
		}
	} else if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mesh: read node key", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, apperr.New(apperr.CodeInternal, "mesh: node key file corrupted (want 32-byte seed)")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, _ := priv.Public().(ed25519.PublicKey)
	return &Identity{NodeID: NodeIDFromPublicKey(pub), PublicKey: pub, PrivateKey: priv}, nil
}
