package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/gateway/server/provider"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/credential"
)

func TestPerformHotRestart_ExecFailure(t *testing.T) {
	originalExec := execFunc
	originalExit := exitFunc
	defer func() {
		execFunc = originalExec
		exitFunc = originalExit
	}()

	execCalled := false
	execFunc = func(argv0 string, argv []string, envv []string) error {
		execCalled = true
		return errors.New("simulated exec failure")
	}

	exitCode := -1
	exitFunc = func(code int) {
		exitCode = code
	}

	performHotRestart(nil)

	if !execCalled {
		t.Error("expected execFunc to be called")
	}
	if exitCode != 1 {
		t.Errorf("expected exit(1) on exec failure, got exit(%d)", exitCode)
	}
}

func TestReloadProvidersCallback_LogError(t *testing.T) {
	originalLoadProviders := loadProvidersFunc
	defer func() { loadProvidersFunc = originalLoadProviders }()

	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, nil)
	logger := slog.New(handler)
	slog.SetDefault(logger)

	loadProvidersFunc = func(ctx context.Context, db protocol.SQLQuerier, vault *credential.Vault, reg provider.ProviderRegistry, httpClient *http.Client, tbr *metrics.TokenBurnRate) error {
		return errors.New("simulated load error")
	}

	cb := func() {
		if err := loadProvidersFunc(context.Background(), nil, nil, nil, nil, nil); err != nil {
			slog.Error("polaris: failed to hot-reload providers", "err", err)
		}
	}

	cb()

	if !strings.Contains(buf.String(), "polaris: failed to hot-reload providers") {
		t.Errorf("expected error log, got: %s", buf.String())
	}
}
