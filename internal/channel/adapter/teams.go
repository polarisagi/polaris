package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Teams 通过 Microsoft Graph API 接入 Teams 聊天。
// 接收：MS Graph Change Notifications (webhook)
// 发送：POST /chats/{chatId}/messages
//
// 配置项：
//
//	tenant_id      string  Azure AD 租户 ID
//	client_id      string  应用注册的 Client ID
//	client_secret  string  应用注册的 Client Secret
//	client_state   string  webhook 订阅时的 clientState 校验值（可选）

const (
	teamsGraphBase = "https://graph.microsoft.com/v1.0"
	teamsTokenBase = "https://login.microsoftonline.com"
)

// TeamsGetAccessToken 通过 client_credentials 获取 Graph API token。
func TeamsGetAccessToken(ctx context.Context, client *http.Client, tenantID, clientID, clientSecret string) (string, error) {
	tokenURL := fmt.Sprintf("%s/%s/oauth2/v2.0/token", teamsTokenBase, tenantID)
	body := fmt.Sprintf(
		"grant_type=client_credentials&client_id=%s&client_secret=%s&scope=https%%3A%%2F%%2Fgraph.microsoft.com%%2F.default",
		clientID, clientSecret,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		bytes.NewReader([]byte(body)))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "TeamsGetAccessToken", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "TeamsGetAccessToken", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode != http.StatusOK {
		return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("teams: token status %d: %s", resp.StatusCode, b))
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(b, &result) != nil || result.AccessToken == "" {
		return "", apperr.New(apperr.CodeInternal, "teams: empty access_token")
	}
	return result.AccessToken, nil
}

// TeamsSendMessage 向指定 Teams chat 发送消息。
func TeamsSendMessage(ctx context.Context, client *http.Client, accessToken, chatID, text string) error {
	url := fmt.Sprintf("%s/chats/%s/messages", teamsGraphBase, chatID)
	body, _ := json.Marshal(map[string]any{
		"body": map[string]string{"contentType": "text", "content": text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "TeamsSendMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "TeamsSendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("teams: SendMessage status %d: %s", resp.StatusCode, b))
	}
	return nil
}
