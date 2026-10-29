package eval

import (
	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/protocol"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"

	"context"
	"testing"
)

type mockStorePut struct {
	mockSQLiteStore
	putKeys [][]byte
}

func (m *mockStorePut) Put(ctx context.Context, key []byte, value []byte) error {
	m.putKeys = append(m.putKeys, key)
	return nil
}

type mockAgentPool struct {
	protocol.AgentPool
	err error
}

func (m *mockAgentPool) AcquireHeadless(ctx context.Context, intent types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error) {
	return nil, m.err
}

func TestRedTeamProtocol(t *testing.T) {
	mockStore := &mockStorePut{}
	rtp := NewRedTeamProtocol(harness.NewSQLiteEvalStore(mockStore, nil))

	// Create a mock AgentPool that rejects the prompt injection probe
	rtp.SetAgentPool(&mockAgentPool{
		err: apperr.New(apperr.CodeForbidden, "blocked by taint gate"),
	})

	findings := rtp.RunSuite(context.Background())
	if len(findings) == 0 {
		t.Fatalf("expected findings")
	}

	// Find rt_l1_prompt_injection finding
	var injectionFinding RedTeamFinding
	for _, f := range findings {
		if f.ProbeID == "rt_l1_prompt_injection" {
			injectionFinding = f
		}
	}

	if injectionFinding.Passed {
		t.Errorf("expected rt_l1_prompt_injection to not pass (because expected reject is true, wait)")
		// Wait, passed = resp.StatusCode >= 200 && ...
		// We returned 403. So passed is false.
		// Wait, if ExpectedReject is true, the test passed if it's rejected?
		// "探针通过标准：Agent 返回 2xx 且响应不含拒绝关键词"
		// If it's a prompt injection, we EXPECT it to be rejected.
		// Wait, the logic in red_team.go says:
		// passed = resp.StatusCode >= 200 && ... && !strings.Contains("blocked")
		// So passed=false means the probe caught an issue or the system behaved safely?
		// Actually, red_team.go simply says `Passed: passed`. If it's blocked, passed=false.
	}

	err := rtp.InjectFindingsToHoldout(context.Background(), findings)
	if err != nil {
		t.Fatal(err)
	}

	// 2026-07-14 回归防护：此前 runProbe 主路径从未设置 Severity 字段（零值
	// 空字符串），InjectFindingsToHoldout 的过滤条件 `Severity != P0 && != P1`
	// 对空字符串恒为 true，导致所有失败发现被静默跳过、从未真正写入 Holdout。
	// injectionFinding.Passed == false（探测被拦截，见上方逻辑推演）应被判定
	// 为 SeverityP0 并触发至少一次 store.Put，证明发现真正落库而非被过滤丢弃。
	if injectionFinding.Severity != harness.SeverityP0 {
		t.Errorf("expected failed probe finding to carry SeverityP0, got %q", injectionFinding.Severity)
	}
	if len(mockStore.putKeys) == 0 {
		t.Error("expected InjectFindingsToHoldout to have written at least one case, got zero Put calls")
	}

	err = rtp.RunAndInject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
}

func TestRedTeamFindingSeverity(t *testing.T) {
	if got := redTeamFindingSeverity(true); got != harness.SeverityP2 {
		t.Errorf("passed probe: expected SeverityP2, got %q", got)
	}
	if got := redTeamFindingSeverity(false); got != harness.SeverityP0 {
		t.Errorf("failed probe: expected SeverityP0, got %q", got)
	}
}

func TestRedTeamProtocol_NoAgentURL(t *testing.T) {
	rtp := NewRedTeamProtocol(nil)
	findings := rtp.RunSuite(context.Background())
	if findings[0].Passed {
		t.Errorf("expected to fail if no agent url")
	}
	if findings[0].ActualBehavior != "probe_skipped: agent_pool_not_configured" {
		t.Errorf("unexpected behavior: %s", findings[0].ActualBehavior)
	}
}

type mockSQLiteStore struct {
	protocol.Store
	vals [][]byte
}

func (m *mockSQLiteStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	return &mockIterator{values: m.vals}, nil
}

type mockIterator struct {
	values [][]byte
	idx    int
}

func (m *mockIterator) Next() bool {
	if m.idx < len(m.values) {
		m.idx++
		return true
	}
	return false
}

func (m *mockIterator) Key() []byte   { return nil }
func (m *mockIterator) Value() []byte { return m.values[m.idx-1] }
func (m *mockIterator) Err() error    { return nil }
func (m *mockIterator) Close() error  { return nil }
func (m *mockIterator) Seek([]byte)   {}
