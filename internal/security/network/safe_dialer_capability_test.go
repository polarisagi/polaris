package network

import (
	"context"
	"net/http"
	"testing"
)

// stubRoundTripper 记录是否被调用到，避免真实网络请求。
type stubRoundTripper struct {
	called bool
	resp   *http.Response
	err    error
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.called = true
	return s.resp, s.err
}

// TestCheckCapability_ReadOnlyBlocksWriteMethods 验证 CapNetworkRead 只放行
// GET/HEAD/OPTIONS，其余方法（POST/PUT/DELETE/PATCH）一律拒绝——这是 web_search/
// fetch_url 等只读工具的纵深防御层，不依赖工具实现本身"恰好硬编码 GET"。
func TestCheckCapability_ReadOnlyBlocksWriteMethods(t *testing.T) {
	readOnlyAllowed := []string{"GET", "HEAD", "OPTIONS", "get", "Head"}
	for _, m := range readOnlyAllowed {
		if err := CheckCapability(CapNetworkRead, m); err != nil {
			t.Errorf("CapNetworkRead should allow method %q, got err: %v", m, err)
		}
	}

	writeMethods := []string{"POST", "PUT", "DELETE", "PATCH"}
	for _, m := range writeMethods {
		err := CheckCapability(CapNetworkRead, m)
		if err == nil {
			t.Errorf("CapNetworkRead should block method %q, got nil error", m)
		}
		if _, ok := err.(*ErrCapabilityWriteBlocked); !ok {
			t.Errorf("expected ErrCapabilityWriteBlocked for method %q, got: %T %v", m, err, err)
		}
	}
}

// TestCheckCapability_WriteLevelsAllowAllMethods 验证 CapNetworkWriteLocal/
// CapNetworkWrite 两级放行所有 HTTP 方法（write 语义由调用方/DialContext 的
// SSRF 校验负责，不在这一层重复拦截）。
func TestCheckCapability_WriteLevelsAllowAllMethods(t *testing.T) {
	for _, cap := range []Capability{CapNetworkWriteLocal, CapNetworkWrite} {
		for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
			if err := CheckCapability(cap, m); err != nil {
				t.Errorf("capability %d should allow method %q, got err: %v", cap, m, err)
			}
		}
	}
}

// TestCapabilityRoundTripper_BlocksBeforeInnerCall 验证能力校验失败时直接
// short-circuit，不会把请求转发给内层 RoundTripper（防止只读工具的畸形请求
// 真的打到网络上）。
func TestCapabilityRoundTripper_BlocksBeforeInnerCall(t *testing.T) {
	inner := &stubRoundTripper{}
	rt := WrapCapability(inner, CapNetworkRead)

	req, err := http.NewRequestWithContext(context.Background(), "POST", "https://example.com", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	_, rtErr := rt.RoundTrip(req)
	if rtErr == nil {
		t.Fatal("expected capability check to block POST under CapNetworkRead")
	}
	if inner.called {
		t.Error("inner RoundTripper must not be called when capability check fails")
	}
}

// TestCapabilityRoundTripper_AllowsPermittedMethod 验证能力校验通过后请求
// 正常转发给内层 RoundTripper。
func TestCapabilityRoundTripper_AllowsPermittedMethod(t *testing.T) {
	inner := &stubRoundTripper{resp: &http.Response{StatusCode: 200}}
	rt := WrapCapability(inner, CapNetworkRead)

	req, err := http.NewRequestWithContext(context.Background(), "GET", "https://example.com", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, rtErr := rt.RoundTrip(req)
	if rtErr != nil {
		t.Fatalf("unexpected error: %v", rtErr)
	}
	if !inner.called {
		t.Error("inner RoundTripper should have been called for a permitted GET")
	}
	if resp.StatusCode != 200 {
		t.Errorf("unexpected status code: %d", resp.StatusCode)
	}
}

// TestWrapCapability_NilInnerDefaultsToDefaultTransport 验证 nil inner 时
// 回退到 http.DefaultTransport，而不是 panic 或裸 nil 解引用。
func TestWrapCapability_NilInnerDefaultsToDefaultTransport(t *testing.T) {
	rt := WrapCapability(nil, CapNetworkRead)
	crt, ok := rt.(*CapabilityRoundTripper)
	if !ok {
		t.Fatalf("expected *CapabilityRoundTripper, got %T", rt)
	}
	if crt.inner != http.DefaultTransport {
		t.Errorf("expected inner to default to http.DefaultTransport, got %v", crt.inner)
	}
}
