package sandbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/types"
)

type ExtensionUninstallPayload struct {
	InstanceID  string `json:"instance_id"`
	CatalogID   string `json:"catalog_id"`
	InstallPath string `json:"install_path"`
	ExtType     string `json:"ext_type"`
	TrustTier   int    `json:"trust_tier"`
	RuntimeID   string `json:"runtime_id"`
}

type ExtensionUninstallHandler struct {
	router  *SandboxRouter
	extRepo protocol.ExtensionRepository
}

func NewExtensionUninstallHandler(router *SandboxRouter, extRepo protocol.ExtensionRepository) *ExtensionUninstallHandler {
	return &ExtensionUninstallHandler{
		router:  router,
		extRepo: extRepo,
	}
}

// Handle consumes the extension_uninstall event.
//nolint:nestif

func (h *ExtensionUninstallHandler) Handle(ctx context.Context, record *store.OutboxRecord) error {
	var payload ExtensionUninstallPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		slog.Error("sandbox: failed to unmarshal extension_uninstall payload", "err", err)
		return nil // Drop invalid payload
	}

	// Create a context with timeout to force destroy if hanging
	execCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	success := true

	if payload.InstallPath != "" && payload.ExtType == "plugin" {
		success = h.executeHook(execCtx, payload)
	}

	// Timeout 则强制保留现场，仅成功时擦除文件
	if success {
		if payload.InstallPath != "" {
			_ = os.RemoveAll(payload.InstallPath)
		}
		// Hard delete from DB
		_ = h.extRepo.DeleteInstance(ctx, payload.InstanceID)
	} else {
		// Update status to error
		_ = h.extRepo.UpdateInstanceStatus(ctx, payload.InstanceID, "error", "uninstall hook failed or timed out")
	}

	return nil
}

func (h *ExtensionUninstallHandler) executeHook(ctx context.Context, payload ExtensionUninstallPayload) bool {
	raw, err := os.ReadFile(filepath.Join(payload.InstallPath, "plugin.json"))
	if err != nil {
		return true // No plugin.json, nothing to do
	}

	var bundle protocol.PluginBundleManifest
	_ = json.Unmarshal(raw, &bundle)

	hook := bundle.Hooks["uninstall"]
	if hook == "" {
		return true // No uninstall hook
	}

	hookPath := filepath.Join(payload.InstallPath, hook)
	cleanHook := filepath.Clean(hookPath)
	cleanBase := filepath.Clean(payload.InstallPath)
	if !strings.HasPrefix(cleanHook, cleanBase+string(filepath.Separator)) && cleanHook != cleanBase {
		return true // Invalid path traversal
	}

	provider, err := h.router.RouteByTier(types.SandboxContainer, types.TrustTier(payload.TrustTier))
	if err != nil {
		slog.Warn("sandbox: failed to route uninstall hook", "err", err, "ext", payload.InstanceID)
		return false
	}

	runner, ok := provider.(interface {
		RunHook(context.Context, string, string) error
	})
	if !ok {
		slog.Warn("sandbox: routed provider does not support RunHook", "ext", payload.InstanceID)
		return true // Ignore if unsupported
	}

	if err := runner.RunHook(ctx, hookPath, payload.InstallPath); err != nil {
		slog.Warn("sandbox: uninstall hook failed", "err", err, "ext", payload.InstanceID)
		return false
	}

	return true
}
