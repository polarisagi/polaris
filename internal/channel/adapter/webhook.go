package adapter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// LINE

func LineSendMessage(ctx context.Context, client *http.Client, accessToken, replyToken, text string) error {
	body, _ := json.Marshal(map[string]any{
		"replyToken": replyToken,
		"messages":   []map[string]string{{"type": "text", "text": text}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.line.me/v2/bot/message/reply", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("LineSendMessage: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("LineSendMessage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("line replyMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}

func LinePushMessage(ctx context.Context, client *http.Client, accessToken, to, text string) error {
	body, _ := json.Marshal(map[string]any{
		"to":       to,
		"messages": []map[string]string{{"type": "text", "text": text}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.line.me/v2/bot/message/push", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("LinePushMessage: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("LinePushMessage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("line pushMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}

// LineVerifySignature 验证 LINE webhook HMAC-SHA256 签名（base64 encoded）。
func LineVerifySignature(channelSecret, body, signatureHeader string) bool {
	if channelSecret == "" {
		// 未配置 channel_secret：无法验证，拒绝（fail-closed）
		return false
	}
	mac := hmac.New(sha256.New, []byte(channelSecret))
	mac.Write([]byte(body))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}

// WhatsApp

func WhatsappSendMessage(ctx context.Context, client *http.Client, phoneNumberID, accessToken, to, text string) error {
	url := fmt.Sprintf("https://graph.facebook.com/v18.0/%s/messages", phoneNumberID)
	body, _ := json.Marshal(map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text":              map[string]string{"body": text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("WhatsappSendMessage: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("WhatsappSendMessage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("whatsapp SendMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}
