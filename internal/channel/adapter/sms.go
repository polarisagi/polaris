package adapter

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SMS 通过 Twilio REST API 发送短信，通过 webhook 接收。
//
// 配置项：
//
//	account_sid     string  Twilio Account SID
//	auth_token      string  Twilio Auth Token
//	from_number     string  Twilio E.164 号码，如 "+15551234567"
//	allowed_numbers string  逗号分隔的白名单电话号码；空=所有人

func TwilioSendSMS(ctx context.Context, client *http.Client, accountSID, authToken, from, to, body string) error {
	apiURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", accountSID)
	formData := url.Values{
		"From": {from},
		"To":   {to},
		"Body": {body},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL,
		strings.NewReader(formData.Encode()))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "TwilioSendSMS", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
		[]byte(accountSID+":"+authToken)))
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "TwilioSendSMS", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("twilio: send status %d: %s", resp.StatusCode, b))
	}
	return nil
}

func init() { Register(&SmsAdapter{}) }

type SmsAdapter struct{}

func (a *SmsAdapter) Type() string { return "sms" }

func (a *SmsAdapter) Extract(body []byte, r *http.Request) protocol.ChannelMessage {
	if r == nil {
		return protocol.ChannelMessage{}
	}
	if err := r.ParseForm(); err != nil {
		return protocol.ChannelMessage{}
	}
	text := r.FormValue("Body")
	from := r.FormValue("From")
	if text == "" || from == "" {
		return protocol.ChannelMessage{}
	}
	// Note: We need to import "github.com/polarisagi/polaris/internal/protocol" and "github.com/polarisagi/polaris/pkg/types"
	return protocol.ChannelMessage{Text: text, ChatID: from, UserID: from, TaintLevel: types.TaintHigh}
}

func (a *SmsAdapter) Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	accountSID, _ := cfg["account_sid"].(string)
	authToken, _ := cfg["auth_token"].(string)
	fromNumber, _ := cfg["from_number"].(string)

	if accountSID == "" || authToken == "" || fromNumber == "" {
		slog.Warn("sms: twilio config missing", "err", apperr.New(apperr.CodeInternal, "log event"))
		return nil
	}
	if err := TwilioSendSMS(ctx, host.HTTPClient(), accountSID, authToken, fromNumber, msg.ChatID, text); err != nil {
		slog.Error("channels: send reply failed", "type", "sms", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "sms: send message", err)
	}
	return nil
}

func (a *SmsAdapter) StartPoller(host Host, channelID string, cfg map[string]any) bool {
	return false // SMS is webhook only
}
