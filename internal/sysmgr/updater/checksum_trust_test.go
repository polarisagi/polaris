package updater

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件覆盖 anchorChecksumTrust 的三条状态（ADR-0095 决策二）：
// 未开通降级 / 已开通验签通过 / 已开通但签名缺失或非法 → 拒绝安装。
//
// 第三条是重点：签名一旦开通，网络侧就不能再把客户端降级回纯 checksum 模式
// （signature stripping）。这个性质没有测试守着，很容易在后续重构中被一句
// "取不到 .sig 就跳过" 悄悄弱化。

const (
	testVersion = "v9.9.9"
	testArchive = "polaris-test.tar.gz"
)

// trustFixture 构造一次验签所需的全部素材。
type trustFixture struct {
	t           *testing.T
	priv        *ecdsa.PrivateKey
	archivePath string
	checksum    string
	checksumDoc string
}

func newTrustFixture(t *testing.T) *trustFixture {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), testArchive)
	content := []byte("fake archive content")
	if err := os.WriteFile(archivePath, content, 0o600); err != nil {
		t.Fatalf("写归档失败: %v", err)
	}
	sum := sha256.Sum256(content)
	checksum := hex.EncodeToString(sum[:])
	return &trustFixture{
		t:           t,
		priv:        priv,
		archivePath: archivePath,
		checksum:    checksum,
		checksumDoc: fmt.Sprintf("%s  %s\n", checksum, testArchive),
	}
}

// newManager 构造一个带 mock HTTP 的 Manager。sigBody 为 nil 表示服务端不提供
// .sig（模拟签名缺失 / 被剥离）；provisioned 决定信任根是否非空。
func (f *trustFixture) newManager(provisioned bool, sigBody []byte) *Manager {
	f.t.Helper()
	base := fmt.Sprintf("/polarisagi/polaris/releases/download/%s/%s", testVersion, testArchive)

	mux := http.NewServeMux()
	mux.HandleFunc(base+".sha256", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, f.checksumDoc)
	})
	if sigBody != nil {
		mux.HandleFunc(base+".sha256.sig", func(w http.ResponseWriter, _ *http.Request) {
			w.Write(sigBody) //nolint:errcheck
		})
	}

	m := New("v1.0.0", "abc", "2024", &http.Client{Transport: &mockTransport{handler: mux}})
	if provisioned {
		m.releaseKeys = []releaseKey{{name: "release-test.pem", pub: &f.priv.PublicKey}}
	} else {
		m.releaseKeys = nil
	}
	return m
}

// TestAnchorChecksumTrust_AnchorB 签名未开通 + 校验值取自 GitHub 直连：
// 锚点 B（TLS）成立，放行。这是绝大多数用户今天走的路径，必须保持可用。
func TestAnchorChecksumTrust_AnchorB(t *testing.T) {
	f := newTrustFixture(t)
	m := f.newManager(false, nil)
	if err := m.verifyChecksum(context.Background(), testVersion, testArchive, f.archivePath); err != nil {
		t.Fatalf("签名未开通但校验值取自 GitHub 直连时应放行（锚点 B），实际: %v", err)
	}
}

// TestAnchorChecksumTrust_NoAnchorIsRejected 两个锚点都不成立时必须拒装。
//
// 这是 2026-08-11 收紧的核心：签名未开通 **且** 校验值只能从镜像取得时，归档与
// 校验值可能出自同一个被污染的镜像，SHA-256 比对必然通过——"校验"退化成"自洽性
// 检查"。此前是 Warn + 放行，等于把一个无法证明来源的二进制装进用户机器，而
// polaris 装完会自我替换、重启、且持有 LLM 凭据与工具执行能力。
func TestAnchorChecksumTrust_NoAnchorIsRejected(t *testing.T) {
	f := newTrustFixture(t)
	// 直接调 anchorChecksumTrust 并置 fromUpstream=false，而不是去 mock 一个
	// "GitHub 不通、镜像通"的网络：候选节点顺序由 downloader 的代理探测决定，
	// 在测试里复现那个环境既脆弱又与本函数要验的语义无关。fromUpstream 本身
	// 就是"校验值是否取自直连"的完整表达。
	m := f.newManager(false, nil)
	err := m.anchorChecksumTrust(context.Background(), testVersion, testArchive, []byte(f.checksumDoc), false)
	if err == nil {
		t.Fatal("签名未开通且校验值只能取自镜像时必须拒绝安装——此时无任何可用信任锚点")
	}
	// 拒绝时必须告诉用户怎么办，否则只是把故障甩给用户。
	for _, want := range []string{"镜像", "releasekeys/README.md", "手动下载"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("拒装错误信息缺少处置指引 %q，实际: %v", want, err)
		}
	}
}

