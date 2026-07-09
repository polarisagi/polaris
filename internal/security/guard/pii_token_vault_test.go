package guard

import (
	"strings"
	"testing"
)

func TestPIITokenVault(t *testing.T) {
	v := NewPIITokenVault()
	orig := "secret@example.com"

	token := v.Tokenize(orig)
	if !strings.HasPrefix(token, "⟦PII:") || !strings.HasSuffix(token, "⟧") {
		t.Errorf("invalid token format: %s", token)
	}

	resolved, err := v.Resolve(token)
	if err != nil || resolved != orig {
		t.Errorf("expected %s, nil error; got %s, %v", orig, resolved, err)
	}

	// fail-closed: 未知/伪造 token 必须返回 error，不能静默透传。
	_, err = v.Resolve("⟦PII:deadbeef⟧")
	if err == nil {
		t.Error("expected error for unknown token, got nil")
	}

	v.Clear()
	_, err = v.Resolve(token)
	if err == nil {
		t.Error("expected error after Clear(), got nil")
	}
}

func TestPIITokenVault_Restore(t *testing.T) {
	v := NewPIITokenVault()
	tok1 := v.Tokenize("alice@example.com")
	tok2 := v.Tokenize("13800001111")

	text := "请发送邮件给 " + tok1 + "，并短信通知 " + tok2
	restored, err := v.Restore(text)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if restored != "请发送邮件给 alice@example.com，并短信通知 13800001111" {
		t.Errorf("unexpected restore result: %s", restored)
	}

	// fail-closed: 文本中混入一个未知 token，整体拒绝，不做部分还原。
	forged := text + " ⟦PII:deadbeef⟧"
	if _, err := v.Restore(forged); err == nil {
		t.Error("expected fail-closed error for text containing unknown token, got nil")
	}

	if !v.HasTokens(text) {
		t.Error("expected HasTokens to detect embedded tokens")
	}
	if v.HasTokens("no tokens here") {
		t.Error("expected HasTokens to return false for plain text")
	}
}
