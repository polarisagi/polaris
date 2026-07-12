package codeact

import (
	"os"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// mockScriptStagingBackend 记录调用参数，供断言 stageScript 是否走了 VFS 路径。
type mockScriptStagingBackend struct {
	namespace   string
	filename    string
	data        []byte
	cleanupHits int
	stagedPath  string
	err         error
}

func (m *mockScriptStagingBackend) StageEphemeralFile(namespace, filename string, data []byte) (string, func(), error) {
	if m.err != nil {
		return "", nil, m.err
	}
	m.namespace = namespace
	m.filename = filename
	m.data = data
	m.stagedPath = "/fake/vfs/root/" + namespace + "/" + filename
	return m.stagedPath, func() { m.cleanupHits++ }, nil
}

// TestStageScript_UsesBackendWhenInjected 验证批次4 XR-11 复核修复：注入
// ScriptStagingBackend 后，stageScript 必须走 VFS 路径而非 os.CreateTemp 落盘
// 系统 /tmp，且 cleanup() 正确转发到后端的清理闭包。
func TestStageScript_UsesBackendWhenInjected(t *testing.T) {
	backend := &mockScriptStagingBackend{}
	ca := &CodeAct{stagingBackend: backend}

	req := protocol.CodeActRequest{Language: "python", SessionID: "sess-42"}
	path, cleanup, err := ca.stageScript(req, "print('hi')")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != backend.stagedPath {
		t.Fatalf("expected staged path from backend, got %q", path)
	}
	if backend.namespace != "sess-42" {
		t.Errorf("expected namespace=sess-42, got %q", backend.namespace)
	}
	if !strings.HasSuffix(backend.filename, ".py") {
		t.Errorf("expected .py extension, got %q", backend.filename)
	}
	if string(backend.data) != "print('hi')" {
		t.Errorf("unexpected staged content: %q", backend.data)
	}
	cleanup()
	if backend.cleanupHits != 1 {
		t.Errorf("expected cleanup to be forwarded exactly once, got %d", backend.cleanupHits)
	}
}

// TestStageScript_NamespaceFallback 验证 SessionID 为空时退化为 AgentID，
// 两者皆空时退化为固定值，不会向后端传空字符串（会被其内部净化逻辑吞掉分组语义）。
func TestStageScript_NamespaceFallback(t *testing.T) {
	backend := &mockScriptStagingBackend{}
	ca := &CodeAct{stagingBackend: backend}

	_, _, err := ca.stageScript(protocol.CodeActRequest{Language: "bash", AgentID: "agent-7"}, "echo hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend.namespace != "agent-7" {
		t.Errorf("expected namespace fallback to AgentID, got %q", backend.namespace)
	}

	_, _, err = ca.stageScript(protocol.CodeActRequest{Language: "bash"}, "echo hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend.namespace != "anon" {
		t.Errorf("expected namespace fallback to \"anon\", got %q", backend.namespace)
	}
}

// TestStageScript_FallsBackToTempFileWhenBackendNil 验证未注入 stagingBackend
// 时（未接入 VFS 的最小 Tier-0 部署 / 单测场景）行为与修复前完全一致：
// 落盘到系统临时目录，cleanup() 删除该文件。
func TestStageScript_FallsBackToTempFileWhenBackendNil(t *testing.T) {
	ca := &CodeAct{} // stagingBackend == nil

	req := protocol.CodeActRequest{Language: "python", SessionID: "sess-1"}
	path, cleanup, err := ca.stageScript(req, "print('fallback')")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("expected temp file to exist: %v", statErr)
	}
	cleanup()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected temp file removed after cleanup(), stat err=%v", statErr)
	}
}
