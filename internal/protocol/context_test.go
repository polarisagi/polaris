package protocol

import (
	"context"
	"testing"
)

type contextKey string

func TestDetachedContext(t *testing.T) {
	// Create a parent context with a value and a cancel function
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey("key"), "value"))

	// Create detached context
	detached := Detach(parent)

	// Cancel the parent
	cancel()

	// Verify detached context still has the value
	if val := detached.Value(contextKey("key")); val != "value" {
		t.Errorf("Expected value 'value', got %v", val)
	}

	// Verify detached context is not canceled (Err() should be nil)
	if err := detached.Err(); err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}

	// Verify detached context has no deadline
	deadline, ok := detached.Deadline()
	if ok || !deadline.IsZero() {
		t.Errorf("Expected no deadline, got ok=%v, deadline=%v", ok, deadline)
	}

	// Verify detached context Done() channel is nil
	if done := detached.Done(); done != nil {
		t.Errorf("Expected nil Done() channel, got %v", done)
	}

	// Test CtxCapabilityToken and CtxDryRun types
	capCtx := context.WithValue(context.Background(), CtxCapabilityToken{}, "cap_token")
	if val := capCtx.Value(CtxCapabilityToken{}); val != "cap_token" {
		t.Errorf("Expected CtxCapabilityToken value 'cap_token', got %v", val)
	}

	dryRunCtx := context.WithValue(context.Background(), CtxDryRun{}, true)
	if val := dryRunCtx.Value(CtxDryRun{}); val != true {
		t.Errorf("Expected CtxDryRun value true, got %v", val)
	}
}
