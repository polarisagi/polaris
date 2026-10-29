package probe

import (
	"context"
	"runtime"
	"testing"
)

func TestParseNvidiaSmiOutput(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expected    AcceleratorInfo
		expectError bool
	}{
		{
			name:  "NVIDIA RTX 4090 >= 6GB",
			input: "NVIDIA GeForce RTX 4090, 24564\n",
			expected: AcceleratorInfo{
				Kind:        "nvidia",
				Model:       "NVIDIA GeForce RTX 4090",
				MemoryMB:    24564,
				Accelerated: true,
			},
		},
		{
			name:  "NVIDIA GTX 1060 3GB < 6GB",
			input: "NVIDIA GeForce GTX 1060 3GB, 3072",
			expected: AcceleratorInfo{
				Kind:        "nvidia",
				Model:       "NVIDIA GeForce GTX 1060 3GB",
				MemoryMB:    3072,
				Accelerated: false,
			},
		},
		{
			name:  "Multiple GPUs takes primary",
			input: "NVIDIA RTX 3080, 10240\nNVIDIA RTX 3080, 10240\n",
			expected: AcceleratorInfo{
				Kind:        "nvidia",
				Model:       "NVIDIA RTX 3080",
				MemoryMB:    10240,
				Accelerated: true,
			},
		},
		{
			name:  "Empty output / no GPU",
			input: "",
			expected: AcceleratorInfo{
				Kind:        "none",
				Accelerated: false,
			},
		},
		{
			name:  "Malformed output without comma",
			input: "NVIDIA-SMI has failed because no devices were found",
			expected: AcceleratorInfo{
				Kind:        "none",
				Accelerated: false,
			},
		},
		{
			name:  "Malformed output non-numeric VRAM",
			input: "GPU-Device, Unknown-VRAM",
			expected: AcceleratorInfo{
				Kind:        "none",
				Accelerated: false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNvidiaSmiOutput(tt.input)
			if (err != nil) != tt.expectError {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != tt.expected.Kind {
				t.Errorf("expected Kind %q, got %q", tt.expected.Kind, got.Kind)
			}
			if got.Model != tt.expected.Model {
				t.Errorf("expected Model %q, got %q", tt.expected.Model, got.Model)
			}
			if got.MemoryMB != tt.expected.MemoryMB {
				t.Errorf("expected MemoryMB %d, got %d", tt.expected.MemoryMB, got.MemoryMB)
			}
			if got.Accelerated != tt.expected.Accelerated {
				t.Errorf("expected Accelerated %v, got %v", tt.expected.Accelerated, got.Accelerated)
			}
		})
	}
}

func TestParseDarwinDisplaysOutput(t *testing.T) {
	sample := `Graphics/Displays:

    Intel UHD Graphics 630:

      Chipset Model: Intel UHD Graphics 630
      Type: GPU
      Bus: Built-In

    Radeon Pro 560X:

      Chipset Model: Radeon Pro 560X
      Type: GPU
      Bus: PCIe`

	got := parseDarwinDisplaysOutput(sample)
	expected := "Intel UHD Graphics 630 / Radeon Pro 560X"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}

	if empty := parseDarwinDisplaysOutput("no display detected"); empty != "" {
		t.Errorf("expected empty string, got %q", empty)
	}
}

func TestAcceleratorProbePlatform(t *testing.T) {
	probe := NewAcceleratorProbe()
	ctx := context.Background()
	info := probe.Probe(ctx)

	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		// Intel Mac 恒 Accelerated=false（ADR-0109 P3）
		if info.Accelerated {
			t.Errorf("expected Accelerated=false on darwin/amd64 Intel Mac, got true")
		}
		if info.Kind != "none" {
			t.Errorf("expected Kind='none' on darwin/amd64, got %q", info.Kind)
		}
		if info.PhysicalCPUs <= 0 {
			t.Errorf("expected positive PhysicalCPUs, got %d", info.PhysicalCPUs)
		}
	}

	cached := probe.Get()
	if cached.Kind != info.Kind || cached.Accelerated != info.Accelerated {
		t.Errorf("cached info mismatch: got %+v, expected %+v", cached, info)
	}
}

func TestAcceleratorProbeCancelledContext(t *testing.T) {
	probe := NewAcceleratorProbe()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Immediately cancelled

	info := probe.Probe(ctx)
	// Cancelled probe should complete without panic and return fallback
	if info.Accelerated && runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		t.Errorf("Intel Mac must never report accelerated")
	}
}
