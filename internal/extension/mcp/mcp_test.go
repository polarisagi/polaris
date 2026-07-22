package mcp

import (
	"context"
	"strings"
	"testing"
)

// ── MCPToolName ───────────────────────────────────────────────────────────────

func TestMCPToolName(t *testing.T) {
	name := MCPToolName("server-1", "get_weather")
	expected := "mcp__server-1__get_weather"
	if name != expected {
		t.Errorf("expected %q, got %q", expected, name)
	}
}

func TestMCPToolName_EmptyParts(t *testing.T) {
	name := MCPToolName("", "")
	if name != "mcp____" {
		t.Errorf("expected 'mcp____', got %q", name)
	}
}

func TestMCPToolName_SanitizesDots(t *testing.T) {
	name := MCPToolName("brave", "brave.web.search")
	expected := "mcp__brave__brave_web_search"
	if name != expected {
		t.Errorf("expected %q, got %q", expected, name)
	}
}

func TestValidateLLMNamePart_Valid(t *testing.T) {
	for _, s := range []string{"brave", "my-server", "server_1", "S3"} {
		if err := validateLLMNamePart(s); err != nil {
			t.Errorf("expected %q to be valid, got error: %v", s, err)
		}
	}
}

func TestValidateLLMNamePart_Invalid(t *testing.T) {
	for _, s := range []string{"", "my:server", "my server", "brave.search", "a/b"} {
		if err := validateLLMNamePart(s); err == nil {
			t.Errorf("expected %q to be invalid, but got no error", s)
		}
	}
}

// ── MCPManager (no-network paths) ────────────────────────────────────────────

func TestMCPManager_ListServers_Empty(t *testing.T) {
	m := NewMCPManager(nil, nil, &mockPolicyGate{})
	servers := m.ListServers()
	if len(servers) != 0 {
		t.Errorf("new manager should have 0 servers, got %d", len(servers))
	}
}

func TestMCPManager_ListToolSchemas_Empty(t *testing.T) {
	m := NewMCPManager(nil, nil, &mockPolicyGate{})
	schemas := m.ListToolSchemas()
	if len(schemas) != 0 {
		t.Errorf("new manager should have 0 tool schemas, got %d", len(schemas))
	}
}

func TestMCPManager_CallTool_ServerNotFound(t *testing.T) {
	m := NewMCPManager(nil, nil, &mockPolicyGate{})
	_, err := m.CallTool(context.Background(), "nonexistent-server", "some_tool", nil)
	if err == nil {
		t.Fatal("calling tool on non-existent server should return error")
	}
}

func TestMCPManager_CallTool_PolicyNil(t *testing.T) {
	m := NewMCPManager(nil, nil, &mockPolicyGate{})
	m.entries["fake-server"] = &mcpEntry{}
	_, err := m.CallTool(context.Background(), "fake-server", "some_tool", nil)
	if err == nil {
		t.Fatal("expected error when policy is nil")
	}
	if !strings.Contains(err.Error(), "envelope not initialized") {
		t.Fatalf("expected fail-closed policy error, got: %v", err)
	}
}

func TestMCPManager_Remove_NonExistent_NoOp(t *testing.T) {
	m := NewMCPManager(nil, nil, &mockPolicyGate{})
	// Remove on non-existent ID should not panic
	m.Remove("ghost-id")
	if len(m.ListServers()) != 0 {
		t.Error("remove on empty manager should leave 0 servers")
	}
}

// ── MCPServerConfig defaults ──────────────────────────────────────────────────

func TestMCPServerConfig_TrustedDefault(t *testing.T) {
	cfg := MCPServerConfig{}
	if cfg.Trusted {
		t.Error("new MCPServerConfig should default to Trusted=false (conservative)")
	}
}

// ── A2AAgentCard ──────────────────────────────────────────────────────────────

func TestA2AAgentCard_Fields(t *testing.T) {
	card := A2AAgentCard{
		Capabilities: map[string]bool{"streaming": true},
		Skills:       []A2ASkillRef{{ID: "s1", Tags: []string{"retrieval"}}},
	}
	if len(card.Skills) != 1 {
		t.Errorf("expected 1 skill, got %d", len(card.Skills))
	}
	if !card.Capabilities["streaming"] {
		t.Error("streaming capability should be true")
	}
}

