package file_edit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func (m *manager) preview(ctx context.Context, sessionID string, req request) (response, error) {
	if len(req.Edits) == 0 || len(req.Edits) > maxFiles {
		return response{}, apperr.New(apperr.CodeInvalidInput, "file_edit: edits must contain 1 to 32 files")
	}
	root, err := openProjectRoot(req.Path)
	if err != nil {
		return response{}, apperr.Wrap(apperr.CodeInvalidInput, "file_edit: open project root", err)
	}
	defer root.Close()
	files, err := prepareFiles(ctx, root, req.Path, guard.ScopedPaths(ctx, m.deps.AllowedPaths), req.Edits)
	if err != nil {
		return response{}, err
	}
	id, err := randomID()
	if err != nil {
		return response{}, err
	}
	tx := transaction{ID: id, SessionID: sessionID, Root: req.Path, State: "prepared", Files: files}
	if err := m.save(ctx, tx); err != nil {
		return response{}, err
	}
	return tx.summary(), nil
}

func openProjectRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "file_edit: project root is not a real directory", err)
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, apperr.New(apperr.CodeForbidden, "file_edit: project root is not a real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "file_edit: open project root", err)
	}
	after, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, apperr.Wrap(apperr.CodeForbidden, "file_edit: project root changed while opening", err)
	}
	if !os.SameFile(before, after) {
		_ = root.Close()
		return nil, apperr.New(apperr.CodeForbidden, "file_edit: project root changed while opening")
	}
	return root, nil
}

func prepareFiles(ctx context.Context, root *os.Root, projectRoot string, allowed []string, edits []edit) ([]journalFile, error) {
	ordered := append([]edit(nil), edits...)
	sort.Slice(ordered, func(i, j int) bool { return filepath.ToSlash(ordered[i].Path) < filepath.ToSlash(ordered[j].Path) })
	seen := make(map[string]struct{}, len(ordered))
	files := make([]journalFile, 0, len(ordered))
	total := 0
	for _, item := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, apperr.Wrap(apperr.CodeCancelled, "file_edit: preview cancelled", err)
		}
		file, size, err := prepareOne(ctx, root, projectRoot, allowed, item)
		if err != nil {
			return nil, err
		}
		name := file.Path
		canonical := strings.ToLower(filepath.ToSlash(name))
		if _, ok := seen[canonical]; ok {
			return nil, invalidf("file_edit: duplicate target %q", item.Path)
		}
		seen[canonical] = struct{}{}
		total += size
		if total > maxTotalBytes {
			return nil, apperr.New(apperr.CodeResourceExhausted, "file_edit: transaction content exceeds 8 MiB")
		}
		files = append(files, file)
	}
	return files, nil
}

func prepareOne(ctx context.Context, root *os.Root, projectRoot string, allowed []string, item edit) (journalFile, int, error) {
	name, err := validateRelative(item.Path)
	if err != nil {
		return journalFile{}, 0, err
	}
	if err := guard.CheckWritablePath(ctx, filepath.Join(projectRoot, name), allowed); err != nil {
		return journalFile{}, 0, apperr.Wrap(apperr.CodeForbidden, "file_edit: target path is forbidden", err)
	}
	old, mode, exists, err := readTarget(root, name)
	if err != nil {
		return journalFile{}, 0, err
	}
	if err := validateEditExpectation(item, old, exists); err != nil {
		return journalFile{}, 0, err
	}
	file := journalFile{Path: name, OldExists: exists, OldContent: old, OldMode: mode, NewExists: !item.Delete}
	size := len(old)
	if exists {
		file.OldHash = hash(old)
	}
	if !item.Delete {
		file.NewContent = []byte(item.Content)
		file.NewHash = hash(file.NewContent)
		size += len(file.NewContent)
	}
	return file, size, nil
}

func validateEditExpectation(item edit, old []byte, exists bool) error {
	if exists == item.ExpectedAbsent {
		return apperr.New(apperr.CodeConflict, "file_edit: expected file existence does not match")
	}
	if exists && (item.ExpectedSHA256 == "" || !strings.EqualFold(item.ExpectedSHA256, hash(old))) {
		return apperr.New(apperr.CodeConflict, "file_edit: expected_sha256 mismatch")
	}
	if !exists && item.ExpectedSHA256 != "" {
		return invalidf("file_edit: expected_sha256 is not allowed for a new file")
	}
	if item.Delete && !exists {
		return apperr.New(apperr.CodeConflict, "file_edit: cannot delete a missing file")
	}
	if item.Delete && item.Content != "" {
		return invalidf("file_edit: delete and content are mutually exclusive")
	}
	return nil
}

