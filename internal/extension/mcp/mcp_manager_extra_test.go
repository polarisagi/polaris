package mcp

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestMCPManager_AddRemoveUpdate(t *testing.T) {
	mgr := NewMCPManager(nil, http.DefaultClient, nil)

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite3 in-memory db: %v", err)
	}
	defer db.Close()

	// Create table for LoadFromDB and Update
	_, err = db.Exec(`CREATE TABLE mcp_servers (
		id TEXT PRIMARY KEY,
		name TEXT,
		transport TEXT,
		command TEXT,
		args TEXT,
		env TEXT,
		url TEXT,
		enabled INTEGER,
		timeout INTEGER,
		trust_tier INTEGER,
		work_dir TEXT,
		updated_at TEXT
	)`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	// Insert dummy data
	_, err = db.Exec(`INSERT INTO mcp_servers (id, name, transport, command, args, env, url, enabled, timeout, trust_tier, work_dir)
		VALUES ('test-1', 'fake-server', 'streamable_http', '', '[]', '{}', 'http://localhost:9999', 1, 30, 2, '')`)
	if err != nil {
		t.Fatalf("failed to insert data: %v", err)
	}

	// test LoadFromDB
	mgr.LoadFromDB(context.Background(), repo.NewSQLiteExtensionRepository(db), "/tmp")

	// Wait a little for goroutines to execute Add
	time.Sleep(100 * time.Millisecond)

	err = mgr.Add(context.Background(), "server-id-1", "fake-server", MCPClientConfig{
		Transport: MCPStreamableHTTP,
		URL:       "http://localhost:9999",
	})
	// Connect will fail because localhost:9999 is not serving
	if err == nil {
		t.Errorf("expected error connecting to fake server")
	}

	// test Update
	err = mgr.Update(context.Background(), repo.NewSQLiteExtensionRepository(db), "test-1", MCPUpdateConfig{Name: "updated-server"}, "")
	if err != nil {
		t.Errorf("unexpected error updating: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	mgr.Remove("test-1")
	mgr.Remove("server-id-1")
	if len(mgr.ListServers()) != 0 {
		t.Errorf("expected 0 servers after removal, got %d", len(mgr.ListServers()))
	}

	// test DynamicConnect
	err = mgr.DynamicConnect(context.Background(), DynamicConnectRequest{
		ServerName: "dyn-server",
		Transport:  "streamable_http",
		URL:        "http://localhost:9999",
	})
	if err == nil {
		t.Errorf("expected error connecting dynamically")
	}

	// Add some dummy entries to test CallTool, registerTools and makeMCPToolFn
	mgr.sandbox = sandbox.NewInProcessSandbox() // need non-nil sandbox for registerTools

	mgr.mu.Lock()
	testClient := NewMCPClient(MCPClientConfig{Trusted: true}, nil)

	// directly test registerTools
	validTools := mgr.registerTools("fake-1", testClient, []MCPTool{
		{Name: "tool1", Description: "desc1", InputSchema: []byte(`{"type":"object"}`)},
	})
	if len(validTools) != 1 {
		t.Errorf("expected 1 valid tool")
	}

	mgr.entries["fake-1"] = &mcpEntry{
		name:   "fake-1",
		tools:  validTools,
		client: testClient,
	}
	mgr.mu.Unlock()

	schemas := mgr.ListToolSchemas()
	if len(schemas) != 1 {
		t.Errorf("expected 1 schema, got %d", len(schemas))
	}

	_, err = mgr.CallTool(context.Background(), "fake-1", "tool1", nil)
	if err == nil {
		t.Errorf("expected error calling tool since client is dummy")
	}

	// test CallTool on non-existent server
	_, err = mgr.CallTool(context.Background(), "non-existent", "tool1", nil)
	if err == nil {
		t.Errorf("expected error calling tool on non-existent server")
	}
}
