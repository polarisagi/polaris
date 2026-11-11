package main

import (
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const surrealPersistentRAMLimit = 4 * 1024 * 1024 * 1024

func resolveSurrealBackend(configured string, totalRAM uint64) (string, string, bool, error) {
	if configured == "" {
		configured = "surrealkv"
	}
	normalized, deprecated, err := store.NormalizeSurrealBackend(configured)
	if err != nil {
		return "", "", false, apperr.Wrap(apperr.CodeInvalidInput, "boot_surreal: invalid configured backend", err)
	}
	effective := normalized
	if normalized == "surrealkv" && totalRAM < surrealPersistentRAMLimit {
		effective = "mem"
	}
	return normalized, effective, deprecated, nil
}