func validateRelative(path string) (string, error) {
	if path == "" || filepath.IsAbs(path) {
		return "", invalidf("file_edit: edit path must be relative")
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean != path {
		return "", invalidf("file_edit: edit path contains traversal or non-canonical components")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".git" || strings.EqualFold(part, ".git") {
			return "", apperr.New(apperr.CodeForbidden, "file_edit: .git paths are protected")
		}
		lower := strings.ToLower(part)
		if strings.Contains(lower, ".file-edit-") && strings.HasSuffix(lower, ".tmp") {
			return "", apperr.New(apperr.CodeForbidden, "file_edit: transaction temporary files are protected")
		}
	}
	return clean, nil
}

func readTarget(root *os.Root, path string) ([]byte, uint32, bool, error) {
	file, info, exists, err := openTarget(root, path)
	if err != nil || !exists {
		return nil, 0, exists, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxTotalBytes+1))
	if err != nil {
		return nil, 0, false, apperr.Wrap(apperr.CodeInternal, "file_edit: read target", err)
	}
	if len(data) > maxTotalBytes {
		return nil, 0, false, apperr.New(apperr.CodeResourceExhausted, "file_edit: existing file exceeds 8 MiB")
	}
	return data, uint32(info.Mode().Perm()), true, nil
}

func openTarget(root *os.Root, path string) (*os.File, fs.FileInfo, bool, error) {
	if err := rejectSymlinkParents(root, path); err != nil {
		return nil, nil, false, err
	}
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, apperr.Wrap(apperr.CodeInternal, "file_edit: inspect target", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, false, apperr.New(apperr.CodeForbidden, "file_edit: target must be a regular non-symlink file")
	}
	if info.Size() < 0 || info.Size() > maxTotalBytes {
		return nil, nil, false, apperr.New(apperr.CodeResourceExhausted, "file_edit: existing file exceeds 8 MiB")
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, nil, false, apperr.Wrap(apperr.CodeInternal, "file_edit: read target", err)
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Size() < 0 || openedInfo.Size() > maxTotalBytes {
		_ = file.Close()
		return nil, nil, false, apperr.New(apperr.CodeForbidden, "file_edit: target changed or exceeds size limit")
	}
	return file, openedInfo, true, nil
}

func rejectSymlinkParents(root *os.Root, path string) error {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := 0; i < len(parts)-1; i++ {
		prefix := filepath.Join(parts[:i+1]...)
		info, err := root.Lstat(prefix)
		if err != nil {
			return apperr.Wrap(apperr.CodeInvalidInput, "file_edit: parent directory missing or inaccessible", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return apperr.New(apperr.CodeForbidden, "file_edit: symlink or non-directory parent is forbidden")
		}
	}
	return nil
}

func (m *manager) apply(ctx context.Context, sessionID string, req request) (response, error) {
	if req.TransactionID == "" || len(req.Edits) != 0 {
		return response{}, invalidf("file_edit: apply requires transaction_id and no edits")
	}
	tx, err := m.load(ctx, req.TransactionID, sessionID, req.Path)
	if err != nil {
		return response{}, err
	}
	if tx.State != "prepared" {
		return response{}, apperr.New(apperr.CodeConflict, "file_edit: transaction is not prepared")
	}
	root, err := openProjectRoot(req.Path)
	if err != nil {
		return response{}, apperr.Wrap(apperr.CodeInvalidInput, "file_edit: open project root", err)
	}
	defer root.Close()
	if err := validateSnapshot(root, tx.Files, false); err != nil {
		tx.State = "conflict"
		if saveErr := m.save(ctx, tx); saveErr != nil {
			return response{}, apperr.Wrap(apperr.CodeStorageUnavailable, "file_edit: conflict journal update failed", saveErr)
		}
		return response{}, err
	}
	tx.State = "applying"
	if err := m.save(ctx, tx); err != nil {
		return response{}, err
	}
	if err := m.applyFiles(ctx, tx, root); err != nil {
		return response{}, err
	}
	if err := ctx.Err(); err != nil {
		if rollbackErr := rollback(root, tx.Files, m.write); rollbackErr != nil {
			return response{}, apperr.Wrap(apperr.CodeInternal, "file_edit: cancelled after writes and rollback failed", rollbackErr)
		}
		tx.State = "rolled_back"
		if saveErr := m.save(context.WithoutCancel(ctx), tx); saveErr != nil {
			return response{}, apperr.Wrap(apperr.CodeStorageUnavailable, "file_edit: cancellation journal update failed", saveErr)
		}
		return response{}, apperr.Wrap(apperr.CodeCancelled, "file_edit: apply cancelled", err)
	}
	tx.State = "applied"
	if err := m.save(ctx, tx); err != nil {
		rollbackErr := rollback(root, tx.Files, m.write)
		if rollbackErr != nil {
			return response{}, apperr.Wrap(apperr.CodeInternal, "file_edit: final journal write and rollback failed", rollbackErr)
		}
		return response{}, apperr.Wrap(apperr.CodeStorageUnavailable, "file_edit: final journal write failed; files rolled back", err)
	}
	return tx.summary(), nil
}

func (m *manager) applyFiles(ctx context.Context, tx transaction, root *os.Root) error {
	for i, file := range tx.Files {
		if err := m.applyFile(ctx, tx, root, i, file); err != nil {
			return err
		}
	}
	return nil
}

func (m *manager) applyFile(ctx context.Context, tx transaction, root *os.Root, index int, file journalFile) error {
	if err := ctx.Err(); err != nil {
		return m.rollbackFailure(context.WithoutCancel(ctx), tx, root, index, "rolled_back", "file_edit: apply cancelled", apperr.Wrap(apperr.CodeCancelled, "file_edit: apply cancelled", err))
	}
	if err := validateOne(root, file, false); err != nil {
		return m.rollbackFailure(context.WithoutCancel(ctx), tx, root, index, "conflict", "file_edit: conflict rollback failed", err)
	}
	if err := applyOne(root, file, m.write); err != nil {
		return m.rollbackFailure(context.WithoutCancel(ctx), tx, root, index+1, "rolled_back", "file_edit: apply rollback failed", apperr.Wrap(apperr.CodeInternal, "file_edit: apply failed", err))
	}
	return nil
}

func (m *manager) rollbackFailure(ctx context.Context, tx transaction, root *os.Root, count int, state, message string, cause error) error {
	if rollbackErr := rollback(root, tx.Files[:count], m.write); rollbackErr != nil {
		tx.State = "applying"
		if saveErr := m.save(ctx, tx); saveErr != nil {
			return apperr.Wrap(apperr.CodeStorageUnavailable, "file_edit: record incomplete rollback", saveErr)
		}
		return apperr.Wrap(apperr.CodeInternal, message, rollbackErr)
	}
	tx.State = state
	if saveErr := m.save(ctx, tx); saveErr != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "file_edit: record rollback", saveErr)
	}
	return cause
}

