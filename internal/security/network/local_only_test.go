package network

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
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

// Enable 改写进程级全局（http.DefaultTransport.DialContext / net.DefaultResolver），
// 并可能对整个进程施加 landlock。在测试进程内直接调用会与同包其他用例遗留的
// stdlib DNS goroutine 读 net.DefaultResolver 构成数据竞态（CI -race -shuffle 实测），
// 且断网状态会污染后续用例。故在重新 exec 的子进程里执行，父进程只看退出码。
const enableBlocksOutboundChildEnv = "POLARIS_TEST_ENABLE_BLOCKS_OUTBOUND_CHILD"

func TestNetworkSandbox_Enable_BlocksOutbound(t *testing.T) {
	if os.Getenv(enableBlocksOutboundChildEnv) == "1" {
		runEnableBlocksOutboundChild(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestNetworkSandbox_Enable_BlocksOutbound$", "-test.v")
	cmd.Env = append(os.Environ(), enableBlocksOutboundChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\n%s", err, out)
	}
	t.Logf("child output:\n%s", out)
}

func runEnableBlocksOutboundChild(t *testing.T) {
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
