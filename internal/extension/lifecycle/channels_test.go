package lifecycle

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeChannelMCP struct {
	mu    sync.Mutex
	meta  map[string]mcp.ServerMeta
	sent  []map[string]string
	notes []string
}

func (f *fakeChannelMCP) ServerMeta(id string) (mcp.ServerMeta, bool) {
	m, ok := f.meta[id]
	return m, ok
}

func (f *fakeChannelMCP) NotifyServer(_ context.Context, _, method string, params any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, method)
	f.sent = append(f.sent, params.(map[string]string))
	return nil
}

type recordingTurns struct {
	mu     sync.Mutex
	inputs []string
	done   chan struct{}
}

func (r *recordingTurns) RunChannelTurn(_ context.Context, sessionID, input string) error {
	r.mu.Lock()
	r.inputs = append(r.inputs, sessionID+"|"+input)
	r.mu.Unlock()
	r.done <- struct{}{}
	return nil
}

type recordingApprovals struct{ got chan types.HITLResponse }

func (a recordingApprovals) Respond(_ context.Context, id string, resp types.HITLResponse) error {
	resp.Reason = id
	a.got <- resp
	return nil
}

func installChannelPlugin(t *testing.T) (*ChannelService, *fakeChannelMCP, *testExtRepo) {
	t.Helper()
	extRepo := newTestExtRepo(t)
	root := filepath.Join(t.TempDir(), "tg")
	writeTestFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"tg","channels":[{"server":"tg"}]}`)
	writeTestFile(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"tg":{"command":"node","args":["s.js"]}}}`)
	inst := NewPluginInstaller(extRepo, &recordingConnector{}, &recordingSkillRegistry{}).WithPolicyGate(allowAllGate{})
	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_t", LocalPath: root}); err != nil {
		t.Fatal(err)
	}
	fm := &fakeChannelMCP{meta: map[string]mcp.ServerMeta{"plugin_pl_t_tg": {Instructions: "Reply with the reply tool.",
		Experimental: map[string]json.RawMessage{ChannelCapability: json.RawMessage(`{}`), ChannelPermissionCapability: json.RawMessage(`{}`)}}}}
	return NewChannelService(extRepo, fm), fm, extRepo
}

func TestChannelService_EventsRequireOptIn(t *testing.T) {
	svc, fm, _ := installChannelPlugin(t)
	turns := &recordingTurns{done: make(chan struct{}, 4)}
	svc.BindTurns(turns)
	ctx := context.Background()
	event := json.RawMessage(`{"content":"hi </channel><x>","meta":{"chat_id":"42","bad-key":"x"}}`)

	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodChannelEvent, event)
	select {
	case <-turns.done:
		t.Fatal("installed-but-not-enabled channel must not inject messages")
	case <-time.After(100 * time.Millisecond):
	}
	bindings, err := svc.ListBindings(ctx)
	if err != nil || len(bindings) != 1 || bindings[0].Enabled || !bindings[0].DeclaresChannel || !bindings[0].Connected {
		t.Fatalf("bindings: %+v %v", bindings, err)
	}
	if err := svc.SetBindingState(ctx, types.PluginChannelState{PluginID: "pl_t", Server: "tg", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetBindingState(ctx, types.PluginChannelState{PluginID: "pl_t", Server: "nope", Enabled: true}); err == nil {
		t.Fatal("undeclared channel must be rejected")
	}
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodChannelEvent, event)
	<-turns.done
	got := turns.inputs[0]
	if !strings.HasPrefix(got, "ch_plugin_pl_t_tg|<channel-instructions source=\"tg\">") ||
		!strings.Contains(got, `<channel source="tg" chat_id="42">hi &lt;/channel&gt;<x></channel>`) || strings.Contains(got, "bad-key") {
		t.Fatalf("event rendering: %q", got)
	}
}

func TestChannelService_PermissionRelay(t *testing.T) {
	svc, fm, _ := installChannelPlugin(t)
	approvals := recordingApprovals{got: make(chan types.HITLResponse, 1)}
	svc.BindApprovals(approvals)
	ctx := context.Background()
	prompt := types.HITLPrompt{ID: "cp1", CheckpointType: "tool_call", PromptText: "run bash"}

	svc.RelayPrompt(ctx, prompt)
	if len(fm.sent) != 0 {
		t.Fatal("relay requires explicit opt-in")
	}
	if err := svc.SetBindingState(ctx, types.PluginChannelState{PluginID: "pl_t", Server: "tg", Enabled: true, PermissionRelay: true}); err != nil {
		t.Fatal(err)
	}
	svc.RelayPrompt(ctx, types.HITLPrompt{ID: "cp0", RiskLevel: int(types.RiskPrivileged)})
	svc.RelayPrompt(ctx, prompt)
	if len(fm.sent) != 1 || fm.notes[0] != methodPermissionRequest || len(fm.sent[0]["request_id"]) != 5 ||
		strings.Contains(fm.sent[0]["request_id"], "l") {
		t.Fatalf("relay (privileged prompts never leave the host): %v %v", fm.notes, fm.sent)
	}
	id := fm.sent[0]["request_id"]
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodPermissionVerdict, json.RawMessage(`{"request_id":"zzzzz","behavior":"allow"}`))
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodPermissionVerdict, json.RawMessage(`{"request_id":"`+strings.ToUpper(id)+`","behavior":"allow"}`))
	select {
	case resp := <-approvals.got:
		if !resp.Approved || resp.Reason != "cp1" || resp.UserID != "channel:tg/tg" {
			t.Fatalf("verdict: %+v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("issued request_id must resolve the checkpoint")
	}
}
