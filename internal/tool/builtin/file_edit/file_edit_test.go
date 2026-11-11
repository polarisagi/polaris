package file_edit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func openTestStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func testContext(root, session string) context.Context {
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, session)
	return protocol.WithProjectRoot(ctx, root)
}

func call(t *testing.T, fn func(context.Context, []byte) ([]byte, error), ctx context.Context, req request) (response, error) {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := fn(ctx, data)
	if err != nil {
		return response{}, err
	}
	var got response
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return got, nil
}

func TestFileEdit_PreviewApplyCreateDeleteAndMode(t *testing.T) {
	root := testRoot(t)
	oldPath := filepath.Join(root, "old.txt")
	if err := os.WriteFile(oldPath, []byte("before"), 0640); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	oldMode := uint32(oldInfo.Mode().Perm())
	ctx := testContext(root, "session-a")
	m := &manager{deps: Deps{AllowedPaths: []string{root}, Store: openTestStore(t)}, write: atomicWrite}
	preview, err := call(t, m.execute, ctx, request{Path: root, Action: "preview", Edits: []edit{
		{Path: "old.txt", Content: "after", ExpectedSHA256: hash([]byte("before"))},
		{Path: "new.txt", Content: "created", ExpectedAbsent: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	encodedPreview, _ := json.Marshal(preview)
	if preview.State != "prepared" || len(preview.Files) != 2 || strings.Contains(string(encodedPreview), "before") {
		t.Fatalf("unexpected or leaking preview: %+v", preview)
	}
	if _, err := call(t, m.execute, ctx, request{Path: root, Action: "apply", TransactionID: preview.TransactionID}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(oldPath)
	if err != nil || string(got) != "after" {
		t.Fatalf("updated file = %q, err=%v", got, err)
	}
	info, err := os.Stat(oldPath)
	if err != nil || !sameMode(uint32(info.Mode().Perm()), oldMode) {
		mode := os.FileMode(0)
		if info != nil {
			mode = info.Mode()
		}
		t.Fatalf("mode not preserved: mode=%v err=%v", mode, err)
	}
	newInfo, err := os.Stat(filepath.Join(root, "new.txt"))
	if err != nil || !sameMode(uint32(newInfo.Mode().Perm()), newMode(journalFile{})) {
		mode := os.FileMode(0)
		if newInfo != nil {
			mode = newInfo.Mode()
		}
		t.Fatalf("new file mode = %v, err=%v", mode, err)
	}
	result, err := call(t, m.execute, ctx, request{Path: root, Action: "status", TransactionID: preview.TransactionID})
	if err != nil || result.State != "applied" {
		t.Fatalf("status = %+v, err=%v", result, err)
	}
	delete, err := call(t, m.execute, ctx, request{Path: root, Action: "preview", Edits: []edit{{Path: "new.txt", Delete: true, ExpectedSHA256: hash([]byte("created"))}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m.execute, ctx, request{Path: root, Action: "apply", TransactionID: delete.TransactionID}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file still exists, err=%v", err)
	}
}

func TestFileEdit_ConflictPreventsAllWrites(t *testing.T) {
	root := testRoot(t)
	for name, content := range map[string]string{"a": "old-a", "b": "old-b"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := testContext(root, "session-a")
	m := &manager{deps: Deps{AllowedPaths: []string{root}, Store: openTestStore(t)}, write: atomicWrite}
	preview, err := call(t, m.execute, ctx, request{Path: root, Action: "preview", Edits: []edit{
		{Path: "a", Content: "new-a", ExpectedSHA256: hash([]byte("old-a"))},
		{Path: "b", Content: "new-b", ExpectedSHA256: hash([]byte("old-b"))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = call(t, m.execute, ctx, request{Path: root, Action: "apply", TransactionID: preview.TransactionID})
	if apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("apply error = %v", err)
	}
	for name, want := range map[string]string{"a": "old-a", "b": "outside"} {
		got, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil || string(got) != want {
			t.Fatalf("%s = %q, err=%v; want %q", name, got, readErr, want)
		}
	}
}

func TestFileEdit_ModeChangeAfterPreviewConflicts(t *testing.T) {
	root := testRoot(t)
	path := filepath.Join(root, "mode.txt")
	if err := os.WriteFile(path, []byte("old"), 0640); err != nil {
		t.Fatal(err)
	}
	ctx := testContext(root, "session-a")
	m := &manager{deps: Deps{AllowedPaths: []string{root}, Store: openTestStore(t)}, write: atomicWrite}
	preview, err := call(t, m.execute, ctx, request{Path: root, Action: "preview", Edits: []edit{{Path: "mode.txt", Content: "new", ExpectedSHA256: hash([]byte("old"))}}})
	if err != nil {
		t.Fatal(err)
	}
	changedMode := setReadOnly(t, path)
	_, err = call(t, m.execute, ctx, request{Path: root, Action: "apply", TransactionID: preview.TransactionID})
	if apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("mode-only apply conflict = %v", err)
	}
	got, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || string(got) != "old" || !sameMode(uint32(info.Mode().Perm()), changedMode) {
		mode := os.FileMode(0)
		if info != nil {
			mode = info.Mode().Perm()
		}
		t.Fatalf("mode conflict changed target: content=%q mode=%v readErr=%v statErr=%v", got, mode, err, statErr)
	}
}

func TestFileEdit_RecoveryPreservesModeOnlyChanges(t *testing.T) {
	root := testRoot(t)
	ctx := testContext(root, "session-a")
	if err := os.WriteFile(filepath.Join(root, "existing"), []byte("new-existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "created"), []byte("new-created"), 0644); err != nil {
		t.Fatal(err)
	}
	existingMode := setReadOnly(t, filepath.Join(root, "existing"))
	createdMode := setReadOnly(t, filepath.Join(root, "created"))
	m := &manager{deps: Deps{AllowedPaths: []string{root}, Store: openTestStore(t)}, write: atomicWrite}
	id, _ := randomID()
	oldExisting, newExisting := []byte("old-existing"), []byte("new-existing")
	newCreated := []byte("new-created")
	tx := transaction{ID: id, SessionID: "session-a", Root: root, State: "applying", Files: []journalFile{
		{Path: "existing", OldExists: true, OldContent: oldExisting, OldMode: 0600, OldHash: hash(oldExisting), NewExists: true, NewContent: newExisting, NewHash: hash(newExisting)},
		{Path: "created", NewExists: true, NewContent: newCreated, NewHash: hash(newCreated)},
	}}
	if err := m.save(ctx, tx); err != nil {
		t.Fatal(err)
	}
	_, err := call(t, m.execute, ctx, request{Path: root, Action: "recover", TransactionID: id})
	if apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("mode-only recovery conflict = %v", err)
	}
	for name, want := range map[string]string{"existing": string(newExisting), "created": string(newCreated)} {
		got, readErr := os.ReadFile(filepath.Join(root, name))
		info, statErr := os.Stat(filepath.Join(root, name))
		wantMode := existingMode
		if name == "created" {
			wantMode = createdMode
		}
		if readErr != nil || statErr != nil || string(got) != want || !sameMode(uint32(info.Mode().Perm()), wantMode) {
			mode := os.FileMode(0)
			if info != nil {
				mode = info.Mode().Perm()
			}
			t.Fatalf("recovery changed %s: content=%q mode=%v readErr=%v statErr=%v", name, got, mode, readErr, statErr)
		}
	}
}

func setReadOnly(t *testing.T, path string) uint32 {
	t.Helper()
	if err := os.Chmod(path, 0444); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return uint32(info.Mode().Perm())
}

func TestFileEdit_SecondWriteFailureRollsBackFirst(t *testing.T) {
	root := testRoot(t)
	for name, content := range map[string]string{"a": "old-a", "b": "old-b"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := testContext(root, "session-a")
	m := &manager{deps: Deps{AllowedPaths: []string{root}, Store: openTestStore(t)}, write: atomicWrite}
	preview, err := call(t, m.execute, ctx, request{Path: root, Action: "preview", Edits: []edit{
		{Path: "a", Content: "new-a", ExpectedSHA256: hash([]byte("old-a"))},
		{Path: "b", Content: "new-b", ExpectedSHA256: hash([]byte("old-b"))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.write = func(root *os.Root, path string, data []byte, mode uint32) error {
		calls++
		if calls == 2 {
			return errors.New("injected write failure")
		}
		return atomicWrite(root, path, data, mode)
	}
	_, err = call(t, m.execute, ctx, request{Path: root, Action: "apply", TransactionID: preview.TransactionID})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	for name, want := range map[string]string{"a": "old-a", "b": "old-b"} {
		got, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil || string(got) != want {
			t.Fatalf("%s = %q, err=%v; want %q", name, got, readErr, want)
		}
	}
}

func TestFileEdit_CancellationRollsBackAndPersists(t *testing.T) {
	root := testRoot(t)
	for name, content := range map[string]string{"a": "old-a", "b": "old-b"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(testContext(root, "session-a"))
	defer cancel()
	st := openTestStore(t)
	m := &manager{deps: Deps{AllowedPaths: []string{root}, Store: st}, write: atomicWrite}
	preview, err := call(t, m.execute, ctx, request{Path: root, Action: "preview", Edits: []edit{
		{Path: "a", Content: "new-a", ExpectedSHA256: hash([]byte("old-a"))},
		{Path: "b", Content: "new-b", ExpectedSHA256: hash([]byte("old-b"))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	write := m.write
	m.write = func(root *os.Root, path string, data []byte, mode uint32) error {
		if err := write(root, path, data, mode); err != nil {
			return err
		}
		cancel()
		return nil
	}
	_, err = call(t, m.execute, ctx, request{Path: root, Action: "apply", TransactionID: preview.TransactionID})
	if apperr.CodeOf(err) != apperr.CodeCancelled {
		t.Fatalf("apply cancellation error = %v", err)
	}
	for name, want := range map[string]string{"a": "old-a", "b": "old-b"} {
		got, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil || string(got) != want {
			t.Fatalf("%s = %q, err=%v; want %q", name, got, readErr, want)
		}
	}
	status, err := call(t, (&manager{deps: Deps{AllowedPaths: []string{root}, Store: st}, write: atomicWrite}).execute, testContext(root, "session-a"), request{Path: root, Action: "status", TransactionID: preview.TransactionID})
	if err != nil || status.State != "rolled_back" {
		t.Fatalf("cancelled status = %+v, err=%v", status, err)
	}
}

func TestFileEdit_RecoverAfterManagerRecreation(t *testing.T) {
	root := testRoot(t)
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := testContext(root, "session-a")
	st := openTestStore(t)
	deps := Deps{AllowedPaths: []string{root}, Store: st}
	m := &manager{deps: deps, write: atomicWrite}
	preview, err := call(t, m.execute, ctx, request{Path: root, Action: "preview", Edits: []edit{{Path: "a", Content: "new", ExpectedSHA256: hash([]byte("old"))}}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.write = func(root *os.Root, path string, data []byte, mode uint32) error {
		calls++
		if calls == 1 {
			if err := atomicWrite(root, path, data, mode); err != nil {
				return err
			}
			return errors.New("fail after write")
		}
		return errors.New("fail rollback")
	}
	if _, err := call(t, m.execute, ctx, request{Path: root, Action: "apply", TransactionID: preview.TransactionID}); err == nil {
		t.Fatal("expected apply and rollback failure")
	}
	status, err := call(t, (&manager{deps: deps, write: atomicWrite}).execute, ctx, request{Path: root, Action: "status", TransactionID: preview.TransactionID})
	if err != nil || status.State != "applying" {
		t.Fatalf("reloaded status = %+v, err=%v", status, err)
	}
	_, err = call(t, (&manager{deps: deps, write: atomicWrite}).execute, ctx, request{Path: root, Action: "recover", TransactionID: preview.TransactionID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "a"))
	if err != nil || string(got) != "old" {
		t.Fatalf("recovered content = %q, err=%v", got, err)
	}
}

func TestFileEdit_RecoveryPreservesUserConflict(t *testing.T) {
	root := testRoot(t)
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("user edit"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := testContext(root, "session-a")
	st := openTestStore(t)
	m := &manager{deps: Deps{AllowedPaths: []string{root}, Store: st}, write: atomicWrite}
	id, _ := randomID()
	tx := transaction{ID: id, SessionID: "session-a", Root: root, State: "applying", Files: []journalFile{{Path: "a", OldExists: true, OldContent: []byte("old"), OldMode: 0600, OldHash: hash([]byte("old")), NewExists: true, NewContent: []byte("new"), NewHash: hash([]byte("new"))}}}
	if err := m.save(ctx, tx); err != nil {
		t.Fatal(err)
	}
	_, err := call(t, m.execute, ctx, request{Path: root, Action: "recover", TransactionID: id})
	if apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("recover error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "a"))
	if err != nil || string(got) != "user edit" {
		t.Fatalf("recovery overwrote user content: %q, err=%v", got, err)
	}
}

func TestFileEdit_RejectsMissingStoreSessionAndUnsafeTargets(t *testing.T) {
	root := testRoot(t)
	ctx := testContext(root, "session-a")
	req := request{Path: root, Action: "preview", Edits: []edit{{Path: "new", Content: "x", ExpectedAbsent: true}}}
	if _, err := call(t, MakeFileEditFn(Deps{AllowedPaths: []string{root}}), ctx, req); apperr.CodeOf(err) != apperr.CodeServiceUnavailable {
		t.Fatalf("nil store error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nil store wrote a file: err=%v", err)
	}
	m := &manager{deps: Deps{AllowedPaths: []string{root}, Store: openTestStore(t)}, write: atomicWrite}
	if _, err := call(t, m.execute, context.Background(), req); err == nil {
		t.Fatal("missing session was accepted")
	}
	for _, path := range []string{"../escape", filepath.Join(root, "absolute"), ".git/config"} {
		bad := req
		bad.Edits = []edit{{Path: path, Content: "x", ExpectedAbsent: true}}
		if _, err := call(t, m.execute, ctx, bad); err == nil {
			t.Errorf("unsafe path %q accepted", path)
		}
	}
	if err := os.Symlink(root, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	bad := req
	bad.Edits = []edit{{Path: "link/new", Content: "x", ExpectedAbsent: true}}
	if _, err := call(t, m.execute, ctx, bad); err == nil {
		t.Fatal("symlink traversal accepted")
	}
	bad.Edits = []edit{{Path: "new", Content: "x", ExpectedAbsent: true}, {Path: "new", Content: "y", ExpectedAbsent: true}}
	if _, err := call(t, m.execute, ctx, bad); err == nil {
		t.Fatal("duplicate target accepted")
	}
	bad.Edits = []edit{{Path: "Case.txt", Content: "x", ExpectedAbsent: true}, {Path: "case.txt", Content: "y", ExpectedAbsent: true}}
	if _, err := call(t, m.execute, ctx, bad); err == nil {
		t.Fatal("case alias duplicate target accepted")
	}
	bad.Edits = make([]edit, maxFiles+1)
	for i := range bad.Edits {
		bad.Edits[i] = edit{Path: filepath.Join("nested", string(rune('a'+i))), ExpectedAbsent: true}
	}
	if _, err := call(t, m.execute, ctx, bad); err == nil {
		t.Fatal("more than 32 targets accepted")
	}
	bad.Edits = []edit{{Path: "large", Content: strings.Repeat("x", maxTotalBytes+1), ExpectedAbsent: true}}
	if _, err := call(t, m.execute, ctx, bad); apperr.CodeOf(err) != apperr.CodeResourceExhausted {
		t.Fatalf("oversize content error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "huge"), []byte(strings.Repeat("x", maxTotalBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	bad.Edits = []edit{{Path: "huge", Content: "small", ExpectedSHA256: hash([]byte(strings.Repeat("x", maxTotalBytes+1)))}}
	if _, err := call(t, m.execute, ctx, bad); apperr.CodeOf(err) != apperr.CodeResourceExhausted {
		t.Fatalf("oversize existing file error = %v", err)
	}
}

func TestFileEdit_RejectsRootAndSessionMismatch(t *testing.T) {
	root, outside := testRoot(t), testRoot(t)
	ctx := testContext(root, "session-a")
	m := &manager{deps: Deps{AllowedPaths: []string{root}, Store: openTestStore(t)}, write: atomicWrite}
	if _, err := call(t, m.execute, ctx, request{Path: outside, Action: "preview", Edits: []edit{{Path: "x", ExpectedAbsent: true}}}); err == nil {
		t.Fatal("outside root accepted")
	}
	preview, err := call(t, m.execute, ctx, request{Path: root, Action: "preview", Edits: []edit{{Path: "x", ExpectedAbsent: true}}})
	if err != nil {
		t.Fatal(err)
	}
	other := testContext(root, "session-b")
	if _, err := call(t, m.execute, other, request{Path: root, Action: "status", TransactionID: preview.TransactionID}); apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("cross-session status error = %v", err)
	}
	if _, err := call(t, m.execute, ctx, request{Path: root, Action: "status", TransactionID: strings.Repeat("0", 32)}); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("unknown transaction error = %v", err)
	}
	key := keyFor(preview.TransactionID)
	data, err := stGet(m.deps.Store, ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var tx transaction
	if err := json.Unmarshal(data, &tx); err != nil {
		t.Fatal(err)
	}
	tx.Files[0].Path = "../outside"
	data, _ = json.Marshal(tx)
	if err := m.deps.Store.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m.execute, ctx, request{Path: root, Action: "status", TransactionID: preview.TransactionID}); err == nil {
		t.Fatal("tampered journal path accepted")
	}
	tx.Files[0].Path = "x"
	tx.Files[0].OldContent = []byte("journal content tampered")
	data, _ = json.Marshal(tx)
	if err := m.deps.Store.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m.execute, ctx, request{Path: root, Action: "status", TransactionID: preview.TransactionID}); err == nil {
		t.Fatal("tampered journal snapshot accepted")
	}
}

func stGet(s protocol.Store, ctx context.Context, key []byte) ([]byte, error) {
	return s.Get(ctx, key)
}
