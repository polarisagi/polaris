package connector

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeMCPClient struct {
	resources   []mcp.MCPResource
	listErr     error
	contents    map[string][]mcp.MCPResourceContent
	readErr     error
	lastReadURI string
}

func (f *fakeMCPClient) ResourcesList(ctx context.Context) ([]mcp.MCPResource, error) {
	return f.resources, f.listErr
}

func (f *fakeMCPClient) ResourcesRead(ctx context.Context, uri string) ([]mcp.MCPResourceContent, error) {
	f.lastReadURI = uri
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.contents[uri], nil
}

func TestMCPKnowledgeConnector_List(t *testing.T) {
	client := &fakeMCPClient{
		resources: []mcp.MCPResource{
			{URI: "file:///a.md", Name: "A", Description: "doc a", MIMEType: "text/markdown"},
			{URI: "file:///b.bin", Name: "B", MIMEType: "application/octet-stream"},
		},
	}
	conn := NewMCPKnowledgeConnector("inst_1", "test-mcp", client)

	refs, err := conn.List(context.Background())
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].URI != "file:///a.md" || refs[0].SourceType != "markdown" {
		t.Fatalf("unexpected ref[0]: %+v", refs[0])
	}
	if refs[1].SourceType != "" {
		t.Fatalf("expected unknown mime type to fall back to empty SourceType, got %q", refs[1].SourceType)
	}
}

func TestMCPKnowledgeConnector_Fetch(t *testing.T) {
	client := &fakeMCPClient{
		contents: map[string][]mcp.MCPResourceContent{
			"file:///a.md": {{URI: "file:///a.md", MIMEType: "text/markdown", Text: "# Hello"}},
		},
	}
	conn := NewMCPKnowledgeConnector("inst_1", "test-mcp", client)

	doc, err := conn.Fetch(context.Background(), &types.DocumentRef{URI: "file:///a.md", Title: "A"})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if string(doc.Content) != "# Hello" {
		t.Fatalf("unexpected content: %q", doc.Content)
	}
	if client.lastReadURI != "file:///a.md" {
		t.Fatalf("expected ResourcesRead called with correct URI, got %q", client.lastReadURI)
	}
}

func TestMCPKnowledgeConnector_Fetch_NoContent(t *testing.T) {
	client := &fakeMCPClient{contents: map[string][]mcp.MCPResourceContent{}}
	conn := NewMCPKnowledgeConnector("inst_1", "test-mcp", client)

	if _, err := conn.Fetch(context.Background(), &types.DocumentRef{URI: "file:///missing.md"}); err == nil {
		t.Fatal("expected error when resources/read returns no content")
	}
}
