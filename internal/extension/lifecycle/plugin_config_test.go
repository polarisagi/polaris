package lifecycle

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// reverseCipher 测试用可逆"加密"：只验证落库的是密文而非明文。
type reverseCipher struct{}

func (reverseCipher) Encrypt(s string) (string, error) { return "enc:" + reverse(s), nil }
func (reverseCipher) Decrypt(s string) (string, error) {
	return reverse(strings.TrimPrefix(s, "enc:")), nil
}

func reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

func installConfigPlugin(t *testing.T, extRepo *testExtRepo) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cfg")
	writeTestFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{
  "name": "cfg",
  "userConfig": {
    "endpoint": {"type": "string", "title": "Endpoint", "description": "d", "default": "https://api.example.com"},
    "token": {"type": "string", "title": "Token", "description": "d", "sensitive": true, "required": true},
    "retries": {"type": "number", "title": "Retries", "description": "d", "min": 0, "max": 5},
    "tone": {"type": "string", "title": "Tone", "description": "d", "options": ["warm", "formal"]}
  },
  "mcpServers": {"bot": {"command": "node", "env": {"TOKEN": "${user_config.token}", "CHAT": "${user_config.chat_id}"}}},
  "channels": [{"server": "bot", "userConfig": {"chat_id": {"type": "string", "title": "Chat", "description": "d", "required": true}}}]
}`)
	inst := NewPluginInstaller(extRepo, &recordingConnector{}, &recordingSkillRegistry{}).WithPolicyGate(allowAllGate{})
	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_cfg", LocalPath: root}); err != nil {
		t.Fatal(err)
	}
}

func raw(v string) json.RawMessage { return json.RawMessage(v) }

func TestPluginConfig_SaveValidateAndMask(t *testing.T) {
	extRepo := newTestExtRepo(t)
	installConfigPlugin(t, extRepo)
	svc := NewPluginConfigService(extRepo, reverseCipher{})
	ctx := context.Background()

	scopes, err := svc.GetSchema(ctx, "pl_cfg")
	if err != nil || len(scopes) != 2 || scopes[1].Scope != "bot" {
		t.Fatalf("schema: %+v %v", scopes, err)
	}
	for _, bad := range []ConfigUpdate{
		{Key: "retries", Value: raw(`9`)},
		{Key: "retries", Value: raw(`"x"`)},
		{Key: "tone", Value: raw(`"rude"`)},
		{Key: "nope", Value: raw(`"x"`)},
	} {
		if err := svc.SaveValues(ctx, "pl_cfg", []ConfigUpdate{bad}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
			t.Errorf("expected invalid input for %+v, got %v", bad, err)
		}
	}
	if err := svc.SaveValues(ctx, "pl_cfg", []ConfigUpdate{{Key: "token", Value: raw(`"s3cret"`)}, {Key: "retries", Value: raw(`3`)}}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := extRepo.db.QueryRow("SELECT value FROM plugin_user_config WHERE key='token'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "s3cret") {
		t.Fatalf("sensitive value stored in plaintext: %q", stored)
	}
	values, _ := svc.ListValues(ctx, "pl_cfg")
	for _, v := range values {
		if v.Key == "token" && (len(v.Value) != 0 || !v.IsSet) {
			t.Fatalf("sensitive value must be masked but reported set: %+v", v)
		}
	}
	// 只改非敏感项：已设置的 token 不得被清空。
	if err := svc.SaveValues(ctx, "pl_cfg", []ConfigUpdate{{Key: "retries", Value: raw(`4`)}}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := svc.ResolveStrings(ctx, "pl_cfg", "", true)
	if got["token"] != "s3cret" || got["retries"] != "4" || got["endpoint"] != "https://api.example.com" {
		t.Fatalf("resolved values: %v", got)
	}
	if nonSensitive, _, _ := svc.ResolveStrings(ctx, "pl_cfg", "", false); nonSensitive["token"] != "" {
		t.Fatalf("sensitive value leaked into non-sensitive resolution")
	}
}

func TestPluginConfig_SensitiveRequiresVault(t *testing.T) {
	extRepo := newTestExtRepo(t)
	installConfigPlugin(t, extRepo)
	svc := NewPluginConfigService(extRepo, nil)
	err := svc.SaveValues(context.Background(), "pl_cfg", []ConfigUpdate{{Key: "token", Value: raw(`"x"`)}})
	if err == nil {
		t.Fatal("sensitive value must be refused without a vault (never stored in plaintext)")
	}
}

func TestPluginVarsResolver_RequiredAndChannelScope(t *testing.T) {
	extRepo := newTestExtRepo(t)
	installConfigPlugin(t, extRepo)
	svc := NewPluginConfigService(extRepo, reverseCipher{})
	resolver := NewPluginVarsResolver(extRepo, t.TempDir(), svc)
	ctx := context.Background()

	_, err := resolver.ResolvePluginVars(ctx, "pl_cfg", "plugin_pl_cfg_bot")
	if !apperr.IsCode(err, apperr.CodeInvalidInput) || !strings.Contains(err.Error(), "token") || !strings.Contains(err.Error(), "chat_id") {
		t.Fatalf("missing required options must block start, got %v", err)
	}
	if err := svc.SaveValues(ctx, "pl_cfg", []ConfigUpdate{
		{Key: "token", Value: raw(`"tok"`)},
		{Scope: "bot", Key: "chat_id", Value: raw(`"42"`)},
	}); err != nil {
		t.Fatal(err)
	}
	vars, err := resolver.ResolvePluginVars(ctx, "pl_cfg", "plugin_pl_cfg_bot")
	if err != nil {
		t.Fatal(err)
	}
	if vars.UserConfig["token"] != "tok" || vars.UserConfig["chat_id"] != "42" || vars.PluginRoot == "" || vars.PluginData == "" {
		t.Fatalf("vars: %+v", vars)
	}
	env, _ := vars.Expand("${user_config.token}/${user_config.chat_id}")
	if env != "tok/42" {
		t.Fatalf("expand: %q", env)
	}
}