// ── Transport constants ───────────────────────────────────────────────────────

func TestMCPTransport_Values(t *testing.T) {
	if MCPStdio != "stdio" {
		t.Errorf("expected stdio, got %q", MCPStdio)
	}
	if MCPStreamableHTTP != "streamable_http" {
		t.Errorf("expected streamable_http, got %q", MCPStreamableHTTP)
	}
	if MCPSSE != "sse" {
		t.Errorf("expected sse, got %q", MCPSSE)
	}
}

// ── parseMCPContent ───────────────────────────────────────────────────────────

func TestParseMCPContent_TextOnly(t *testing.T) {
	blocks := []mcpContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "text", Text: " world"},
	}
	text, imgs := parseMCPContent(blocks)
	if text != "hello world" {
		t.Errorf("expected 'hello world', got %q", text)
	}
	if len(imgs) != 0 {
		t.Errorf("expected 0 images, got %d", len(imgs))
	}
}

func TestParseMCPContent_ImageOnly(t *testing.T) {
	// 1×1 白色 JPEG（最小合法 JPEG，base64 标准编码）
	// 使用简单的 1 字节数据测试解码流程
	import64 := "AAEC" // 3 字节 base64 → [0,1,2]
	blocks := []mcpContentBlock{
		{Type: "image", Data: import64, MIMEType: "image/jpeg"},
	}
	text, imgs := parseMCPContent(blocks)
	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
	if len(imgs) != 1 {
		t.Fatalf("expected 1 image, got %d", len(imgs))
	}
	if imgs[0].MediaType != "image/jpeg" {
		t.Errorf("expected image/jpeg, got %q", imgs[0].MediaType)
	}
	if len(imgs[0].Data) != 3 {
		t.Errorf("expected 3 bytes decoded, got %d", len(imgs[0].Data))
	}
}

func TestParseMCPContent_Mixed(t *testing.T) {
	blocks := []mcpContentBlock{
		{Type: "text", Text: "result:"},
		{Type: "image", Data: "AAEC", MIMEType: "image/png"},
		{Type: "text", Text: " done"},
	}
	text, imgs := parseMCPContent(blocks)
	if text != "result: done" {
		t.Errorf("expected 'result: done', got %q", text)
	}
	if len(imgs) != 1 {
		t.Errorf("expected 1 image, got %d", len(imgs))
	}
}

func TestParseMCPContent_ImageMissingData_Skipped(t *testing.T) {
	blocks := []mcpContentBlock{
		{Type: "image", Data: "", MIMEType: "image/jpeg"}, // 空 data
		{Type: "image", Data: "AAEC", MIMEType: ""},       // 空 mimeType
	}
	_, imgs := parseMCPContent(blocks)
	if len(imgs) != 0 {
		t.Errorf("blocks with missing data/mimeType should be skipped, got %d images", len(imgs))
	}
}

func TestParseMCPContent_UnknownTypeSkipped(t *testing.T) {
	blocks := []mcpContentBlock{
		{Type: "embedded_resource", Text: "whatever"},
		{Type: "text", Text: "ok"},
	}
	text, imgs := parseMCPContent(blocks)
	if text != "ok" {
		t.Errorf("expected 'ok', got %q", text)
	}
	if len(imgs) != 0 {
		t.Errorf("expected 0 images, got %d", len(imgs))
	}
}

func TestDecodeBase64_Standard(t *testing.T) {
	// "hello" → aGVsbG8=（标准编码，含 padding）
	raw, err := decodeBase64("aGVsbG8=")
	if err != nil {
		t.Fatalf("standard base64 decode failed: %v", err)
	}
	if string(raw) != "hello" {
		t.Errorf("expected 'hello', got %q", string(raw))
	}
}

func TestDecodeBase64_URLSafe(t *testing.T) {
	// "hello world" → aGVsbG8gd29ybGQ（URL-safe, no padding）
	raw, err := decodeBase64("aGVsbG8gd29ybGQ")
	if err != nil {
		t.Fatalf("URL-safe base64 decode failed: %v", err)
	}
	if string(raw) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(raw))
	}
}
