package channels

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

	perrors "github.com/polarisagi/polaris/internal/errors"
)

type dialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

func (m *Manager) startEmailPoller(channelID string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	go m.runEmailPoller(ctx, channelID, cfg)
}

func (m *Manager) runEmailPoller(ctx context.Context, channelID string, cfg map[string]any) {
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
		if m.safeDialer == nil {
			slog.Error("email: SafeDialer 未注入，拒绝 IMAP 连接（SSRF 防护）", "channel", channelID, "err", perrors.New(perrors.CodeInternal, "log event"))
			return
		}
		msgs, err := imapFetchUnseen(ctx, m.safeDialer.DialContext, imapHost+":"+imapPort, address, password)
		if err != nil {
			slog.Error("email: IMAP fetch failed", "err", err)
			continue
		}
		for _, em := range msgs {
			fromAddr := strings.ToLower(em.From)
			if len(allowedSenders) > 0 && !allowedSenders[fromAddr] {
				continue
			}
			go m.onMessage("email", channelID, cfg, Message{
				Text:       em.Body,
				ChatID:     em.From,
				UserID:     em.From,
				ReplyToken: em.MessageID,
			})
		}
	}
}

func emailSendMessage(smtpHost, smtpPort, address, password, to, subject, body string) error {
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
	From      string
	Subject   string
	MessageID string
	Body      string
}

// imapFetchUnseen 通过调用方注入的 dialCtx 建立 TCP 连接（必须经 SafeDialer 校验），
// 再在已校验连接上升级 TLS，防止直接调用 tls.DialWithDialer 绕过 SSRF 拦截。
func imapFetchUnseen(ctx context.Context, dialCtx dialContextFunc, addr, user, password string) ([]imapMessage, error) { //nolint:gocyclo
	host := strings.Split(addr, ":")[0]
	rawConn, err := dialCtx(ctx, "tcp", addr)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("imap: dial: %v", err), err)
	}
	tlsCfg := &tls.Config{ServerName: host}
	conn := tls.Client(rawConn, tlsCfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("imap: tls handshake: %v", err), err)
	}
	defer conn.Close()

	r := bufio.NewReader(conn)
	w := func(cmd string) error {
		_, err := fmt.Fprintf(conn, "%s\r\n", cmd)
		return err
	}
	readLine := func() (string, error) {
		line, err := r.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}

	if _, err := readLine(); err != nil {
		return nil, err
	}
	if err := w("A001 LOGIN " + user + " " + password); err != nil {
		return nil, err
	}
	if line, err := readLine(); err != nil || !strings.HasPrefix(line, "A001 OK") {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("imap: login failed: %s", line))
	}
	if err := w("A002 SELECT INBOX"); err != nil {
		return nil, err
	}
	for {
		line, err := readLine()
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(line, "A002 OK") {
			break
		}
		if strings.HasPrefix(line, "A002 NO") || strings.HasPrefix(line, "A002 BAD") {
			return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("imap: SELECT INBOX failed: %s", line))
		}
	}
	if err := w("A003 SEARCH UNSEEN"); err != nil {
		return nil, err
	}
	var seqNums []string
	for {
		line, err := readLine()
		if err != nil {
			return nil, err
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
		return nil, nil
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
