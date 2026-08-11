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

func TestAnchorChecksumTrust_SigningNotProvisioned(t *testing.T) {
	f := newTrustFixture(t)
	// 信任根为空 = 签名未开通：必须放行（否则本特性一落地就卡死所有存量部署的
	// 更新），且不去取 .sig（服务端根本没提供）。
	m := f.newManager(false, nil)
	if err := m.verifyChecksum(context.Background(), testVersion, testArchive, f.archivePath); err != nil {
		t.Fatalf("签名未开通时应退回纯 checksum 校验并放行，实际: %v", err)
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
