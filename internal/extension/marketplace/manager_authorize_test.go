package marketplace_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
)

// TestManager_Authorize_NilPolicyGate 验证 policyGate 为 nil 时返回错误而非 panic（GR-8-001）
func TestManager_Authorize_NilPolicyGate(t *testing.T) {
	m := marketplace.NewManager(nil, nil, nil, nil, nil, nil)
	err := m.Authorize(context.Background(), protocol.ExtensionInstallRequest{})
	require.Error(t, err, "policyGate 为 nil 时应返回错误而非 panic")
}
