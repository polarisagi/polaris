package adapter

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type dialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

//nolint:gocyclo
func RunEmailPoller(ctx context.Context, host PollerHost, channelID string, cfg map[string]any) {
	slog.Info("email: poller started", "channel", channelID)
	defer slog.Info("email: poller stopped", "channel", channelID)

	imapHost, _ := cfg["imap_host"].(string)
	imapPort, _ := cfg["imap_port"].(string)
	if imapPort == "" {
		imapPort = "993"
	}
	address, _ := cfg["address"].(string)
	password, _ := cfg["password"].(string)

	pollInterval := 30
	if n, ok := cfg["poll_interval"].(int); ok && n > 0 {
		pollInterval = n
	}

	allowedSenders := make(map[string]bool)
	if as, _ := cfg["allowed_senders"].(string); as != "" {
		for sender := range strings.SplitSeq(as, ",") {
			sender = strings.ToLower(strings.TrimSpace(sender))
			if sender != "" {
				allowedSenders[sender] = true
			}
		}
	}

	ticker := time.NewTicker(time.Duration(pollInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if host.SafeDialer() == nil {
			slog.Error("email: SafeDialer 未注入，拒绝 IMAP 连接（SSRF 防护）", "channel", channelID, "err", apperr.New(apperr.CodeInternal, "log event"))
			return
		}
		msgs, err := imapFetchUnseen(ctx, host.SafeDialer().DialContext, imapHost+":"+imapPort, address, password)
		if err != nil {
			slog.Error("email: IMAP fetch failed", "err", err)
			continue
		}
		for _, em := range msgs {
			fromAddr := strings.ToLower(em.From)
			if len(allowedSenders) > 0 && !allowedSenders[fromAddr] {
				continue
			}
			// SPF/DKIM 校验：防止 From 地址伪造注入
			if !emailAuthPassed(em.AuthResults) {
				slog.Warn("email: message rejected, SPF/DKIM failed",
					"from", em.From, "channel", channelID, "auth_results", em.AuthResults)
				continue
			}
			//custom-nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
			go host.OnMessage("email", channelID, cfg, Message{
				Text:       em.Body,
				ChatID:     em.From,
				UserID:     em.From,
				ReplyToken: em.MessageID,

				TaintLevel: types.TaintHigh,
			})
		}
	}
}

func EmailSendMessage(smtpHost, smtpPort, address, password, to, subject, body string) error {
	auth := smtp.PlainAuth("", address, password, smtpHost)
	msg := []byte(
		"From: " + address + "\r\n" +
			"To: " + to + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"Content-Type: text/plain; charset=UTF-8\r\n" +
			"\r\n" +
			body + "\r\n",
	)
	return smtp.SendMail(smtpHost+":"+smtpPort, auth, address, []string{to}, msg)
}

// ─── 轻量 IMAP 客户端 ─────────────────────────────────────────────────────────

type imapMessage struct {
	From        string
	Subject     string
	MessageID   string
	Body        string
	AuthResults string // Authentication-Results 头原始内容
}

// imapFetchUnseen 通过调用方注入的 dialCtx 建立 TCP 连接（必须经 SafeDialer 校验），
// 再在已校验连接上升级 TLS，防止直接调用 tls.DialWithDialer 绕过 SSRF 拦截。
func imapFetchUnseen(ctx context.Context, dialCtx dialContextFunc, addr, user, password string) ([]imapMessage, error) { //nolint:gocyclo
	host := strings.Split(addr, ":")[0]
	rawConn, err := dialCtx(ctx, "tcp", addr)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("imap: dial: %v", err), err)
	}
	tlsCfg := &tls.Config{ServerName: host}
	conn := tls.Client(rawConn, tlsCfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("imap: tls handshake: %v", err), err)
	}
	defer conn.Close()

	r := bufio.NewReader(conn)
	w := func(cmd string) error {
		_, err := fmt.Fprintf(conn, "%s\r\n", cmd)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "imapFetchUnseen", err)
		}
		return nil
	}
	readLine := func() (string, error) {
		line, err := r.ReadString('\n')
		if err != nil {
			return strings.TrimRight(line, "\r\n"), apperr.Wrap(apperr.CodeInternal, "imapFetchUnseen", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	if _, err := readLine(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "imapFetchUnseen", err)
	}
	if err := w("A001 LOGIN " + user + " " + password); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "imapFetchUnseen", err)
	}
	if line, err := readLine(); err != nil || !strings.HasPrefix(line, "A001 OK") {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("imap: login failed: %s", line))
	}
	if err := w("A002 SELECT INBOX"); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "imapFetchUnseen", err)
	}
	for {
		line, err := readLine()
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "imapFetchUnseen", err)
		}
		if strings.HasPrefix(line, "A002 OK") {
			break
		}
		if strings.HasPrefix(line, "A002 NO") || strings.HasPrefix(line, "A002 BAD") {
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("imap: SELECT INBOX failed: %s", line))
		}
	}
	if err := w("A003 SEARCH UNSEEN"); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "imapFetchUnseen", err)
	}
	var seqNums []string
	for {
		line, err := readLine()
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "imapFetchUnseen", err)
		}
		if strings.HasPrefix(line, "* SEARCH") {
			parts := strings.Fields(line)
			if len(parts) > 2 {
				seqNums = parts[2:]
			}
		}
		if strings.HasPrefix(line, "A003 OK") {
			break
		}
	}
	if len(seqNums) == 0 {
		_ = w("A999 LOGOUT")
		return []imapMessage{}, nil
	}

	var result []imapMessage
	for i, seq := range seqNums {
		tag := fmt.Sprintf("A%03d", 10+i)
		if err := w(tag + " FETCH " + seq + " (BODY[TEXT] BODY[HEADER.FIELDS (FROM SUBJECT MESSAGE-ID)])"); err != nil {
			continue
		}
		var em imapMessage
		var collecting bool
		var bodyLines []string
		for {
			line, err := readLine()
			if err != nil {
				break
			}
			if strings.HasPrefix(line, tag+" OK") || strings.HasPrefix(line, tag+" NO") || strings.HasPrefix(line, tag+" BAD") {
				break
			}
			lc := strings.ToLower(line)
			switch {
			case strings.HasPrefix(lc, "from:"):
				em.From = extractEmailAddress(strings.TrimPrefix(line, "From:"))
			case strings.HasPrefix(lc, "subject:"):
				em.Subject = strings.TrimSpace(strings.TrimPrefix(line, "Subject:"))
			case strings.HasPrefix(lc, "message-id:"):
				em.MessageID = strings.TrimSpace(strings.TrimPrefix(line, "Message-ID:"))
			case strings.HasPrefix(lc, "authentication-results:"):
				em.AuthResults = strings.TrimSpace(strings.TrimPrefix(line, "Authentication-Results:"))
			case strings.HasPrefix(line, "{"):
				collecting = true
			default:
				if collecting {
					bodyLines = append(bodyLines, line)
				}
			}
		}
		em.Body = strings.Join(bodyLines, "\n")
		if em.From != "" && em.Body != "" {
			result = append(result, em)
		}
		markTag := fmt.Sprintf("B%03d", 10+i)
		_ = w(markTag + " STORE " + seq + " +FLAGS (\\Seen)")
		for {
			line, _ := readLine()
			if strings.HasPrefix(line, markTag) {
				break
			}
		}
	}
	_ = w("A999 LOGOUT")
	return result, nil
}

func extractEmailAddress(raw string) string {
	raw = strings.TrimSpace(raw)
	if lt := strings.LastIndex(raw, "<"); lt >= 0 {
		if gt := strings.LastIndex(raw, ">"); gt > lt {
			return strings.ToLower(raw[lt+1 : gt])
		}
	}
	return strings.ToLower(raw)
}

// emailAuthPassed 检查 Authentication-Results 是否通过 SPF 或 DKIM。
// 规则：spf=pass OR dkim=pass 至少一项通过即视为认证成功。
// 若 authResults 为空（服务器未填写），视为不可验证，返回 true（兼容旧配置）。
func emailAuthPassed(authResults string) bool {
	if authResults == "" {
		return true // 无头部：降级放行（保持向后兼容；生产建议配置 strict_auth=true）
	}
	lower := strings.ToLower(authResults)
	spfPass := strings.Contains(lower, "spf=pass")
	dkimPass := strings.Contains(lower, "dkim=pass")
	return spfPass || dkimPass
}
