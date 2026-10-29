package secret_get_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/secret_get"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockSecretReader struct {
	secrets map[string]string
	taint   types.TaintLevel
}

func (m *mockSecretReader) GetSecret(ctx context.Context, name string) (string, types.TaintLevel, error) {
	if val, ok := m.secrets[name]; ok {
		return val, m.taint, nil
	}
	return "", types.TaintNone, apperr.New(apperr.CodeNotFound, "secret not found: "+name)
}

func TestSecretGet_PerSessionAndTurnCounter(t *testing.T) {
	reader := &mockSecretReader{
		secrets: map[string]string{
			"api_key": "sk-secret-12345",
			"db_pass": "pass-67890",
		},
		taint: types.TaintLow,
	}

	fn := secret_get.MakeSecretGetFn(reader, 2)
	inputJSON, _ := json.Marshal(map[string]string{"name": "api_key"})

	ctxSess1Turn1 := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "sess-1")
	ctxSess1Turn1 = protocol.WithTurnID(ctxSess1Turn1, "turn-1")

	// 第 1 回合：第 1 次读取成功
	if _, err := fn(ctxSess1Turn1, inputJSON); err != nil {
		t.Fatalf("turn 1 call 1 failed: %v", err)
	}
	// 第 1 回合：第 2 次读取成功
	if _, err := fn(ctxSess1Turn1, inputJSON); err != nil {
		t.Fatalf("turn 1 call 2 failed: %v", err)
	}
	// 第 1 回合：第 3 次读取超限，必须返回 CodeForbidden
	_, err := fn(ctxSess1Turn1, inputJSON)
	if err == nil {
		t.Fatal("turn 1 call 3 expected forbidden error, got nil")
	}
	if apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("turn 1 call 3 expected CodeForbidden, got %v", apperr.CodeOf(err))
	}

	// 第 2 回合：新回合计数重置，应当可继续成功读取 2 次
	ctxSess1Turn2 := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "sess-1")
	ctxSess1Turn2 = protocol.WithTurnID(ctxSess1Turn2, "turn-2")

	if _, err := fn(ctxSess1Turn2, inputJSON); err != nil {
		t.Fatalf("turn 2 call 1 failed (counter should have reset for new turn): %v", err)
	}
	if _, err := fn(ctxSess1Turn2, inputJSON); err != nil {
		t.Fatalf("turn 2 call 2 failed: %v", err)
	}
	if _, err := fn(ctxSess1Turn2, inputJSON); err == nil {
		t.Fatal("turn 2 call 3 expected forbidden error, got nil")
	}

	// 跨会话隔离：sess-2 在同名 turn-1 下独立计数
	ctxSess2Turn1 := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "sess-2")
	ctxSess2Turn1 = protocol.WithTurnID(ctxSess2Turn1, "turn-1")

	if _, err := fn(ctxSess2Turn1, inputJSON); err != nil {
		t.Fatalf("session 2 turn 1 call 1 failed (should be isolated from session 1): %v", err)
	}
}

func TestSecretGet_ExplicitTurnCounterInContext(t *testing.T) {
	reader := &mockSecretReader{
		secrets: map[string]string{"k": "v"},
		taint:   types.TaintLow,
	}
	fn := secret_get.MakeSecretGetFn(reader, 1)
	inputJSON, _ := json.Marshal(map[string]string{"name": "k"})

	counter := secret_get.NewTurnCounter()
	ctx := secret_get.WithTurnCounter(context.Background(), counter)

	if _, err := fn(ctx, inputJSON); err != nil {
		t.Fatalf("call 1 failed: %v", err)
	}
	if _, err := fn(ctx, inputJSON); err == nil {
		t.Fatal("call 2 expected forbidden error, got nil")
	}

	// 手动重置计数器
	counter.Reset()
	if _, err := fn(ctx, inputJSON); err != nil {
		t.Fatalf("call after counter.Reset() failed: %v", err)
	}
}

func TestSecretGet_TaintHigh_PropagatedThroughSandbox(t *testing.T) {
	reader := &mockSecretReader{
		secrets: map[string]string{"api_key": "val"},
		taint:   types.TaintLow,
	}
	fn := secret_get.MakeSecretGetFn(reader, 5)
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	sbx.RegisterWithTaint("secret_get", fn, types.TaintHigh)

	res, err := sbx.Run(context.Background(), sandbox.SandboxSpec{
		ToolName: "secret_get",
		Input:    []byte(`{"name":"api_key"}`),
	})
	if err != nil {
		t.Fatalf("sbx.Run failed: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	if res.TaintLevel != types.TaintHigh {
		t.Fatalf("expected TaintHigh, got %v", res.TaintLevel)
	}
}
