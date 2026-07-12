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
	rtp := NewRedTeamProtocol(harness.NewSQLiteEvalStore(&mockStorePut{}, nil))

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

	err = rtp.RunAndInject(context.Background())
	if err != nil {
		t.Fatal(err)
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
