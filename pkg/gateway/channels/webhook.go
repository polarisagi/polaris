package channels

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

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// LINE

func lineSendMessage(ctx context.Context, client *http.Client, accessToken, replyToken, text string) error {
	body, _ := json.Marshal(map[string]any{
		"replyToken": replyToken,
		"messages":   []map[string]string{{"type": "text", "text": text}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.line.me/v2/bot/message/reply", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("line replyMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}

func linePushMessage(ctx context.Context, client *http.Client, accessToken, to, text string) error {
	body, _ := json.Marshal(map[string]any{
		"to":       to,
		"messages": []map[string]string{{"type": "text", "text": text}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.line.me/v2/bot/message/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("line pushMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}

// LineVerifySignature 验证 LINE webhook HMAC-SHA256 签名（base64 encoded）。
func LineVerifySignature(channelSecret, body, signatureHeader string) bool {
	if channelSecret == "" {
		return true
	}
	mac := hmac.New(sha256.New, []byte(channelSecret))
	mac.Write([]byte(body))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}

// WhatsApp

func whatsappSendMessage(ctx context.Context, client *http.Client, phoneNumberID, accessToken, to, text string) error {
	url := fmt.Sprintf("https://graph.facebook.com/v18.0/%s/messages", phoneNumberID)
	body, _ := json.Marshal(map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text":              map[string]string{"body": text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("whatsapp sendMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}
