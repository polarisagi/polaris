package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/pkg/types"
)

func newTestStatefulCodeActAdapter(t *testing.T, stateDir string) *testCodeActAdapter {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping stateful code_act test")
	}

	container := sandbox.NewContainerSandbox("", runtime.GOOS, 1, toolsb.NewWrapBashCmdRunner(), config.DefaultThresholds().M7Tool)
	router := sandbox.NewSandboxRouter(nil, container, nil, runtime.GOOS, 1)
	envelope := sandbox.NewExecEnvelope(testPolicyGate{}, router, 1, runtime.GOOS, nil)
	bridgeExec := newCodeActTestBridgeExecutor()

	caEngine := codeact.NewCodeAct(
		envelope,
		nil,
		codeact.WithGovernanceAgent(testGovAgent{}),
		codeact.WithASTChecker(&codeact.DefaultASTChecker{}),
		codeact.WithTokenManager(action.GetTokenManager()),
		codeact.WithToolExecutor(bridgeExec),
		codeact.WithPolicy(policy.ProfilePolicy(policy.ProfileStandard)),
		codeact.WithStateDir(stateDir),
	)

	return &testCodeActAdapter{inner: caEngine}
}

// TestAgenticCodeAct_SessionPersistence_SameSessionSharesState 验证 R4-03：
// 两次 code_act 调用使用同一个 session_id，第二次能读到第一次定义的变量。
func TestAgenticCodeAct_SessionPersistence_SameSessionSharesState(t *testing.T) {
	stateDir := t.TempDir()
	adapter := newTestStatefulCodeActAdapter(t, stateDir)

	agent := NewAgentWithDefaults("test-agent-sess-persistence")
	agent.SetCodeAct(adapter)
	agent.sCtx.SessionID = "agent-session-1"

	ctx := context.Background()

	// 第一次调用：定义变量
	call1Args, err := json.Marshal(map[string]any{
		"language":   "python",
		"code":       "counter = 42\nprint('SET_COUNTER_' + str(counter))\n",
		"session_id": "repl_alpha",
	})
	require.NoError(t, err)

	res1, err := agent.runCodeAct(ctx, "python", call1Args, types.TaintLow)
	require.NoError(t, err)
	require.True(t, res1.Success, "首次定义变量应执行成功")
	require.Contains(t, string(res1.Output), "SET_COUNTER_42")

	// 第二次调用：读取相同 session_id 中的变量
	call2Args, err := json.Marshal(map[string]any{
		"language":   "python",
		"code":       "print('READ_COUNTER_' + str(counter))\n",
		"session_id": "repl_alpha",
	})
	require.NoError(t, err)

	res2, err := agent.runCodeAct(ctx, "python", call2Args, types.TaintLow)
	require.NoError(t, err)
	require.True(t, res2.Success, "复用相同 session_id 读取变量应成功")
	require.Contains(t, string(res2.Output), "READ_COUNTER_42")
}

// TestAgenticCodeAct_SessionPersistence_DifferentSessionIsolated 验证 R4-03：
// 不同 session_id 之间相互隔离。
func TestAgenticCodeAct_SessionPersistence_DifferentSessionIsolated(t *testing.T) {
	stateDir := t.TempDir()
	adapter := newTestStatefulCodeActAdapter(t, stateDir)

	agent := NewAgentWithDefaults("test-agent-sess-iso")
	agent.SetCodeAct(adapter)
	agent.sCtx.SessionID = "agent-session-iso"

	ctx := context.Background()

	// 在 session_id: "repl_1" 中定义变量
	call1Args, err := json.Marshal(map[string]any{
		"language":   "python",
		"code":       "secret_num = 888\nprint('SET_OK')\n",
		"session_id": "repl_1",
	})
	require.NoError(t, err)

	res1, err := agent.runCodeAct(ctx, "python", call1Args, types.TaintLow)
	require.NoError(t, err)
	require.True(t, res1.Success)
	require.Contains(t, string(res1.Output), "SET_OK")

	// 在 session_id: "repl_2" 中尝试读取该变量，应隔离（报错或未定义）
	call2Args, err := json.Marshal(map[string]any{
		"language":   "python",
		"code":       "print('SECRET_' + str(secret_num))\n",
		"session_id": "repl_2",
	})
	require.NoError(t, err)

	res2, err := agent.runCodeAct(ctx, "python", call2Args, types.TaintLow)
	require.NoError(t, err)
	require.False(t, res2.Success, "不同 session_id 隔离，读取未定义变量应失败")
	require.Contains(t, string(res2.Output), "NameError: name 'secret_num' is not defined")
}

