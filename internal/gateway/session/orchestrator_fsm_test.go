package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeSink struct {
	events []Event
}

func (s *fakeSink) Emit(e Event) error {
	s.events = append(s.events, e)
	return nil
}

func TestOrchestrator_FSMSlidingWindowLeak(t *testing.T) {
	cfg := config.Config{}
	cfg.Thresholds.Session.LeakScanWindowBytes = 64
	config.Update(&cfg)

	o := &orchestrator{}

	g := guard.NewSystemPromptGuard(3)
	g.AddFragment("alpha beta gamma delta epsilon")

	var replyBuilder []byte
	var errBuilder string
	windowSize := 64
	var leakWindow []byte

	sink := &fakeSink{}

	o.handleFSMEvent(sink, "sess-1", types.AgentStreamEvent{
		Type:    types.AgentStreamEventToken,
		Content: "alpha ",
	}, g, &replyBuilder, &errBuilder, &leakWindow, windowSize)
	require.Equal(t, "alpha ", string(replyBuilder))

	o.handleFSMEvent(sink, "sess-1", types.AgentStreamEvent{
		Type:    types.AgentStreamEventToken,
		Content: "beta ",
	}, g, &replyBuilder, &errBuilder, &leakWindow, windowSize)
	require.Equal(t, "alpha beta ", string(replyBuilder))

	o.handleFSMEvent(sink, "sess-1", types.AgentStreamEvent{
		Type:    types.AgentStreamEventToken,
		Content: "gamma",
	}, g, &replyBuilder, &errBuilder, &leakWindow, windowSize)
	require.Equal(t, "alpha beta ", string(replyBuilder))

	o.handleFSMEvent(sink, "sess-1", types.AgentStreamEvent{
		Type:    types.AgentStreamEventToken,
		Content: " normal text",
	}, g, &replyBuilder, &errBuilder, &leakWindow, windowSize)
	require.Equal(t, "alpha beta  normal text", string(replyBuilder))

	// Turn 2
	var leakWindowTurn2 []byte
	o.handleFSMEvent(sink, "sess-1", types.AgentStreamEvent{
		Type:    types.AgentStreamEventToken,
		Content: "gamma",
	}, g, &replyBuilder, &errBuilder, &leakWindowTurn2, windowSize)
	require.Equal(t, "alpha beta  normal textgamma", string(replyBuilder))
}
