package file_edit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	maxFiles       = 32
	maxTotalBytes  = 8 << 20
	journalPrefix  = "builtin:file_edit:v1:"
	maxJournalSize = 20 << 20
)

type Deps struct {
	AllowedPaths []string
	Store        protocol.Store
}

type request struct {
	Path          string `json:"path"`
	Action        string `json:"action"`
	TransactionID string `json:"transaction_id,omitempty"`
	Edits         []edit `json:"edits,omitempty"`
}

type edit struct {
	Path           string `json:"path"`
	Content        string `json:"content,omitempty"`
	Delete         bool   `json:"delete,omitempty"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	ExpectedAbsent bool   `json:"expected_absent,omitempty"`
}

type response struct {
	TransactionID string        `json:"transaction_id,omitempty"`
	State         string        `json:"state"`
	Files         []fileSummary `json:"files,omitempty"`
	Error         string        `json:"error,omitempty"`
}

type fileSummary struct {
	Path      string `json:"path"`
	OldSHA256 string `json:"old_sha256,omitempty"`
	NewSHA256 string `json:"new_sha256,omitempty"`
	Deleted   bool   `json:"deleted,omitempty"`
}

type transaction struct {
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	Root      string        `json:"root"`
	State     string        `json:"state"`
	Files     []journalFile `json:"files"`
}

type journalFile struct {
	Path       string `json:"path"`
	OldExists  bool   `json:"old_exists"`
	OldContent []byte `json:"old_content,omitempty"`
	OldMode    uint32 `json:"old_mode,omitempty"`
	OldHash    string `json:"old_hash,omitempty"`
	NewExists  bool   `json:"new_exists"`
	NewContent []byte `json:"new_content,omitempty"`
	NewHash    string `json:"new_hash,omitempty"`
}

type manager struct {
	deps  Deps
	mu    sync.Mutex
	write func(*os.Root, string, []byte, uint32) error
}

// MakeFileEditFn 创建受会话、项目根和持久 journal 约束的文件编辑工具。
func MakeFileEditFn(deps Deps) sandbox.InProcessFn {
	deps.AllowedPaths = append([]string(nil), deps.AllowedPaths...)
	m := &manager{deps: deps, write: atomicWrite}
	return m.execute
}

func (m *manager) execute(ctx context.Context, input []byte) ([]byte, error) {
	if m.deps.Store == nil {
		return nil, apperr.New(apperr.CodeServiceUnavailable, "file_edit: persistent store unavailable")
	}
	sessionID, err := guard.SessionIDFromCtx(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "file_edit: session identity unavailable", err)
	}
	var req request
	if len(input) > maxTotalBytes+(1<<20) {
		return nil, apperr.New(apperr.CodeResourceExhausted, "file_edit: request exceeds input size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "file_edit: invalid arguments", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, apperr.New(apperr.CodeInvalidInput, "file_edit: trailing JSON data")
	}
	if err := m.validateRoot(ctx, req.Path); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out response
	switch req.Action {
	case "preview":
		if req.TransactionID != "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "file_edit: preview does not accept transaction_id")
		}
		out, err = m.preview(ctx, sessionID, req)
	case "apply":
		out, err = m.apply(ctx, sessionID, req)
	case "status":
		out, err = m.status(ctx, sessionID, req)
	case "recover":
		out, err = m.recover(ctx, sessionID, req)
	default:
		return nil, apperr.New(apperr.CodeInvalidInput, "file_edit: action must be preview, apply, status, or recover")
	}
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "file_edit: encode response", err)
	}
	return encoded, nil
}

func (m *manager) validateRoot(ctx context.Context, root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return apperr.New(apperr.CodeInvalidInput, "file_edit: path must be a clean absolute project root")
	}
	if err := guard.CheckScopeRoot(root); err != nil {
		return apperr.Wrap(apperr.CodeForbidden, "file_edit: project root is forbidden", err)
	}
	if err := guard.CheckWritablePath(ctx, root, guard.ScopedPaths(ctx, m.deps.AllowedPaths)); err != nil {
		return apperr.Wrap(apperr.CodeForbidden, "file_edit: project root is outside allowed paths", err)
	}
	physical, err := filepath.EvalSymlinks(root)
	if err != nil || physical != root {
		if err != nil {
			return apperr.Wrap(apperr.CodeInvalidInput, "file_edit: project root cannot be resolved", err)
		}
		return apperr.New(apperr.CodeForbidden, "file_edit: symbolic-link project root is forbidden")
	}
	return nil
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "file_edit: generate transaction id", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validateTransactionID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && !strings.Contains(id, "/")
}

func keyFor(id string) []byte { return []byte(journalPrefix + id) }

func (m *manager) load(ctx context.Context, id, sessionID, root string) (transaction, error) {
	if !validateTransactionID(id) {
		return transaction{}, apperr.New(apperr.CodeInvalidInput, "file_edit: invalid transaction_id")
	}
	data, err := m.deps.Store.Get(ctx, keyFor(id))
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) || apperr.CodeOf(err) == apperr.CodeNotFound {
			return transaction{}, apperr.New(apperr.CodeNotFound, "file_edit: transaction not found")
		}
		return transaction{}, apperr.Wrap(apperr.CodeStorageUnavailable, "file_edit: read journal", err)
	}
	if len(data) == 0 || len(data) > maxJournalSize {
		return transaction{}, apperr.New(apperr.CodeNotFound, "file_edit: transaction not found")
	}
	var tx transaction
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tx); err != nil {
		return transaction{}, apperr.Wrap(apperr.CodeInternal, "file_edit: decode journal", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return transaction{}, apperr.New(apperr.CodeInternal, "file_edit: trailing journal data")
	}
	if tx.ID != id || tx.SessionID != sessionID || tx.Root != root {
		return transaction{}, apperr.New(apperr.CodeForbidden, "file_edit: transaction scope mismatch")
	}
	if err := m.validateJournal(ctx, tx); err != nil {
		return transaction{}, err
	}
	return tx, nil
}

func (m *manager) validateJournal(ctx context.Context, tx transaction) error {
	if !validateTransactionID(tx.ID) || tx.SessionID == "" || tx.Root == "" || len(tx.Files) == 0 || len(tx.Files) > maxFiles {
		return apperr.New(apperr.CodeInternal, "file_edit: invalid persisted journal header")
	}
	switch tx.State {
	case "prepared", "applying", "applied", "rolled_back", "conflict":
	default:
		return apperr.New(apperr.CodeInternal, "file_edit: invalid persisted journal state")
	}
	allowed := guard.ScopedPaths(ctx, m.deps.AllowedPaths)
	seen := make(map[string]struct{}, len(tx.Files))
	total := 0
	for _, file := range tx.Files {
		bytes, err := validateJournalFile(ctx, tx.Root, allowed, file, seen)
		if err != nil {
			return err
		}
		total += bytes
		if total > maxTotalBytes {
			return apperr.New(apperr.CodeResourceExhausted, "file_edit: persisted journal content exceeds 8 MiB")
		}
	}
	return nil
}

func validateJournalFile(ctx context.Context, root string, allowed []string, file journalFile, seen map[string]struct{}) (int, error) {
	if !file.OldExists && !file.NewExists {
		return 0, apperr.New(apperr.CodeInternal, "file_edit: persisted journal has an empty operation")
	}
	name, err := validateRelative(file.Path)
	if err != nil || name != file.Path {
		return 0, apperr.New(apperr.CodeInternal, "file_edit: invalid persisted journal path")
	}
	canonical := strings.ToLower(filepath.ToSlash(name))
	if _, ok := seen[canonical]; ok {
		return 0, apperr.New(apperr.CodeInternal, "file_edit: duplicate persisted journal path")
	}
	seen[canonical] = struct{}{}
	if err := guard.CheckWritablePath(ctx, filepath.Join(root, name), allowed); err != nil {
		return 0, apperr.Wrap(apperr.CodeForbidden, "file_edit: persisted journal target is forbidden", err)
	}
	if err := validateJournalSnapshots(file); err != nil {
		return 0, err
	}
	return len(file.OldContent) + len(file.NewContent), nil
}

func validateJournalSnapshots(file journalFile) error {
	if file.OldExists && (len(file.OldContent) > maxTotalBytes || hash(file.OldContent) != file.OldHash || file.OldMode&^uint32(0777) != 0) {
		return apperr.New(apperr.CodeInternal, "file_edit: invalid persisted old snapshot")
	}
	if !file.OldExists && (len(file.OldContent) != 0 || file.OldHash != "" || file.OldMode != 0) {
		return apperr.New(apperr.CodeInternal, "file_edit: unexpected persisted old snapshot")
	}
	if file.NewExists && (len(file.NewContent) > maxTotalBytes || hash(file.NewContent) != file.NewHash) {
		return apperr.New(apperr.CodeInternal, "file_edit: invalid persisted new snapshot")
	}
	if !file.NewExists && (len(file.NewContent) != 0 || file.NewHash != "") {
		return apperr.New(apperr.CodeInternal, "file_edit: unexpected persisted new snapshot")
	}
	return nil
}

func (m *manager) save(ctx context.Context, tx transaction) error {
	data, err := json.Marshal(tx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "file_edit: encode journal", err)
	}
	if len(data) > maxJournalSize {
		return apperr.New(apperr.CodeResourceExhausted, "file_edit: journal size limit exceeded")
	}
	if err := m.deps.Store.Put(ctx, keyFor(tx.ID), data); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "file_edit: write journal", err)
	}
	return nil
}

func (tx transaction) summary() response {
	out := response{TransactionID: tx.ID, State: tx.State, Files: make([]fileSummary, 0, len(tx.Files))}
	for _, file := range tx.Files {
		out.Files = append(out.Files, fileSummary{Path: file.Path, OldSHA256: file.OldHash, NewSHA256: file.NewHash, Deleted: !file.NewExists})
	}
	return out
}

func invalidf(format string, args ...any) error {
	return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf(format, args...))
}
