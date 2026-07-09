package channelsadmin

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// verifyWebhookSource 统一校验 Webhook 来源。如果验证失败或处理了特定的握手请求则返回 false。
func (h *ChannelsAdmin) verifyWebhookSource(w http.ResponseWriter, r *http.Request, channelType string, cfg map[string]any, body []byte) error {
	switch channelType {
	case "line":
		return h.verifyLineWebhook(w, r, cfg, body)
	case "whatsapp":
		return h.verifyWhatsAppWebhook(w, r, cfg, body)
	case "teams":
		return h.verifyTeamsWebhook(w, r, cfg, body)
	case "telegram":
		return h.verifyTelegramWebhook(w, r, cfg)
	case "slack":
		return h.verifySlackWebhook(w, r, cfg, body)
	case "discord":
		return h.verifyDiscordWebhook(w, r, cfg, body)
	default:
		if secret, _ := cfg["webhook_secret"].(string); secret != "" {
			// 通用 HMAC-SHA256 验证（X-Hub-Signature-256 header）
			return h.verifyGenericHMAC(w, r, secret, body)
		}
		return apperr.New(apperr.CodeUnauthorized, "generic webhook: missing webhook_secret") // fail-closed
	}
}

func (h *ChannelsAdmin) verifyLineWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) error {
	channelSecret, _ := cfg["channel_secret"].(string)
	sig := r.Header.Get("X-Line-Signature")
	if !cadapter.LineVerifySignature(channelSecret, string(body), sig) {
		return apperr.New(apperr.CodeUnauthorized, "line webhook: signature mismatch")
	}
	return nil
}

func (h *ChannelsAdmin) verifyWhatsAppWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) error {
	// GET：处理 hub challenge
	if r.Method == http.MethodGet {
		challenge := r.URL.Query().Get("hub.challenge")
		verifyToken, _ := cfg["verify_token"].(string)
		if verifyToken != "" && r.URL.Query().Get("hub.verify_token") != verifyToken {
			return apperr.New(apperr.CodeUnauthorized, "whatsapp webhook: verify_token mismatch")
		}
		w.Write([]byte(challenge)) //nolint:errcheck
		return apperr.New(apperr.CodeOK, "hub challenge handled")
	}

	// POST：验证 X-Hub-Signature-256
	appSecret, _ := cfg["app_secret"].(string)
	if appSecret == "" {
		return apperr.New(apperr.CodeUnauthorized, "whatsapp webhook: app_secret not configured")
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		return apperr.New(apperr.CodeUnauthorized, "whatsapp webhook: missing signature")
	}
	sig = strings.TrimPrefix(sig, "sha256=")
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return apperr.New(apperr.CodeUnauthorized, "whatsapp webhook: signature mismatch")
	}
	return nil
}

func (h *ChannelsAdmin) verifyTeamsWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) error { //nolint:gocyclo
	vt := r.URL.Query().Get("validationToken")
	if vt != "" {
		if len(vt) > 256 {
			return apperr.New(apperr.CodeInvalidInput, "invalid validationToken")
		}
		for _, c := range vt {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return apperr.New(apperr.CodeInvalidInput, "invalid validationToken")
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(vt)) //nolint:errcheck
		return apperr.New(apperr.CodeOK, "validationToken handled")
	}

	expectedState, _ := cfg["client_state"].(string)
	if expectedState == "" {
		return apperr.New(apperr.CodeUnauthorized, "teams webhook: client_state not configured")
	}
	var payload struct {
		Value []struct {
			ClientState string `json:"clientState"`
		} `json:"value"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Value) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "teams webhook: bad payload")
	}
	if payload.Value[0].ClientState != expectedState {
		return apperr.New(apperr.CodeUnauthorized, "teams webhook: clientState mismatch")
	}
	return nil
}

func (h *ChannelsAdmin) verifyTelegramWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any) error {
	// Telegram setWebhook 的 secret_token 是可选的：
	// 仅当用户在 cfg 中配置了 webhook_secret_token 时做比对，否则直接放行（向后兼容）。
	// bot_token 用于主动 API 调用，不参与 webhook 签名验证。
	secretToken, _ := cfg["webhook_secret_token"].(string)
	if secretToken == "" {
		return nil
	}
	incomingToken := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	// 用 hmac.Equal 做常量时间比对，防止时序攻击
	if !hmac.Equal([]byte(incomingToken), []byte(secretToken)) {
		return apperr.New(apperr.CodeUnauthorized, "telegram webhook: secret token mismatch")
	}
	return nil
}

func (h *ChannelsAdmin) verifySlackWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) error {
	secret, ok := cfg["signing_secret"].(string)
	if !ok || secret == "" {
		return apperr.New(apperr.CodeUnauthorized, "slack webhook: signing_secret not configured")
	}
	timestamp := r.Header.Get("X-Slack-Request-Timestamp")
	if timestamp == "" {
		return apperr.New(apperr.CodeUnauthorized, "slack webhook: missing timestamp")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)) > 5*time.Minute {
		return apperr.New(apperr.CodeUnauthorized, "slack webhook: invalid or expired timestamp")
	}
	sig := r.Header.Get("X-Slack-Signature")
	if !strings.HasPrefix(sig, "v0=") {
		return apperr.New(apperr.CodeUnauthorized, "slack webhook: invalid signature format")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:%s", timestamp, body)
	expectedSig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return apperr.New(apperr.CodeUnauthorized, "slack webhook: signature mismatch")
	}
	return nil
}

func (h *ChannelsAdmin) verifyDiscordWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) error {
	pubKeyStr, ok := cfg["public_key"].(string)
	if !ok || pubKeyStr == "" {
		return apperr.New(apperr.CodeUnauthorized, "discord webhook: public_key not configured")
	}
	pubKey, err := hex.DecodeString(pubKeyStr)
	if err != nil || len(pubKey) != ed25519.PublicKeySize {
		return apperr.New(apperr.CodeUnauthorized, "discord webhook: invalid public_key")
	}
	sigStr := r.Header.Get("X-Signature-Ed25519")
	ts := r.Header.Get("X-Signature-Timestamp")
	if sigStr == "" || ts == "" {
		return apperr.New(apperr.CodeUnauthorized, "discord webhook: missing signature or timestamp")
	}
	sig, err := hex.DecodeString(sigStr)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return apperr.New(apperr.CodeUnauthorized, "discord webhook: invalid signature format")
	}
	msg := []byte(ts)
	msg = append(msg, body...)
	if !ed25519.Verify(pubKey, msg, sig) {
		return apperr.New(apperr.CodeUnauthorized, "discord webhook: signature verification failed")
	}
	return nil
}

func (h *ChannelsAdmin) verifyGenericHMAC(w http.ResponseWriter, r *http.Request, secret string, body []byte) error {
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		return apperr.New(apperr.CodeUnauthorized, "generic webhook: missing X-Hub-Signature-256")
	}
	sig = strings.TrimPrefix(sig, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return apperr.New(apperr.CodeUnauthorized, "generic webhook: signature mismatch")
	}
	return nil
}
