package main

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestResolveSurrealBackend(t *testing.T) {
	tests := []struct {
		name              string
		configured        string
		ram               uint64
		wantNormalized    string
		wantEffective     string
		wantDeprecated    bool
		wantInvalidConfig bool
	}{
		{name: "boot empty defaults to persistent", configured: "", ram: 4 * 1024 * 1024 * 1024, wantNormalized: "surrealkv", wantEffective: "surrealkv"},
		{name: "low memory downgrades persistent", configured: "surrealkv", ram: 3 * 1024 * 1024 * 1024, wantNormalized: "surrealkv", wantEffective: "mem"},
		{name: "four gigabytes uses configured backend", configured: "surrealkv", ram: 4 * 1024 * 1024 * 1024, wantNormalized: "surrealkv", wantEffective: "surrealkv"},
		{name: "explicit mem stays memory", configured: "mem", ram: 8 * 1024 * 1024 * 1024, wantNormalized: "mem", wantEffective: "mem"},
		{name: "legacy name is normalized", configured: "rocksdb", ram: 8 * 1024 * 1024 * 1024, wantNormalized: "surrealkv", wantEffective: "surrealkv", wantDeprecated: true},
		{name: "legacy low memory normalizes then downgrades", configured: "rocksdb", ram: 3 * 1024 * 1024 * 1024, wantNormalized: "surrealkv", wantEffective: "mem", wantDeprecated: true},
		{name: "unknown does not fall back to memory", configured: "unknown", ram: 3 * 1024 * 1024 * 1024, wantInvalidConfig: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, effective, deprecated, err := resolveSurrealBackend(test.configured, test.ram)
			if test.wantInvalidConfig {
				if apperr.CodeOf(err) != apperr.CodeInvalidInput {
					t.Fatalf("resolveSurrealBackend error=%v, want invalid input", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if normalized != test.wantNormalized || effective != test.wantEffective || deprecated != test.wantDeprecated {
				t.Fatalf("resolveSurrealBackend=(%q,%q,%t), want (%q,%q,%t)", normalized, effective, deprecated, test.wantNormalized, test.wantEffective, test.wantDeprecated)
			}
		})
	}
}