// TestAnchorChecksumTrust_SignatureRescuesMirrorPath 签名开通后，镜像路径恢复可用。
//
// 这正是开通签名的产品价值：锚点 A 不依赖能否直连 GitHub，因此 GitHub 完全不可达
// 的用户重新获得自动更新能力，**且比开通前更安全**（镜像伪造不出签名）。
func TestAnchorChecksumTrust_SignatureRescuesMirrorPath(t *testing.T) {
	f := newTrustFixture(t)
	sig := signLikeCosign(t, f.priv, []byte(f.checksumDoc))
	m := f.newManager(true, []byte(sig))
	// fromUpstream=false：校验值取自镜像。有锚点 A 时这不再是问题。
	if err := m.anchorChecksumTrust(context.Background(), testVersion, testArchive, []byte(f.checksumDoc), false); err != nil {
		t.Fatalf("签名开通后镜像路径应恢复可用（锚点 A 与传输路径无关），实际: %v", err)
	}
}

func TestAnchorChecksumTrust_SignatureValid(t *testing.T) {
	f := newTrustFixture(t)
	sig := signLikeCosign(t, f.priv, []byte(f.checksumDoc))
	m := f.newManager(true, []byte(sig))
	if err := m.verifyChecksum(context.Background(), testVersion, testArchive, f.archivePath); err != nil {
		t.Fatalf("合法签名应通过: %v", err)
	}
}

func TestAnchorChecksumTrust_SignatureStrippedIsRejected(t *testing.T) {
	f := newTrustFixture(t)
	// 签名已开通但服务端不提供 .sig —— 中间人剥离签名，试图把客户端降级回
	// 纯 checksum 模式。必须拒绝，否则"开通签名"可被网络侧单方面撤销。
	m := f.newManager(true, nil)
	err := m.verifyChecksum(context.Background(), testVersion, testArchive, f.archivePath)
	if err == nil {
		t.Fatal("签名已开通但取不到 .sig 时必须拒绝安装（signature stripping 防护）")
	}
	if !strings.Contains(err.Error(), "sha256.sig") {
		t.Errorf("错误信息应指明缺失的是签名文件，实际: %v", err)
	}
}

func TestAnchorChecksumTrust_ForgedSignatureIsRejected(t *testing.T) {
	f := newTrustFixture(t)
	attacker, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	// 攻击者用自己的密钥对篡改后的校验值签名——签名格式完全合法，只是签发者
	// 不在信任根内。这是镜像投毒的标准形态。
	sig := signLikeCosign(t, attacker, []byte(f.checksumDoc))
	m := f.newManager(true, []byte(sig))
	if err := m.verifyChecksum(context.Background(), testVersion, testArchive, f.archivePath); err == nil {
		t.Fatal("非信任根签发的签名必须被拒绝")
	}
}

func TestAnchorChecksumTrust_TamperedChecksumIsRejected(t *testing.T) {
	f := newTrustFixture(t)
	// 签名是对**原始**校验值文档签的，但服务端返回了被篡改的校验值文档。
	sig := signLikeCosign(t, f.priv, []byte(f.checksumDoc))
	f.checksumDoc = strings.Repeat("0", 64) + "  " + testArchive + "\n"
	m := f.newManager(true, []byte(sig))
	if err := m.verifyChecksum(context.Background(), testVersion, testArchive, f.archivePath); err == nil {
		t.Fatal("校验值被篡改后签名不匹配，必须拒绝")
	}
}