func validateSnapshot(root *os.Root, files []journalFile, isNew bool) error {
	for _, file := range files {
		if err := validateOne(root, file, isNew); err != nil {
			return err
		}
	}
	return nil
}

func validateOne(root *os.Root, file journalFile, isNew bool) error {
	data, mode, exists, err := readTarget(root, file.Path)
	if err != nil {
		return err
	}
	expectedExists, expectedHash := file.OldExists, file.OldHash
	expectedMode := file.OldMode
	if isNew {
		expectedExists, expectedHash = file.NewExists, file.NewHash
		expectedMode = newMode(file)
	}
	if exists != expectedExists || exists && (hash(data) != expectedHash || !sameMode(mode, expectedMode)) {
		return apperr.New(apperr.CodeConflict, "file_edit: file changed since preview or during transaction")
	}
	return nil
}

func newMode(file journalFile) uint32 {
	if file.OldExists {
		return file.OldMode
	}
	if runtime.GOOS == "windows" {
		return 0666
	}
	return 0600
}

func sameMode(actual, expected uint32) bool {
	if runtime.GOOS == "windows" {
		return (actual&0222 == 0) == (expected&0222 == 0)
	}
	return actual == expected
}

func applyOne(root *os.Root, file journalFile, write func(*os.Root, string, []byte, uint32) error) error {
	if !file.NewExists {
		if err := root.Remove(file.Path); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "file_edit: delete target", err)
		}
		return nil
	}
	mode := file.OldMode
	if !file.OldExists {
		mode = 0600
	}
	if err := write(root, file.Path, file.NewContent, mode); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "file_edit: write target", err)
	}
	return nil
}

