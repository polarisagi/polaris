package policy

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

// ── DataSplitter ──────────────────────────────────────────────────────────────

func TestDataSplitter_Partition(t *testing.T) {
	s := DataSplitter{}
	cases := []struct {
		source        string
		allowTraining bool
		want          string
	}{
		{SourceSynthetic, false, PartitionTraining},
		{SourceSynthetic, true, PartitionTraining}, // allowTraining 对 synthetic 无效
		{SourceManual, false, PartitionHoldout},
		{SourceManual, true, PartitionTraining},
		{SourceIncident, false, PartitionHoldout},
		{SourceIncident, true, PartitionHoldout}, // allowTraining 对 incident 无效
		{SourceShadow, false, PartitionHoldout},
		{"unknown", false, PartitionHoldout}, // fail-closed
		{"", false, PartitionHoldout},
	}
	for _, c := range cases {
		got := s.Partition(c.source, c.allowTraining)
		if got != c.want {
			t.Errorf("Partition(%q, %v) = %q, want %q", c.source, c.allowTraining, got, c.want)
		}
	}
}

// ── CheckAccess ───────────────────────────────────────────────────────────────

func TestCheckAccess(t *testing.T) {
	allowed := []struct{ role, partition string }{
		{RoleM9Optimizer, PartitionTraining},
		{RoleM9Optimizer, PartitionValidation},
		{RoleCIGate, PartitionHoldout},
	}
	for _, c := range allowed {
		if err := CheckAccess(c.role, c.partition); err != nil {
			t.Errorf("CheckAccess(%q,%q) unexpected error: %v", c.role, c.partition, err)
		}
	}

	denied := []struct{ role, partition string }{
		{RoleM9Optimizer, PartitionHoldout},
		{RoleCIGate, PartitionTraining},
		{RoleCIGate, PartitionValidation},
		{"unknown_role", PartitionTraining},
		{"", PartitionHoldout},
	}
	for _, c := range denied {
		if err := CheckAccess(c.role, c.partition); err == nil {
			t.Errorf("CheckAccess(%q,%q) should have returned an error", c.role, c.partition)
		}
	}
}

// ── Engine ────────────────────────────────────────────────────────────────────

func mustGenerateKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

func signRequest(priv ed25519.PrivateKey, role, partition string, ts int64) []byte {
	msg := []byte(role + ":" + partition + ":" + itoa(ts))
	return ed25519.Sign(priv, msg)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

func TestEngine_VerifyRequest_Valid(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	e := NewEngine(map[string]ed25519.PublicKey{RoleM9Optimizer: pub})

	ts := time.Now().Unix()
	sig := signRequest(priv, RoleM9Optimizer, PartitionTraining, ts)
	if err := e.VerifyRequest(RoleM9Optimizer, PartitionTraining, sig, ts); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestEngine_VerifyRequest_WrongPartition(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	e := NewEngine(map[string]ed25519.PublicKey{RoleM9Optimizer: pub})

	ts := time.Now().Unix()
	// 签名是针对 training 的，但请求访问 holdout
	sig := signRequest(priv, RoleM9Optimizer, PartitionTraining, ts)
	if err := e.VerifyRequest(RoleM9Optimizer, PartitionHoldout, sig, ts); err == nil {
		t.Fatal("expected error for role access violation")
	}
}

func TestEngine_VerifyRequest_BadSignature(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	_, otherPriv := mustGenerateKey(t) // 不同私钥签名
	e := NewEngine(map[string]ed25519.PublicKey{RoleM9Optimizer: pub})

	ts := time.Now().Unix()
	sig := signRequest(otherPriv, RoleM9Optimizer, PartitionTraining, ts)
	if err := e.VerifyRequest(RoleM9Optimizer, PartitionTraining, sig, ts); err == nil {
		t.Fatal("expected error for invalid signature")
	}
}

func TestEngine_VerifyRequest_ReplayAttack(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	e := NewEngine(map[string]ed25519.PublicKey{RoleCIGate: pub})

	// 超过时间窗口的旧时间戳
	oldTs := time.Now().Unix() - sigReplayWindowSec - 1
	sig := signRequest(priv, RoleCIGate, PartitionHoldout, oldTs)
	if err := e.VerifyRequest(RoleCIGate, PartitionHoldout, sig, oldTs); err == nil {
		t.Fatal("expected error for expired timestamp")
	}
}

func TestEngine_VerifyRequest_NoKeyRegistered(t *testing.T) {
	e := NewEngine(nil) // 无注册密钥
	ts := time.Now().Unix()
	if err := e.VerifyRequest(RoleM9Optimizer, PartitionTraining, []byte("sig"), ts); err == nil {
		t.Fatal("expected error when no public key is registered (fail-closed)")
	}
}

func TestEngine_VerifyRequestDev_NoKey_SkipsSig(t *testing.T) {
	e := NewEngine(nil) // 无注册密钥 → dev 模式跳过签名
	ts := time.Now().Unix()
	// 即使签名是空的，dev 模式下角色合法时应放行
	if err := e.VerifyRequestDev(RoleM9Optimizer, PartitionTraining, nil, ts); err != nil {
		t.Fatalf("VerifyRequestDev should skip sig when no key: %v", err)
	}
}

func TestEngine_VerifyRequestDev_NoKey_StillEnforcesRole(t *testing.T) {
	e := NewEngine(nil)
	ts := time.Now().Unix()
	// dev 模式下，角色访问违规仍应拒绝
	if err := e.VerifyRequestDev(RoleM9Optimizer, PartitionHoldout, nil, ts); err == nil {
		t.Fatal("VerifyRequestDev should still enforce role access policy")
	}
}
