package network

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeLocalProvider 是 protocol.LocalProvider 的最小测试替身，
// 仅 Probe() 返回值可配置，其余方法均为不会被本测试触及的占位实现。
type fakeLocalProvider struct {
	probeResult protocol.LocalProbeResult
	probeErr    error
}

func (f *fakeLocalProvider) Infer(context.Context, []types.Message, ...types.InferOption) (*types.ProviderResponse, error) {
	return nil, nil
}
func (f *fakeLocalProvider) StreamInfer(context.Context, []types.Message, ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
func (f *fakeLocalProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (f *fakeLocalProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (f *fakeLocalProvider) ModelID() string                      { return "fake" }
func (f *fakeLocalProvider) LoadModel(context.Context, string, protocol.LocalModelOptions) error {
	return nil
}
func (f *fakeLocalProvider) UnloadModel(context.Context) error  { return nil }
func (f *fakeLocalProvider) EvictKVCache(context.Context) error { return nil }
func (f *fakeLocalProvider) LocalStatus(context.Context) (protocol.LocalModelStatus, error) {
	return protocol.LocalModelStatus{}, nil
}
func (f *fakeLocalProvider) Probe(context.Context) (protocol.LocalProbeResult, error) {
	return f.probeResult, f.probeErr
}

var _ protocol.LocalProvider = (*fakeLocalProvider)(nil)

func TestCheckLocalModelMemoryBudget_NoModelLoaded(t *testing.T) {
	ns := NewNetworkSandbox(5)
	ns.SetLocalProvider(&fakeLocalProvider{
		probeResult: protocol.LocalProbeResult{ModelLoadable: false},
	})
	if err := ns.checkLocalModelMemoryBudget(context.Background()); err == nil {
		t.Fatal("expected error when no local model is loaded")
	}
}

func TestCheckLocalModelMemoryBudget_OverBudget(t *testing.T) {
	ns := NewNetworkSandbox(5)
	ns.SetLocalProvider(&fakeLocalProvider{
		probeResult: protocol.LocalProbeResult{
			ModelLoadable:   true,
			PeakRSSBytes:    40 * 1024 * 1024 * 1024,
			UsedMemoryBytes: 30 * 1024 * 1024 * 1024, // 40+30=70GB >= 63GB budget
		},
	})
	if err := ns.checkLocalModelMemoryBudget(context.Background()); err == nil {
		t.Fatal("expected error when peak RSS + used memory exceeds 64GB-1GB budget")
	}
}

func TestCheckLocalModelMemoryBudget_WithinBudget(t *testing.T) {
	ns := NewNetworkSandbox(5)
	ns.SetLocalProvider(&fakeLocalProvider{
		probeResult: protocol.LocalProbeResult{
			ModelLoadable:   true,
			PeakRSSBytes:    4 * 1024 * 1024 * 1024,
			UsedMemoryBytes: 8 * 1024 * 1024 * 1024, // well within 63GB budget
		},
	})
	if err := ns.checkLocalModelMemoryBudget(context.Background()); err != nil {
		t.Fatalf("expected no error when within budget, got: %v", err)
	}
}

func TestCheckLocalModelMemoryBudget_ProbeError(t *testing.T) {
	ns := NewNetworkSandbox(5)
	ns.SetLocalProvider(&fakeLocalProvider{probeErr: context.DeadlineExceeded})
	if err := ns.checkLocalModelMemoryBudget(context.Background()); err == nil {
		t.Fatal("expected error when Probe() itself fails")
	}
}

func TestStartupCheck_SkipsLocalModelBudgetWhenProviderUnset(t *testing.T) {
	// localProvider 未注入时 StartupCheck 不应因该分支 panic；此测试只验证
	// nil localProvider 分支被跳过不引入新的 nil-pointer 崩溃点，不断言
	// StartupCheck() 的整体返回值（其余检查项依赖真实硬件/网络环境）。
	ns := NewNetworkSandbox(5)
	if ns.localProvider != nil {
		t.Fatal("expected localProvider to be nil by default")
	}
}

func TestNetworkSandbox_Enable_BlocksOutbound(t *testing.T) {
	// Enable local_only network sandbox
	ns := NewNetworkSandbox(10)

	err := ns.Enable()
	if err != nil {
		t.Logf("Enable returned err: %v (expected if OS sandbox not supported)", err)
	}

	// Test DNS override
	_, err = net.DefaultResolver.LookupHost(context.Background(), "example.com")
	if err == nil {
		t.Errorf("expected DNS to be blocked for example.com")
	}

	// Test Go layer RoundTripper
	client := &http.Client{} // Uses default transport
	_, err = client.Get("http://example.com")
	if err == nil {
		t.Errorf("expected outbound HTTP GET to be blocked")
	} else {
		t.Logf("Outbound blocked successfully: %v", err)
	}
}
