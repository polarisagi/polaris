package network

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestListSignedAllowlistEntries_MissingFileReturnsNilNil(t *testing.T) {
	entries, err := ListSignedAllowlistEntries(filepath.Join(t.TempDir(), "does_not_exist.toml"), "irrelevant")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if entries != nil {
		t.Fatalf("expected nil entries for missing file, got %v", entries)
	}
}

func TestListSignedAllowlistEntries_FailClosedWithoutPubKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist.toml")
	if err := os.WriteFile(path, []byte(`[[entry]]
domain = "api.example.com"
port = 443
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ListSignedAllowlistEntries(path, ""); err == nil {
		t.Fatal("expected error when file exists but pubkey is not configured (fail-closed)")
	}
}

func TestListSignedAllowlistEntries_FailClosedOnTamperedSignature(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist.toml")
	content := []byte(`[[entry]]
domain = "api.example.com"
port = 443
protocol = "https"
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, content)
	if err := os.WriteFile(path+".sig", []byte(base64.StdEncoding.EncodeToString(sig)), 0o644); err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	// 篡改文件内容但不重新签名 → 验签必须失败
	if err := os.WriteFile(path, append(content, []byte("\n# tampered\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ListSignedAllowlistEntries(path, pubB64); err == nil {
		t.Fatal("expected signature verification failure after tampering")
	}
}

func TestListSignedAllowlistEntries_ValidSignatureParsesEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist.toml")
	content := []byte(`[[entry]]
domain = "api.example.com"
port = 443
protocol = "https"

[[entry]]
domain = "notion.so"
port = 443
protocol = "https"
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, content)
	if err := os.WriteFile(path+".sig", []byte(base64.StdEncoding.EncodeToString(sig)), 0o644); err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	entries, err := ListSignedAllowlistEntries(path, pubB64)
	if err != nil {
		t.Fatalf("expected successful load, got %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Domain != "api.example.com" || entries[0].Port != 443 {
		t.Fatalf("unexpected entry[0]: %+v", entries[0])
	}
	if entries[1].Domain != "notion.so" {
		t.Fatalf("unexpected entry[1]: %+v", entries[1])
	}
}

func TestNetworkSandbox_InitAllowlistFromFile_PopulatesAllowlist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist.toml")
	content := []byte(`[[entry]]
domain = "api.example.com"
port = 443
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, content)
	if err := os.WriteFile(path+".sig", []byte(base64.StdEncoding.EncodeToString(sig)), 0o644); err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	ns := NewNetworkSandbox(5)
	if err := ns.InitAllowlistFromFile(path, pubB64); err != nil {
		t.Fatalf("InitAllowlistFromFile failed: %v", err)
	}
	if !ns.allowlist.IsAllowed("api.example.com", 443) {
		t.Fatal("expected api.example.com:443 to be allowed after loading signed allowlist")
	}
	if ns.allowlist.IsAllowed("evil.example.com", 443) {
		t.Fatal("unexpected host allowed")
	}
}
