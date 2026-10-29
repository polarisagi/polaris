package network

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// local_only Allowlist 签名加载器（2026-07-21 deadcode 审查补齐，M11 §5.3）。
//
// 文件约定：
//   - config/local_only_network_allowlist.toml — 运营手写的白名单（[[entry]] 数组），
//     不含签名字段本身。
//   - config/local_only_network_allowlist.toml.sig — `polaris allowlist sign` 对上述
//     文件原始字节的 Ed25519 签名，base64 编码文本。
//
// 配套 CLI: cmd/polaris/cli_allowlist.go（polaris allowlist genkey/sign），
// 私钥永远只存在于运营本地磁盘，服务器侧只需要通过
// POLARIS_LOCAL_ONLY_ALLOWLIST_PUBKEY 环境变量配置公钥（同 cli_eval.go 的
// meta_auditor 密钥隔离原则）。

// allowlistFile TOML 顶层结构。
type allowlistFile struct {
	Entry []AllowlistEntry `toml:"entry"`
}

// ListSignedAllowlistEntries 读取 path 并用 pubKeyB64 验证配套 .sig 签名后解析为条目列表。
//
// Fail-closed 语义（HE-2 可验证执行——安全边界必须密码学可验证）：
//   - path 不存在 → (nil, nil)：视为"运营未配置白名单"，不是错误，local_only
//     照常全阻断非 loopback 出站，行为与从未实现此加载器时一致。
//   - path 存在但 pubKeyB64 为空 / 格式非法 / .sig 缺失 / 验签失败 → 返回 error。
//     调用方（boot 阶段）必须把这当致命错误处理，拒绝进入 local_only 模式，
//     绝不能吞掉错误退化成"文件存在就直接信任内容"的裸信任模式。
func ListSignedAllowlistEntries(path, pubKeyB64 string) ([]AllowlistEntry, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "local_only allowlist: read file failed", err)
	}

	if pubKeyB64 == "" {
		return nil, apperr.New(apperr.CodeInternal,
			"local_only allowlist: "+path+" exists but POLARIS_LOCAL_ONLY_ALLOWLIST_PUBKEY is not set — refusing to load an unverifiable allowlist")
	}
	pubKeyRaw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pubKeyB64))
	if err != nil || len(pubKeyRaw) != ed25519.PublicKeySize {
		return nil, apperr.New(apperr.CodeInternal, "local_only allowlist: POLARIS_LOCAL_ONLY_ALLOWLIST_PUBKEY is not a valid base64 ed25519 public key")
	}

	sigRaw, err := os.ReadFile(path + ".sig")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "local_only allowlist: signature file "+path+".sig missing or unreadable (run `polaris allowlist sign`)", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigRaw)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, apperr.New(apperr.CodeInternal, "local_only allowlist: "+path+".sig is not a valid base64 ed25519 signature")
	}

	if !ed25519.Verify(ed25519.PublicKey(pubKeyRaw), raw, sig) {
		return nil, apperr.New(apperr.CodeInternal,
			"local_only allowlist: Ed25519 signature verification FAILED for "+path+" — refusing to load (file may be stale or tampered; re-run `polaris allowlist sign`)")
	}

	var f allowlistFile
	if err := toml.Unmarshal(raw, &f); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "local_only allowlist: TOML parse failed", err)
	}
	return f.Entry, nil
}
