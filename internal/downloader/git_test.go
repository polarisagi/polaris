package downloader

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestGitShortHash_NotARepo(t *testing.T) {
	dir := t.TempDir()
	hash := GitShortHash(dir)
	if hash != "" {
		t.Errorf("expected empty hash for non-repo, got %s", hash)
	}
}

func TestGitHash_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	hash := gitHash(dir)
	if hash != "" {
		t.Errorf("expected empty hash for empty dir, got %s", hash)
	}
}

func TestGitCloneOrPull_NotARepo(t *testing.T) {
	// Let's create an empty dir and call GitCloneOrPull with a fake local git repo.
	dir := t.TempDir()
	repoDir := t.TempDir()

	// Create a dummy file in repoDir so it's not totally empty,
	// though without git init it's not a repo either.
	os.WriteFile(filepath.Join(repoDir, "test.txt"), []byte("data"), 0644)

	// Since we don't have git setup in tests, just testing error paths
	available, updated := GitCloneOrPull(context.Background(), http.DefaultClient, "http://127.0.0.1:0/fake.git", dir)
	if available {
		t.Errorf("expected not available")
	}
	if updated {
		t.Errorf("expected not updated")
	}
}

func TestRunGitClone_Error(t *testing.T) {
	dir := t.TempDir()
	err := runGitClone(context.Background(), "http://127.0.0.1:0/fake.git", dir)
	if err == nil {
		t.Errorf("expected error on fake clone")
	}
}
