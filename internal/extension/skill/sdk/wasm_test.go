//go:build wasip1

package sdk

import (
	"testing"
)

func TestPack_RoundTrip(t *testing.T) {
	ptr := uint32(0xDEAD)
	length := uint32(0xBEEF)
	packed := pack(ptr, length)
	if uint32(packed>>32) != ptr {
		t.Errorf("packed ptr mismatch: got 0x%X", uint32(packed>>32))
	}
	if uint32(packed) != length {
		t.Errorf("packed length mismatch: got 0x%X", uint32(packed))
	}
}

func TestPack_ZeroValues(t *testing.T) {
	if pack(0, 0) != 0 {
		t.Error("pack(0,0) should return 0")
	}
}

func TestPolarisAlloc_NonZeroSize(t *testing.T) {
	ptr := polaris_malloc(64)
	if ptr == 0 {
		t.Fatal("expected non-zero ptr for 64-byte allocation")
	}
	if _, ok := retain[ptr]; !ok {
		t.Error("allocated buffer should be in retain map")
	}
	polaris_free(ptr)
	if _, ok := retain[ptr]; ok {
		t.Error("freed buffer should be removed from retain map")
	}
}

func TestPolarisAlloc_ZeroSize(t *testing.T) {
	ptr := polaris_malloc(0)
	if ptr != 0 {
		t.Errorf("expected 0 for zero-size allocation, got %d", ptr)
	}
}

func TestRegister_SetsHandler(t *testing.T) {
	defer func() { activeHandler = nil }()
	called := false
	Register(func(input []byte) ([]byte, error) {
		called = true
		return input, nil
	})
	if activeHandler == nil {
		t.Fatal("activeHandler should be non-nil after Register")
	}
	// 直接调用注册的 handler
	out, err := activeHandler([]byte("hello"))
	if err != nil || string(out) != "hello" || !called {
		t.Errorf("handler not working correctly")
	}
}

func TestRun_NilHandler(t *testing.T) {
	defer func() { activeHandler = nil }()
	activeHandler = nil
	result := run(0, 0)
	if result != 0 {
		t.Errorf("expected 0 for nil handler, got %d", result)
	}
}

func TestRun_ZeroPtrZeroLen(t *testing.T) {
	defer func() { activeHandler = nil }()
	Register(func(input []byte) ([]byte, error) {
		return []byte("output"), nil
	})
	// ptr=0, len=0: inBuf stays nil, handler called with nil input
	// output="output" → should be allocated and packed
	result := run(0, 0)
	outPtr := uint32(result >> 32)
	outLen := uint32(result)
	if outPtr == 0 || outLen == 0 {
		t.Errorf("expected non-zero packed result, got ptr=%d len=%d", outPtr, outLen)
	}
	// cleanup
	polaris_free(outPtr)
}