// TestAgenticCodeAct_SessionPersistence_CrossAgentIsolated 验证 R4-03：
// 即使两个 Agent 传入相同的 session_id（如 "repl_common"），在不同 Agent 会话间也必须隔离。
func TestAgenticCodeAct_SessionPersistence_CrossAgentIsolated(t *testing.T) {
	stateDir := t.TempDir()
	adapter := newTestStatefulCodeActAdapter(t, stateDir)

	agentA := NewAgentWithDefaults("agent-A")
	agentA.SetCodeAct(adapter)
	agentA.sCtx.SessionID = "session-A"

	agentB := NewAgentWithDefaults("agent-B")
	agentB.SetCodeAct(adapter)
	agentB.sCtx.SessionID = "session-B"

	ctx := context.Background()

	// Agent A 在 repl_common 定义变量
	callA, err := json.Marshal(map[string]any{
		"language":   "python",
		"code":       "shared_var = 12345\nprint('AGENT_A_SET')\n",
		"session_id": "repl_common",
	})
	require.NoError(t, err)

	resA, err := agentA.runCodeAct(ctx, "python", callA, types.TaintLow)
	require.NoError(t, err)
	require.True(t, resA.Success)
	require.Contains(t, string(resA.Output), "AGENT_A_SET")

	// Agent B 使用相同的 repl_common 读取，应隔离不可见
	callB, err := json.Marshal(map[string]any{
		"language":   "python",
		"code":       "print('VAL_' + str(shared_var))\n",
		"session_id": "repl_common",
	})
	require.NoError(t, err)

	resB, err := agentB.runCodeAct(ctx, "python", callB, types.TaintLow)
	require.NoError(t, err)
	require.False(t, resB.Success, "不同 Agent 会话间相同 session_id 必须隔离")
	require.True(t, strings.Contains(string(resB.Output), "NameError"), "应出现变量未定义的 NameError")
}

// TestAgenticCodeAct_StatefulSession_Compatibility 验证 R4-03：
// 既有 stateful_session=true 兼容解析生效。
func TestAgenticCodeAct_StatefulSession_Compatibility(t *testing.T) {
	stateDir := t.TempDir()
	adapter := newTestStatefulCodeActAdapter(t, stateDir)

	agent := NewAgentWithDefaults("test-compat-agent")
	agent.SetCodeAct(adapter)
	agent.sCtx.SessionID = "compat-session"

	ctx := context.Background()

	// 第一次调用：stateful_session = true
	call1Args, err := json.Marshal(map[string]any{
		"language":         "python",
		"code":             "compat_val = 77\nprint('COMPAT_SET')",
		"stateful_session": true,
	})
	require.NoError(t, err)

	res1, err := agent.runCodeAct(ctx, "python", call1Args, types.TaintLow)
	require.NoError(t, err)
	require.True(t, res1.Success)

	// 第二次调用：读取 compat_val
	call2Args, err := json.Marshal(map[string]any{
		"language":         "python",
		"code":             "print('COMPAT_READ_' + str(compat_val))\n",
		"stateful_session": true,
	})
	require.NoError(t, err)

	res2, err := agent.runCodeAct(ctx, "python", call2Args, types.TaintLow)
	require.NoError(t, err)
	require.True(t, res2.Success)
	require.Contains(t, string(res2.Output), "COMPAT_READ_77")
}
