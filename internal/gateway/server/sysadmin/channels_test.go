package sysadmin

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerifyLineWebhook(t *testing.T) {
	h := &SysAdminHandler{}
	cfg := map[string]any{"channel_secret": "my-secret"}
	body := []byte(`{"events":[]}`)

	// Correct signature (LINE uses base64)
	mac := hmac.New(sha256.New, []byte("my-secret"))
	mac.Write(body)
	validSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Line-Signature", validSig)
	w := httptest.NewRecorder()

	if !h.verifyLineWebhook(w, req, cfg, body) {
		t.Errorf("Expected true for valid signature")
	}

	// Invalid signature
	req.Header.Set("X-Line-Signature", "invalid")
	w = httptest.NewRecorder()
	if h.verifyLineWebhook(w, req, cfg, body) {
		t.Errorf("Expected false for invalid signature")
	}

	// No secret configured (fail-closed)
	cfgEmpty := map[string]any{}
	if h.verifyLineWebhook(w, req, cfgEmpty, body) {
		t.Errorf("Expected false if no secret configured")
	}
}

func TestVerifyWhatsAppWebhook(t *testing.T) {
	h := &SysAdminHandler{}
	cfg := map[string]any{"verify_token": "my-token"}

	// Valid GET verification
	req := httptest.NewRequest(http.MethodGet, "/?hub.mode=subscribe&hub.challenge=1158201444&hub.verify_token=my-token", nil)
	w := httptest.NewRecorder()
	// verifyWhatsAppWebhook returns false for handled challenge
	if h.verifyWhatsAppWebhook(w, req, cfg, nil) {
		t.Errorf("Expected false because challenge is handled")
	}
	if w.Body.String() != "1158201444" {
		t.Errorf("Expected challenge to be echoed, got %s", w.Body.String())
	}

	// Invalid token
	req = httptest.NewRequest(http.MethodGet, "/?hub.mode=subscribe&hub.challenge=1158201444&hub.verify_token=invalid", nil)
	w = httptest.NewRecorder()
	if h.verifyWhatsAppWebhook(w, req, cfg, nil) {
		t.Errorf("Expected false for invalid token")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("Expected 403 Forbidden")
	}

	// POST request (message delivery) without app_secret (fail-closed)
	req = httptest.NewRequest(http.MethodPost, "/", nil)
	w = httptest.NewRecorder()
	if h.verifyWhatsAppWebhook(w, req, cfg, nil) {
		t.Errorf("Expected false for missing app_secret")
	}
}

func TestVerifyTeamsWebhook(t *testing.T) {
	h := &SysAdminHandler{}

	// Validation request
	req := httptest.NewRequest(http.MethodPost, "/?validationToken=validToken123", nil)
	w := httptest.NewRecorder()
	if h.verifyTeamsWebhook(w, req, nil, nil) {
		t.Errorf("Expected false for validation token request")
	}
	if w.Body.String() != "validToken123" {
		t.Errorf("Expected token to be echoed")
	}

	// Normal request
	req = httptest.NewRequest(http.MethodPost, "/", nil)
	w = httptest.NewRecorder()
	if !h.verifyTeamsWebhook(w, req, nil, nil) {
		t.Errorf("Expected true for normal request without validationToken")
	}

	// Invalid characters in token
	req = httptest.NewRequest(http.MethodPost, "/?validationToken=invalid@token!", nil)
	w = httptest.NewRecorder()
	if h.verifyTeamsWebhook(w, req, nil, nil) {
		t.Errorf("Expected false for invalid token chars")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 Bad Request")
	}
}

func TestVerifyTelegramWebhook(t *testing.T) {
	h := &SysAdminHandler{}
	botToken := "my-bot-token"
	cfg := map[string]any{"bot_token": botToken}

	mac := hmac.New(sha256.New, []byte("WebAppData"))
	mac.Write([]byte(botToken))
	validToken := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", validToken)
	w := httptest.NewRecorder()

	if !h.verifyTelegramWebhook(w, req, cfg) {
		t.Errorf("Expected true for valid token")
	}

	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "invalid")
	w = httptest.NewRecorder()
	if h.verifyTelegramWebhook(w, req, cfg) {
		t.Errorf("Expected false for invalid token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 Unauthorized")
	}

	// Test missing token in request (fail-closed)
	req.Header.Del("X-Telegram-Bot-Api-Secret-Token")
	w = httptest.NewRecorder()
	if h.verifyTelegramWebhook(w, req, cfg) {
		t.Errorf("Expected false when header missing")
	}

	// Test missing config
	if !h.verifyTelegramWebhook(w, req, map[string]any{}) {
		t.Errorf("Expected true when config is missing")
	}
}

