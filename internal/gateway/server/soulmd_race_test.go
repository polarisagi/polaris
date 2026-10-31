package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/server/chat"
	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type soulRaceAgent struct {
	protocol.AgentController
	memory protocol.MemoryFacade
}

func (a soulRaceAgent) Memory() protocol.MemoryFacade { return a.memory }

type soulRaceMemory struct {
	protocol.MemoryFacade
	core protocol.ImmutableCore
}

func (m soulRaceMemory) ImmutableCore() protocol.ImmutableCore { return m.core }
func (soulRaceMemory) GetUserProfile(context.Context, string) (*types.UserProfile, error) {
	return nil, nil
}

type soulRaceRegistry struct{}

func (soulRaceRegistry) PickProvider(string) protocol.Provider                      { return nil }
func (soulRaceRegistry) PickProviderName(string) string                             { return "" }
func (soulRaceRegistry) PickProviderByRecordID(string) protocol.Provider            { return nil }
func (soulRaceRegistry) UnregisterAll()                                             {}
func (soulRaceRegistry) RegisterWithRole(string, string, string, protocol.Provider) {}

func TestSoulMDSharedLockProtectsPromptReadAndAdminWrites(t *testing.T) {
	previousGate := probe.GlobalFeatureGate()
	probe.SetGlobalFeatureGate(probe.NewFeatureGate(probe.NewHardwareProbe(4<<30, 4<<30), nil))
	defer probe.SetGlobalFeatureGate(previousGate)

	dataDir := t.TempDir()
	rwDB, err := sql.Open("sqlite3", filepath.Join(dataDir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	rwDB.SetMaxOpenConns(1)
	defer rwDB.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(ctx, "127.0.0.1:0", dataDir, nil, nil, nil, rwDB, nil, soulRaceRegistry{}, nil, nil, config.CompressorConfig{}, config.AgentConfig{}, config.A2AConfig{}, nil, nil)
	promptMgr := prompt.NewManager(t.TempDir(), nil)
	server.SetPromptManager(promptMgr)
	chatHandler := server.chatHandler
	adminHandler := server.sysadminHandler
	if chatHandler.PromptService.SoulMDMu != adminHandler.SoulMDMu || chatHandler.PromptService.SoulMDContent != adminHandler.SoulMDContent {
		t.Fatal("chat prompt reader and sysadmin writer must share the same source and lock")
	}
	if chatHandler.PromptService.SoulMDMu == nil || chatHandler.PromptService.SoulMDContent != &server.soulMDContent || adminHandler.PromptMgr != promptMgr {
		t.Fatal("NewServer→SetPromptManager must wire the shared identity source, lock, and prompt manager")
	}

	start := make(chan struct{})
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range 500 {
			core := store.NewImmutableCore()
			memory := soulRaceMemory{core: core}
			agent := soulRaceAgent{memory: memory}
			chatHandler.PromptService.InjectSystemPrompt(context.Background(), agent, nil, "query")
			got := core.Fields().SoulMDContent
			if got != "identity-initial" && got != "identity-alpha" && got != "identity-beta" && got != "" {
				errCh <- fmt.Errorf("unexpected identity source value: %q", got)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := range 40 {
			value := "identity-alpha"
			if i%2 == 1 {
				value = "identity-beta"
			}
			body, _ := json.Marshal(map[string]string{"value": value})
			request := httptest.NewRequest(http.MethodPut, "/v1/config/prompts/identity", bytes.NewReader(body))
			request.SetPathValue("name", "identity")
			response := httptest.NewRecorder()
			adminHandler.HandleSetPrompt(response, request)
			if response.Code != http.StatusOK {
				errCh <- fmt.Errorf("identity PUT failed: %s", response.Body.String())
				return
			}
			if i%4 == 3 {
				request = httptest.NewRequest(http.MethodDelete, "/v1/config/prompts/identity", nil)
				request.SetPathValue("name", "identity")
				response = httptest.NewRecorder()
				adminHandler.HandleResetPrompt(response, request)
				if response.Code != http.StatusOK {
					errCh <- fmt.Errorf("identity DELETE failed: %s", response.Body.String())
					return
				}
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestInjectSystemPromptNilSoulMDRemainsSafe(t *testing.T) {
	core := store.NewImmutableCore()
	core.SoulMDContent = "existing identity"
	memory := soulRaceMemory{core: core}
	service := chat.NewPromptAssemblyService(prompt.NewManager(t.TempDir(), nil), nil, "", soulRaceRegistry{}, "", nil, nil, nil, 0, 0, nil, "")
	service.InjectSystemPrompt(context.Background(), soulRaceAgent{memory: memory}, nil, "query")
	if core.SoulMDContent != "existing identity" {
		t.Fatalf("nil SoulMDContent should preserve the prior nil-safe behavior, got %q", core.SoulMDContent)
	}
}