func rollback(root *os.Root, files []journalFile, write func(*os.Root, string, []byte, uint32) error) error {
	var failures []string
	for i := len(files) - 1; i >= 0; i-- {
		file := files[i]
		data, mode, exists, err := readTarget(root, file.Path)
		if err != nil {
			failures = append(failures, file.Path+": "+err.Error())
			continue
		}
		if exists == file.OldExists && (!exists || hash(data) == file.OldHash && sameMode(mode, file.OldMode)) {
			continue
		}
		if exists != file.NewExists || exists && (hash(data) != file.NewHash || !sameMode(mode, newMode(file))) {
			failures = append(failures, file.Path+": recovery conflict; file changed after transaction")
			continue
		}
		if file.OldExists {
			if err := write(root, file.Path, file.OldContent, file.OldMode); err != nil {
				failures = append(failures, file.Path+": "+err.Error())
			}
		} else if err := root.Remove(file.Path); err != nil {
			failures = append(failures, file.Path+": "+err.Error())
		}
	}
	if len(failures) != 0 {
		return apperr.New(apperr.CodeConflict, strings.Join(failures, "; "))
	}
	return nil
}

func atomicWrite(root *os.Root, path string, data []byte, mode uint32) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	temp := path + ".file-edit-" + id + ".tmp"
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(mode))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "file_edit: create temporary file", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return cleanupTemp(root, temp, apperr.Wrap(apperr.CodeInternal, "file_edit: write temporary file", err))
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return cleanupTemp(root, temp, apperr.Wrap(apperr.CodeInternal, "file_edit: sync temporary file", err))
	}
	if err := file.Chmod(fs.FileMode(mode)); err != nil {
		return cleanupTemp(root, temp, apperr.Wrap(apperr.CodeInternal, "file_edit: set temporary file permissions", err))
	}
	if err := file.Sync(); err != nil {
		return cleanupTemp(root, temp, apperr.Wrap(apperr.CodeInternal, "file_edit: sync temporary permissions", err))
	}
	if err := file.Close(); err != nil {
		return cleanupTemp(root, temp, apperr.Wrap(apperr.CodeInternal, "file_edit: close temporary file", err))
	}
	if err := root.Rename(temp, path); err != nil {
		return cleanupTemp(root, temp, apperr.Wrap(apperr.CodeInternal, "file_edit: replace target", err))
	}
	return nil
}

func cleanupTemp(root *os.Root, temp string, cause error) error {
	if err := root.Remove(temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return apperr.Wrap(apperr.CodeInternal, "file_edit: temporary file cleanup failed", errors.Join(cause, err))
	}
	return cause
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (m *manager) status(ctx context.Context, sessionID string, req request) (response, error) {
	if req.TransactionID == "" || len(req.Edits) != 0 {
		return response{}, invalidf("file_edit: status requires transaction_id and no edits")
	}
	tx, err := m.load(ctx, req.TransactionID, sessionID, req.Path)
	if err != nil {
		return response{}, err
	}
	return tx.summary(), nil
}

func (m *manager) recover(ctx context.Context, sessionID string, req request) (response, error) {
	if req.TransactionID == "" || len(req.Edits) != 0 {
		return response{}, invalidf("file_edit: recover requires transaction_id and no edits")
	}
	tx, err := m.load(ctx, req.TransactionID, sessionID, req.Path)
	if err != nil {
		return response{}, err
	}
	if tx.State != "applying" {
		return response{}, apperr.New(apperr.CodeConflict, "file_edit: only applying transactions can be recovered")
	}
	root, err := openProjectRoot(req.Path)
	if err != nil {
		return response{}, apperr.Wrap(apperr.CodeInvalidInput, "file_edit: open project root", err)
	}
	defer root.Close()
	if err := rollback(root, tx.Files, m.write); err != nil {
		tx.State = "conflict"
		if saveErr := m.save(ctx, tx); saveErr != nil {
			return response{}, apperr.Wrap(apperr.CodeInternal, "file_edit: recovery conflict and journal update failed", saveErr)
		}
		return response{}, apperr.Wrap(apperr.CodeConflict, "file_edit: recovery incomplete; conflicting files were preserved", err)
	}
	tx.State = "rolled_back"
	if err := m.save(ctx, tx); err != nil {
		return response{}, apperr.Wrap(apperr.CodeStorageUnavailable, "file_edit: recovery succeeded but journal update failed", err)
	}
	return tx.summary(), nil
}
