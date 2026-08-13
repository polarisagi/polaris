package adapter

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type mockSafeDialer struct {
	dialFunc func(ctx context.Context, network, address string) (net.Conn, error)
}

func (m *mockSafeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if m.dialFunc != nil {
		return m.dialFunc(ctx, network, address)
	}
	return nil, apperr.New(apperr.CodeNetworkUnavailable, "mock error")
}

func TestEmailSendMessage_NilDialer(t *testing.T) {
	err := EmailSendMessage(context.Background(), nil, "smtp.example.com", "587", "user", "pass", "to@example.com", "Subject", "Body")
	if err == nil || !strings.Contains(err.Error(), "SafeDialer 未注入") {
		t.Fatalf("expected SSRF protection error, got: %v", err)
	}
}

func TestEmailSendMessage_DialerError(t *testing.T) {
	dialer := &mockSafeDialer{
		dialFunc: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, apperr.New(apperr.CodeNetworkUnavailable, "mock dial error")
		},
	}
	err := EmailSendMessage(context.Background(), dialer, "smtp.example.com", "587", "user", "pass", "to@example.com", "Subject", "Body")
	if err == nil || !strings.Contains(err.Error(), "mock dial error") {
		t.Fatalf("expected dial error, got: %v", err)
	}
}
