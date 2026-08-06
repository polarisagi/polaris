package optimizer

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestPromptVersionStore(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open sqlite memory db: %v", err)
	}
	defer db.Close()

	// Setup table
	_, err = db.Exec(`
		CREATE TABLE prompt_versions (
			id TEXT PRIMARY KEY,
			version INTEGER,
			task_type TEXT,
			prompt_text TEXT,
			score REAL,
			cost REAL,
			source TEXT,
			parent_version TEXT,
			is_active INTEGER,
			created_at INTEGER
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}

	activatedCalled := false
	store := NewPromptVersionStore(db)
	store.OnActivate = func(taskType, promptText string) {
		activatedCalled = true
	}

	ctx := context.Background()

	// Test GetActive empty
	active, err := store.GetActive(ctx, "task1")
	if err != nil {
		t.Errorf("GetActive failed: %v", err)
	}
	if active != nil {
		t.Errorf("Expected nil active version, got %v", active)
	}

	// Test Save empty ID
	err = store.Save(ctx, &PromptVersion{Prompt: "test"})
	if err == nil {
		t.Errorf("Expected error for empty ID")
	}

	// Test Save
	v1 := &PromptVersion{
		ID:        "v1",
		Version:   1,
		TaskType:  "task1",
		Prompt:    "prompt 1",
		Score:     0.5,
		Cost:      0.1,
		Source:    "test",
		ParentVer: 0,
	}
	err = store.Save(ctx, v1)
	if err != nil {
		t.Errorf("Save failed: %v", err)
	}

	// Test Save duplicate ID (should be ignored)
	err = store.Save(ctx, v1)
	if err != nil {
		t.Errorf("Save duplicate failed: %v", err)
	}

	// Test Activate low score
	err = store.Activate(ctx, "task1", "v1", 0.8)
	if err == nil {
		t.Errorf("Expected error for low score activation")
	}

	// Test UpdateScore
	err = store.UpdateScore(ctx, "v1", 0.9)
	if err != nil {
		t.Errorf("UpdateScore failed: %v", err)
	}

	// Test UpdateScore not found
	err = store.UpdateScore(ctx, "v2", 0.9)
	if err == nil {
		t.Errorf("Expected error for updating not found version")
	}

	// Test Activate not found
	err = store.Activate(ctx, "task1", "v2", 0.8)
	if err == nil {
		t.Errorf("Expected error for activating not found version")
	}

	// Test Activate success
	err = store.Activate(ctx, "task1", "v1", 0.8)
	if err != nil {
		t.Errorf("Activate failed: %v", err)
	}
	if !activatedCalled {
		t.Errorf("Expected OnActivate callback to be called")
	}

	// Test GetActive
	active, err = store.GetActive(ctx, "task1")
	if err != nil {
		t.Errorf("GetActive failed: %v", err)
	}
	if active == nil || active.ID != "v1" {
		t.Errorf("Expected active version v1")
	}

	// Add second version and activate it
	v2 := &PromptVersion{
		ID:       "v2",
		Version:  2,
		TaskType: "task1",
		Prompt:   "prompt 2",
		Score:    0.95,
	}
	store.Save(ctx, v2)
	err = store.Activate(ctx, "task1", "v2", 0.8)
	if err != nil {
		t.Errorf("Activate v2 failed: %v", err)
	}

	// GetActive should return v2 now
	active, err = store.GetActive(ctx, "task1")
	if err != nil {
		t.Errorf("GetActive failed: %v", err)
	}
	if active == nil || active.ID != "v2" {
		t.Errorf("Expected active version v2")
	}

	// Wait 1 second to ensure created_at ordering works
	time.Sleep(1 * time.Second)

	v3 := &PromptVersion{
		ID:       "v3",
		Version:  3,
		TaskType: "task1",
		Prompt:   "prompt 3",
		Score:    0.6,
	}
	store.Save(ctx, v3)

	// Test ListRecent
	recent, err := store.ListRecent(ctx, "task1", 2)
	if err != nil {
		t.Errorf("ListRecent failed: %v", err)
	}
	if len(recent) != 2 {
		t.Errorf("Expected 2 recent versions, got %d", len(recent))
	}
	if recent[0].ID != "v3" {
		t.Errorf("Expected newest to be v3, got %s", recent[0].ID)
	}

	// Test ListRecent default
	recentAll, err := store.ListRecent(ctx, "task1", 0)
	if err != nil {
		t.Errorf("ListRecent default failed: %v", err)
	}
	if len(recentAll) != 3 {
		t.Errorf("Expected 3 recent versions, got %d", len(recentAll))
	}
}
