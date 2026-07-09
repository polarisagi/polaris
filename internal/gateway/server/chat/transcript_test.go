package chat

import (
	"os"
	"testing"
)

func TestTranscript(t *testing.T) {
	tempDir := t.TempDir()

	tw, err := openTranscript(tempDir, "session-1", true)
	if err != nil {
		t.Fatalf("openTranscript failed: %v", err)
	}

	// WriteTurn
	tw.WriteTurn("user", "hello", 100, 10)

	// WriteError
	tw.WriteError("500", "some error")

	// Close
	tw.Close()

	// Check if file exists
	files, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(files) == 0 {
		t.Errorf("no transcript files created")
	}

	// PruneTranscripts
	PruneTranscripts(tempDir, 0) // 0 days retention

	// File should be deleted
	files, err = os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(files) > 0 {
		t.Errorf("expected 0 files after prune, got %d", len(files))
	}
}
