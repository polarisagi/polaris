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
)

func runVaultCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("vault: missing subcommand (e.g. init, rotate-master-key)")
	}

	switch args[0] {
	case "init":
		return runVaultInit()
	case "rotate-master-key":
		return runVaultRotate()
	default:
		return fmt.Errorf("vault: unknown subcommand %q", args[0])
	}
}

func runVaultInit() error {
	_, err := credential.NewVault()
	if err != nil {
		return fmt.Errorf("vault init failed: %w", err)
	}
	slog.Info("polaris: credential vault initialized successfully")
	return nil
}

func runVaultRotate() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("vault rotate failed: %w", err)
	}
	keyPath := filepath.Join(homeDir, ".polarisagi", "polaris", "vault.key")
	dbPath := filepath.Join(homeDir, ".polarisagi", "polaris", "data", "polaris.db")

	oldVault, err := credential.NewVault()
	if err != nil {
		return fmt.Errorf("vault rotate: failed to load existing vault: %w", err)
	}

	// Generate new key
	newKey, err := credential.GenerateNewKey(keyPath + ".new")
	if err != nil {
		return fmt.Errorf("vault rotate: failed to generate new key: %w", err)
	}

	newVault, err := credential.NewVaultWithKey(newKey)
	if err != nil {
		return fmt.Errorf("vault rotate: failed to load new vault: %w", err)
	}

	// Connect to DB and rotate
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("vault rotate: failed to open db: %w", err)
	}
	defer db.Close()

	oldRepo := repo.NewSQLiteProviderRepository(db).WithVault(oldVault)
	newRepo := repo.NewSQLiteProviderRepository(db).WithVault(newVault)

	providers, err := oldRepo.ListProviders(context.Background())
	if err != nil {
		return fmt.Errorf("vault rotate: failed to list providers: %w", err)
	}

	for _, p := range providers {
		if p.APIKey != "" {
			err = newRepo.UpdateProviderAPIKey(context.Background(), p.ID, p.APIKey, p.UpdatedAt)
			if err != nil {
				return fmt.Errorf("vault rotate: failed to update provider %s: %w", p.ID, err)
			}
		}
	}

	// Atomically swap keys
	if err := os.Rename(keyPath+".new", keyPath); err != nil {
		return fmt.Errorf("vault rotate: failed to swap keys: %w", err)
	}

	slog.Info("polaris: credential master key rotated successfully", "providers_updated", len(providers))
	return nil
}
