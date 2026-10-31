package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type profilePromptStub struct{ protocol.PromptFacade }

func (profilePromptStub) ReadPrompt(string, string) string { return "" }
func (profilePromptStub) PlatformHintFor(string) string    { return "" }

type profileCoreStub struct {
	protocol.ImmutableCore
	fields *protocol.ImmutableCoreFields
}

func (c *profileCoreStub) Fields() *protocol.ImmutableCoreFields { return c.fields }
func (c *profileCoreStub) PrependToMessages(messages []types.Message) []types.Message {
	return messages
}

type profileMemoryStub struct {
	protocol.MemoryFacade
	core    protocol.ImmutableCore
	profile *types.UserProfile
	err     error
	key     string
}

func (m *profileMemoryStub) ImmutableCore() protocol.ImmutableCore { return m.core }
func (m *profileMemoryStub) GetUserProfile(_ context.Context, key string) (*types.UserProfile, error) {
	m.key = key
	return m.profile, m.err
}

type profileAgentStub struct {
	protocol.AgentController
	memory protocol.MemoryFacade
}

func (a *profileAgentStub) Memory() protocol.MemoryFacade { return a.memory }

type emptyProfileRegistry struct{}

func (emptyProfileRegistry) PickProvider(string) protocol.Provider                      { return nil }
func (emptyProfileRegistry) PickProviderName(string) string                             { return "" }
func (emptyProfileRegistry) PickProviderByRecordID(string) protocol.Provider            { return nil }
func (emptyProfileRegistry) UnregisterAll()                                             {}
func (emptyProfileRegistry) RegisterWithRole(string, string, string, protocol.Provider) {}

func TestInjectSystemPromptUsesVersionedProfileAndClearsMissingOrFailedReads(t *testing.T) {
	fields := &protocol.ImmutableCoreFields{UserProfile: "stale profile from previous session"}
	core := &profileCoreStub{fields: fields}
	memory := &profileMemoryStub{
		core: core,
		profile: &types.UserProfile{
			ProfileKey:  types.GlobalUserProfileKey,
			StableFacts: map[string]any{"role": "reviewer"},
		},
	}
	agent := &profileAgentStub{memory: memory}
	service := &PromptAssemblyService{PromptMgr: profilePromptStub{}, Registry: emptyProfileRegistry{}}

	service.InjectSystemPrompt(context.Background(), agent, nil, "query")
	if memory.key != types.GlobalUserProfileKey {
		t.Fatalf("profile read must use versioned key, got %q", memory.key)
	}
	if fields.UserProfile == "" || fields.UserProfile == "stale profile from previous session" {
		t.Fatalf("valid global profile should replace stale content, got %q", fields.UserProfile)
	}

	memory.profile = nil
	service.InjectSystemPrompt(context.Background(), agent, nil, "query")
	if fields.UserProfile != "" {
		t.Fatalf("missing profile must clear shared field, got %q", fields.UserProfile)
	}

	fields.UserProfile = "stale after retrieval failure"
	memory.err = errors.New("profile store unavailable")
	service.InjectSystemPrompt(context.Background(), agent, nil, "query")
	if fields.UserProfile != "" {
		t.Fatalf("profile read failure must clear shared field, got %q", fields.UserProfile)
	}
}
