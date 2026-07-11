package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func runVaultCmd(args []string) error {
	if len(args) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "vault: missing subcommand (e.g. init, rotate-master-key)")
	}

	switch args[0] {
	case "init":
		return runVaultInit()
	case "rotate-master-key":
		return runVaultRotate()
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("vault: unknown subcommand %q", args[0]))
	}
}

func runVaultInit() error {
	// resolveDataDirBase(nil)：vault 子命令不加载完整 config（与 benchmark.go 的
	// runBenchmarkRouting 同一模式），只识别 POLARIS_DATA_DIR env 覆盖，
	// 不识别 cfg.System.DataDir——避免 vault key 落在与 server 启动路径不同的
	// 硬编码 home 目录（Docker 部署下 $HOME 常非持久化卷）。
	dataDir, err := resolveDataDirBase(nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault init failed", err)
	}
	_, err = credential.NewVaultInDir(dataDir)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault init failed", err)
	}
	slog.Info("polaris: credential vault initialized successfully", "data_dir", dataDir)
	return nil
}

func runVaultRotate() error {
	dataDir, err := resolveDataDirBase(nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault rotate failed", err)
	}
	keyPath := filepath.Join(dataDir, "vault.key")
	dbPath := filepath.Join(dataDir, "data", "polaris.db")

	oldVault, err := credential.NewVaultInDir(dataDir)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault rotate: failed to load existing vault", err)
	}

	// Generate new key
	newKey, err := credential.GenerateNewKey(keyPath + ".new")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault rotate: failed to generate new key", err)
	}

	newVault, err := credential.NewVaultWithKey(newKey)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault rotate: failed to load new vault", err)
	}

	// Connect to DB and rotate.
	// 驱动名必须是 "sqlite"（modernc.org/sqlite，纯 Go，ADR-0011 零 CGO 约束），
	// 而非 "sqlite3"（mattn/go-sqlite3，仅在测试文件里注册，main 包从未 blank-import，
	// 用 "sqlite3" 会导致运行时 "unknown driver" 报错）。
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault rotate: failed to open db", err)
	}
	defer db.Close()

	oldRepo := repo.NewSQLiteProviderRepository(db).WithVault(oldVault)
	newRepo := repo.NewSQLiteProviderRepository(db).WithVault(newVault)

	providers, err := oldRepo.ListProviders(context.Background())
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault rotate: failed to list providers", err)
	}

	for _, p := range providers {
		if p.APIKey != "" {
			err = newRepo.UpdateProviderAPIKey(context.Background(), p.ID, p.APIKey, p.UpdatedAt)
			if err != nil {
				return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("vault rotate: failed to update provider %s", p.ID), err)
			}
		}
	}

	// Atomically swap keys
	if err := os.Rename(keyPath+".new", keyPath); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault rotate: failed to swap keys", err)
	}

	slog.Info("polaris: credential master key rotated successfully", "providers_updated", len(providers))
	return nil
}
