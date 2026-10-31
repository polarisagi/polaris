package memory

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	memstore "github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestSessionMemoryFacade_NilCoreSemantics(t *testing.T) {
	if NewSessionMemoryFacade(nil) != nil {
		t.Fatal("nil facade must produce nil session facade")
	}
	var typedNil *MemoryFacadeImpl
	if NewSessionMemoryFacade(typedNil) != nil {
		t.Fatal("typed nil facade must produce nil session facade")
	}
	view := NewSessionMemoryFacade(&MemoryFacadeImpl{})
	if view == nil || view.ImmutableCore() != nil {
		t.Fatal("facade without a backing core must expose a nil core")
	}
}

func TestSessionMemoryFacade_IsolatesCoreAndSharesBackend(t *testing.T) {
	mem := NewMemImpl(testutil.NewMockStore())
	base := NewMemoryFacade(NewMemorySystemFromMemImpl(mem))
	baseCore := base.ImmutableCore()
	baseFields := baseCore.Fields()
	baseFields.AgentName = "global-agent"
	baseFields.GlobalGoal = "global-goal"
	baseFields.UserPreferences["language"] = "zh-CN"
	baseFields.AmbientContext = "stale ambient"
	baseFields.VolatileBlock = "stale volatile"
	baseFields.UserProfile = "stale profile"

	a := NewSessionMemoryFacade(base)
	b := NewSessionMemoryFacade(base)
	if a.ImmutableCore() == b.ImmutableCore() || a.ImmutableCore() == baseCore {
		t.Fatal("Agent core views must have distinct identities")
	}
	if _, ok := a.ImmutableCore().(*memstore.ImmutableCore); !ok {
		t.Fatal("session core must retain store.ImmutableCore optional capabilities")
	}
	if _, ok := a.ImmutableCore().(interface{ HasPhaseContracts() bool }); !ok {
		t.Fatal("session core must retain phase-contract capability")
	}

	for name, core := range map[string]protocol.ImmutableCore{"a": a.ImmutableCore(), "b": b.ImmutableCore()} {
		fields := core.Fields()
		if fields.AmbientContext != "" || fields.VolatileBlock != "" || fields.UserProfile != "" {
			t.Fatalf("%s inherited per-request fields: %+v", name, fields)
		}
		if fields.AgentName != "global-agent" || fields.GlobalGoal != "global-goal" || fields.UserPreferences["language"] != "zh-CN" {
			t.Fatalf("%s lost global core fields: %+v", name, fields)
		}
	}
	a.ImmutableCore().Fields().UserPreferences["language"] = "en"
	if b.ImmutableCore().Fields().UserPreferences["language"] != "zh-CN" || baseCore.Fields().UserPreferences["language"] != "zh-CN" {
		t.Fatal("UserPreferences must be copied per session")
	}

	if err := a.AddWorkingContext(t.Context(), "from-a"); err != nil {
		t.Fatal(err)
	}
	if err := b.AddWorkingContext(t.Context(), "from-b"); err != nil {
		t.Fatal(err)
	}
	messages := mem.Working().Context().Messages()
	if len(messages) != 2 || messages[0].Content != "from-a" || messages[1].Content != "from-b" {
		t.Fatalf("non-core memory backend should remain shared, got %+v", messages)
	}
}

func TestSessionMemoryFacade_ConcurrentPromptRenderingIsIsolated(t *testing.T) {
	mem := NewMemImpl(testutil.NewMockStore())
	base := NewMemoryFacade(NewMemorySystemFromMemImpl(mem))
	a := NewSessionMemoryFacade(base)
	b := NewSessionMemoryFacade(base)

	start := make(chan struct{})
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for _, pair := range []struct {
		name  string
		other string
		core  protocol.ImmutableCore
	}{{"alpha", "beta", a.ImmutableCore()}, {"beta", "alpha", b.ImmutableCore()}} {
		wg.Add(1)
		go func(name, other string, core protocol.ImmutableCore) {
			defer wg.Done()
			<-start
			for i := range 1000 {
				fields := core.Fields()
				fields.UserProfile = fmt.Sprintf("profile-%s-%d", name, i)
				fields.AmbientContext = fmt.Sprintf("ambient-%s-%d", name, i)
				fields.VolatileBlock = fmt.Sprintf("volatile-%s-%d", name, i)
				fields.UserPreferences["session"] = name

				stable := core.StableMessage().Content
				volatile := core.VolatileContent()
				contract, ok := core.(interface{ StableMessagesWithContracts() []types.Message })
				if !ok {
					errCh <- fmt.Errorf("%s core lost phase-contract capability", name)
					return
				}
				contractMessages := contract.StableMessagesWithContracts()
				all := stable + volatile
				for _, message := range contractMessages {
					all += message.Content
				}
				if !strings.Contains(stable, "profile-"+name+"-") || !strings.Contains(volatile, "ambient-"+name+"-") || strings.Contains(all, "profile-"+other+"-") || strings.Contains(all, "ambient-"+other+"-") {
					errCh <- fmt.Errorf("%s rendered other session state: %q", name, all)
					return
				}
			}
		}(pair.name, pair.other, pair.core)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