func TestVerifySlackWebhook(t *testing.T) {
	h := &SysAdminHandler{}
	secret := "my-slack-secret"
	cfg := map[string]any{"signing_secret": secret}
	body := []byte(`{"text":"hello"}`)
	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	sigBase := fmt.Sprintf("v0:%s:%s", timestamp, body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(sigBase))
	validSig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", validSig)
	w := httptest.NewRecorder()

	if !h.verifySlackWebhook(w, req, cfg, body) {
		t.Errorf("Expected true for valid signature")
	}

	// Invalid signature
	req.Header.Set("X-Slack-Signature", "v0=invalid")
	w = httptest.NewRecorder()
	if h.verifySlackWebhook(w, req, cfg, body) {
		t.Errorf("Expected false for invalid signature")
	}

	// Missing timestamp
	req.Header.Del("X-Slack-Request-Timestamp")
	w = httptest.NewRecorder()
	if h.verifySlackWebhook(w, req, cfg, body) {
		t.Errorf("Expected false for missing timestamp")
	}
}

func TestVerifyDiscordWebhook(t *testing.T) {
	h := &SysAdminHandler{}

	pubKey, privKey, _ := ed25519.GenerateKey(nil)
	cfg := map[string]any{"public_key": hex.EncodeToString(pubKey)}

	body := []byte(`{"type":1}`)
	ts := "1234567890"

	msg := []byte(ts)
	msg = append(msg, body...)
	sig := ed25519.Sign(privKey, msg)
	sigStr := hex.EncodeToString(sig)

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Signature-Ed25519", sigStr)
	req.Header.Set("X-Signature-Timestamp", ts)
	w := httptest.NewRecorder()

	if !h.verifyDiscordWebhook(w, req, cfg, body) {
		t.Errorf("Expected true for valid signature")
	}

	// Invalid signature
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(make([]byte, ed25519.SignatureSize)))
	w = httptest.NewRecorder()
	if h.verifyDiscordWebhook(w, req, cfg, body) {
		t.Errorf("Expected false for invalid signature")
	}
}

func TestVerifyGenericHMAC(t *testing.T) {
	h := &SysAdminHandler{}
	secret := "generic-secret"
	body := []byte("payload")

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+validSig)
	w := httptest.NewRecorder()

	if !h.verifyGenericHMAC(w, req, secret, body) {
		t.Errorf("Expected true for valid generic HMAC")
	}

	req.Header.Set("X-Hub-Signature-256", "invalid")
	w = httptest.NewRecorder()
	if h.verifyGenericHMAC(w, req, secret, body) {
		t.Errorf("Expected false for invalid HMAC")
	}
}

func TestVerifyWebhookSource(t *testing.T) {
	h := &SysAdminHandler{}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()

	// Test Line
	cfg := map[string]any{"channel_secret": "my-secret"}
	// It should fail line verification since we have no headers set
	if h.verifyWebhookSource(w, req, "line", cfg, nil) {
		t.Errorf("Expected false for line without headers")
	}

	// Test WhatsApp GET
	reqGet := httptest.NewRequest(http.MethodGet, "/?hub.verify_token=invalid", nil)
	wGet := httptest.NewRecorder()
	if h.verifyWebhookSource(wGet, reqGet, "whatsapp", map[string]any{"verify_token": "secret"}, nil) {
		t.Errorf("Expected false for whatsapp with invalid token")
	}

	// Test default generic
	if h.verifyWebhookSource(w, req, "unknown", map[string]any{"webhook_secret": "sec"}, nil) {
		t.Errorf("Expected false for generic with missing header")
	}

	// Test generic without secret
	if !h.verifyWebhookSource(w, req, "unknown", map[string]any{}, nil) {
		t.Errorf("Expected true for generic without secret configured")
	}
}
