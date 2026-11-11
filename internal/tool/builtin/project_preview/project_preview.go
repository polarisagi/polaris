package project_preview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

const (
	maxActivePreviews  = 3
	maxSessionPreviews = 1
	cleanupTimeout     = 5 * time.Second
)

type Deps struct {
	AllowedPaths   []string
	Registry       protocol.ToolRegistry
	Store          protocol.Store
	SafeHTTPClient network.SafeHTTPClient
	JobManager     *bash.JobManager
}

type arguments struct {
	Action  string `json:"action"`
	Path    string `json:"path"`
	JobID   string `json:"job_id"`
	Timeout int    `json:"timeout"`
}

type record struct {
	SessionID string    `json:"session_id"`
	Root      string    `json:"root"`
	JobID     string    `json:"job_id"`
	Port      int       `json:"port"`
	StartedAt time.Time `json:"started_at"`
	State     string    `json:"state,omitempty"`
}

type response struct {
	Status string `json:"status"`
	JobID  string `json:"job_id,omitempty"`
	URL    string `json:"url,omitempty"`
	Host   string `json:"host,omitempty"`
	Output string `json:"output,omitempty"`
}

type previewTool struct {
	deps          Deps
	mu            sync.Mutex
	active        map[string]record
	pending       map[string]int
	portAllocator func() (int, error)
}

func MakeProjectPreviewFn(deps Deps) sandbox.InProcessRichFn {
	tool := &previewTool{deps: deps, active: make(map[string]record), pending: make(map[string]int)}
	return func(ctx context.Context, spec sandbox.SandboxSpec) (*types.ToolResult, error) {
		out, err := tool.execute(ctx, spec.Input, spec.TaintLevel)
		if err != nil {
			return &types.ToolResult{Success: false, Error: err.Error(), TaintLevel: types.TaintHigh}, err
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "project_preview: encode result", err)
		}
		return &types.ToolResult{Success: true, Output: encoded, TaintLevel: types.TaintHigh}, nil
	}
}

func (p *previewTool) execute(ctx context.Context, input []byte, taint types.TaintLevel) (*response, error) {
	args, session, root, err := p.validateRequest(ctx, input)
	if err != nil {
		return nil, err
	}
	switch args.Action {
	case "start":
		return p.start(ctx, session, root, args.Timeout, taint)
	case "status":
		return p.status(ctx, session, root, args.JobID, taint)
	default:
		return p.stop(ctx, session, root, args.JobID, taint)
	}
}

func (p *previewTool) validateRequest(ctx context.Context, input []byte) (arguments, string, string, error) {
	var args arguments
	if err := json.Unmarshal(input, &args); err != nil {
		return arguments{}, "", "", apperr.Wrap(apperr.CodeInvalidInput, "project_preview: invalid input", err)
	}
	if args.Action != "start" && args.Action != "status" && args.Action != "stop" {
		return arguments{}, "", "", apperr.New(apperr.CodeInvalidInput, "project_preview: unsupported action")
	}
	if p.deps.Registry == nil || p.deps.Store == nil || p.deps.JobManager == nil || p.deps.SafeHTTPClient.Client == nil || !p.deps.SafeHTTPClient.IsSafe() {
		return arguments{}, "", "", apperr.New(apperr.CodeServiceUnavailable, "project_preview: required dependency unavailable")
	}
	session, _ := ctx.Value(protocol.CtxSessionIDKey{}).(string)
	if session == "" {
		return arguments{}, "", "", apperr.New(apperr.CodeForbidden, "project_preview: session identity required")
	}
	root, err := validateRoot(ctx, args.Path, p.deps.AllowedPaths)
	if err != nil {
		return arguments{}, "", "", err
	}
	if args.Timeout <= 0 || args.Timeout > 60 {
		args.Timeout = 20
	}
	return args, session, root, nil
}

func (p *previewTool) start(ctx context.Context, session, root string, timeout int, taint types.TaintLevel) (*response, error) {
	if err := p.pruneFinished(ctx); err != nil {
		return nil, err
	}
	if !p.reserve(session) {
		return nil, apperr.New(apperr.CodeResourceExhausted, "project_preview: active preview limit reached")
	}
	defer p.releaseReservation(session)
	launcher, ok := detectLauncher(root)
	if !ok {
		return nil, apperr.New(apperr.CodeUnimplemented, "project_preview: no recognized local Vite, Next.js, or static Python launcher")
	}
	port, err := p.allocatePort()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNetworkUnavailable, "project_preview: allocate loopback port", err)
	}
	if !portAvailable(port) {
		return nil, apperr.New(apperr.CodeConflict, "project_preview: selected loopback port became occupied")
	}
	command := "cd " + shellQuote(root) + " && exec " + launcher(port)
	jobID, err := p.launch(ctx, session, root, command, taint)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "project_preview: start command requires separate bash authorization: "+command, err)
	}
	entry := record{SessionID: session, Root: root, JobID: jobID, Port: port, StartedAt: time.Now().UTC()}
	return p.persistAndAwait(ctx, entry, timeout, taint)
}

func (p *previewTool) launch(ctx context.Context, session, root, command string, taint types.TaintLevel) (string, error) {
	args, _ := json.Marshal(map[string]any{"command": command, "background": true, "session_id": session})
	data, err := p.call(nestedContext(ctx, taint), "bash", args, taint)
	if err != nil {
		return "", err
	}
	jobID := stringField(data, "job_id")
	if jobID == "" {
		return "", apperr.New(apperr.CodeInternal, "project_preview: bash did not return a job id")
	}
	meta, err := p.deps.JobManager.GetJob(jobID)
	if err != nil || meta.SessionID != session || guard.CheckAllowedPath(meta.WorkDir, guard.ScopedPaths(ctx, p.deps.AllowedPaths)) != nil {
		if err == nil && meta.SessionID == session {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			if killErr := p.kill(cleanupCtx, session, jobID, taint); killErr != nil {
				return "", apperr.Wrap(apperr.CodeInternal, "project_preview: invalid job metadata and cleanup failed; job_id="+jobID, killErr)
			}
		}
		return "", apperr.New(apperr.CodeForbidden, "project_preview: bash job identity or work directory mismatch")
	}
	return jobID, nil
}

func (p *previewTool) persistAndAwait(ctx context.Context, entry record, timeout int, taint types.TaintLevel) (*response, error) {
	key := storageKey(entry.SessionID, entry.Root, entry.JobID)
	encoded, err := json.Marshal(entry)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "project_preview: encode session binding", err)
	}
	if err := p.deps.Store.Put(ctx, []byte(key), encoded); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if killErr := p.kill(cleanupCtx, entry.SessionID, entry.JobID, taint); killErr != nil {
			p.mu.Lock()
			p.active[entry.JobID] = entry
			p.mu.Unlock()
			return nil, apperr.Wrap(apperr.CodeInternal, "project_preview: binding persistence failed and cleanup failed; job_id="+entry.JobID+"; stop it after granting job_kill", killErr)
		}
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "project_preview: persist session binding", err)
	}
	p.mu.Lock()
	p.active[entry.JobID] = entry
	p.mu.Unlock()
	if err := p.awaitReady(ctx, entry, timeout); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if killErr := p.kill(cleanupCtx, entry.SessionID, entry.JobID, taint); killErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "project_preview: "+err.Error()+"; cleanup failed; job_id="+entry.JobID+"; stop it after granting job_kill", killErr)
		}
		p.mu.Lock()
		delete(p.active, entry.JobID)
		p.mu.Unlock()
		if deleteErr := p.deps.Store.Delete(cleanupCtx, []byte(key)); deleteErr != nil {
			return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "project_preview: readiness failed and binding cleanup failed: "+err.Error(), deleteErr)
		}
		return nil, err
	}
	return &response{Status: "ready", JobID: entry.JobID, URL: fmt.Sprintf("http://127.0.0.1:%d", entry.Port), Host: "守护进程所在主机的 loopback 地址"}, nil
}

func (p *previewTool) status(ctx context.Context, session, root, jobID string, taint types.TaintLevel) (*response, error) {
	entry, err := p.loadBinding(ctx, session, root, jobID)
	if err != nil {
		return nil, err
	}
	if !p.isActive(jobID) {
		return p.inactiveStatus(session, entry)
	}
	meta, err := p.deps.JobManager.GetJob(jobID)
	if err != nil || meta.SessionID != session {
		return nil, apperr.New(apperr.CodeForbidden, "project_preview: job identity mismatch")
	}
	args, _ := json.Marshal(map[string]string{"job_id": jobID})
	data, err := p.call(nestedContext(ctx, taint), "job_status", args, taint)
	if err != nil {
		if apperr.CodeOf(err) == apperr.CodeForbidden {
			return nil, apperr.Wrap(apperr.CodeForbidden, "project_preview: status authorization denied", err)
		}
		return nil, apperr.Wrap(apperr.CodeOf(err), "project_preview: job_status execution failed", err)
	}
	if meta.Status != bash.JobStatusRunning {
		p.mu.Lock()
		delete(p.active, jobID)
		p.mu.Unlock()
		entry.State = string(meta.Status)
		if err := p.saveBinding(ctx, entry); err != nil {
			return nil, err
		}
	}
	return &response{Status: stringField(data, "status"), JobID: entry.JobID, URL: fmt.Sprintf("http://127.0.0.1:%d", entry.Port)}, nil
}

func (p *previewTool) stop(ctx context.Context, session, root, jobID string, taint types.TaintLevel) (*response, error) {
	entry, err := p.loadBinding(ctx, session, root, jobID)
	if err != nil {
		return nil, err
	}
	if !p.isActive(jobID) {
		return p.inactiveStatus(session, entry)
	}
	if err := p.kill(ctx, session, jobID, taint); err != nil {
		return nil, err
	}
	p.mu.Lock()
	delete(p.active, jobID)
	p.mu.Unlock()
	entry.State = string(bash.JobStatusKilled)
	if err := p.saveBinding(ctx, entry); err != nil {
		return nil, err
	}
	return &response{Status: "stopped", JobID: jobID}, nil
}

func (p *previewTool) awaitReady(ctx context.Context, entry record, timeout int) error {
	deadline := time.NewTimer(time.Duration(timeout) * time.Second)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		meta, err := p.deps.JobManager.GetJob(entry.JobID)
		if err != nil || meta.Status != bash.JobStatusRunning {
			return apperr.New(apperr.CodeServiceUnavailable, "project_preview: launcher exited before readiness")
		}
		if p.probe(ctx, entry.Port) {
			meta, err := p.deps.JobManager.GetJob(entry.JobID)
			if err == nil && meta.Status == bash.JobStatusRunning {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return apperr.Wrap(apperr.CodeCancelled, "project_preview: readiness cancelled", ctx.Err())
		case <-deadline.C:
			return apperr.New(apperr.CodeTimeout, "project_preview: readiness timed out")
		case <-ticker.C:
		}
	}
}

func (p *previewTool) probe(ctx context.Context, port int) bool {
	clientCopy := *p.deps.SafeHTTPClient.Client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	if err != nil {
		return false
	}
	resp, err := clientCopy.Do(req)
	if err != nil {
		return false
	}
	if err := resp.Body.Close(); err != nil {
		return false
	}
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (p *previewTool) call(ctx context.Context, name string, args []byte, taint types.TaintLevel) (map[string]any, error) {
	tool, err := p.deps.Registry.Lookup(name)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "project_preview: lookup "+name, err)
	}
	authorizer := protocol.ToolCallAuthorizerFromContext(ctx)
	if authorizer == nil {
		return nil, apperr.New(apperr.CodeForbidden, "project_preview: tool-call authorizer unavailable; request separate tool approval")
	}
	ctx = protocol.WithPermissionGrant(ctx, protocol.GrantNone)
	grant, reason, ok := authorizer.AuthorizeCall(ctx, tool, args, maxTaint(taint, contextTaint(ctx)))
	if !ok || grant == protocol.GrantNone {
		if reason == "" {
			reason = "separate tool approval required"
		}
		return nil, apperr.New(apperr.CodeForbidden, "project_preview: "+reason)
	}
	ctx = protocol.WithPermissionGrant(ctx, grant)
	res, err := p.deps.Registry.ExecuteTool(ctx, name, args, maxTaint(taint, contextTaint(ctx)))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "project_preview: execute "+name, err)
	}
	if res == nil || !res.Success {
		message := "tool returned no result"
		if res != nil {
			message = res.Error
		}
		return nil, apperr.New(apperr.CodeForbidden, "project_preview: "+message)
	}
	var data map[string]any
	if err := json.Unmarshal(res.Output, &data); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "project_preview: decode "+name+" result", err)
	}
	return data, nil
}

func (p *previewTool) kill(ctx context.Context, session, jobID string, taint types.TaintLevel) error {
	meta, err := p.deps.JobManager.GetJob(jobID)
	if err != nil || meta.SessionID != session {
		return apperr.New(apperr.CodeForbidden, "project_preview: job identity mismatch")
	}
	args, _ := json.Marshal(map[string]string{"job_id": jobID})
	if _, err := p.call(nestedContext(ctx, taint), "job_kill", args, taint); err != nil {
		if apperr.CodeOf(err) == apperr.CodeForbidden {
			return apperr.Wrap(apperr.CodeForbidden, "project_preview: stop authorization denied", err)
		}
		return apperr.Wrap(apperr.CodeOf(err), "project_preview: job_kill execution failed", err)
	}
	return nil
}

func (p *previewTool) loadBinding(ctx context.Context, session, root, jobID string) (*record, error) {
	if jobID == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "project_preview: job_id is required")
	}
	value, err := p.deps.Store.Get(ctx, []byte(storageKey(session, root, jobID)))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNotFound, "project_preview: preview binding not found", err)
	}
	var entry record
	if err := json.Unmarshal(value, &entry); err != nil || entry.SessionID != session || entry.Root != root || entry.JobID != jobID || entry.Port < 1 || entry.Port > 65535 || entry.StartedAt.IsZero() {
		return nil, apperr.New(apperr.CodeForbidden, "project_preview: preview binding mismatch")
	}
	return &entry, nil
}

func (p *previewTool) saveBinding(ctx context.Context, entry *record) error {
	value, err := json.Marshal(entry)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "project_preview: encode binding", err)
	}
	if err := p.deps.Store.Put(ctx, []byte(storageKey(entry.SessionID, entry.Root, entry.JobID)), value); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "project_preview: persist binding", err)
	}
	return nil
}

func (p *previewTool) inactiveStatus(session string, entry *record) (*response, error) {
	if entry.State != "" {
		return &response{Status: entry.State, JobID: entry.JobID}, nil
	}
	meta, err := p.deps.JobManager.GetJob(entry.JobID)
	if err == nil && meta.SessionID == session && meta.Status != bash.JobStatusRunning {
		return &response{Status: string(meta.Status), JobID: entry.JobID}, nil
	}
	return &response{Status: "interrupted", JobID: entry.JobID}, nil
}

func (p *previewTool) isActive(jobID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.active[jobID]
	return ok
}

func (p *previewTool) releaseReservation(session string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending[session] <= 1 {
		delete(p.pending, session)
		return
	}
	p.pending[session]--
}

func (p *previewTool) reserve(session string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	activeTotal, activeSession := len(p.active), 0
	for _, entry := range p.active {
		if entry.SessionID == session {
			activeSession++
		}
	}
	if activeTotal+sumPending(p.pending) >= maxActivePreviews || activeSession+p.pending[session] >= maxSessionPreviews {
		return false
	}
	p.pending[session]++
	return true
}

func (p *previewTool) pruneFinished(ctx context.Context) error {
	p.mu.Lock()
	entries := make([]record, 0, len(p.active))
	for _, entry := range p.active {
		entries = append(entries, entry)
	}
	p.mu.Unlock()
	for _, entry := range entries {
		meta, err := p.deps.JobManager.GetJob(entry.JobID)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "project_preview: refresh active preview status", err)
		}
		if meta.Status == bash.JobStatusRunning {
			continue
		}
		p.mu.Lock()
		delete(p.active, entry.JobID)
		p.mu.Unlock()
		if meta.SessionID != entry.SessionID {
			return apperr.New(apperr.CodeForbidden, "project_preview: active job session mismatch")
		}
		entry.State = string(meta.Status)
		if err := p.saveBinding(ctx, &entry); err != nil {
			return err
		}
	}
	return nil
}

func sumPending(pending map[string]int) int {
	total := 0
	for _, count := range pending {
		total += count
	}
	return total
}
func nestedContext(ctx context.Context, taint types.TaintLevel) context.Context {
	ctx = context.WithValue(ctx, protocol.CtxTaintLevelKey{}, maxTaint(taint, contextTaint(ctx)))
	return protocol.WithPermissionGrant(ctx, protocol.GrantNone)
}
func contextTaint(ctx context.Context) types.TaintLevel {
	v, _ := ctx.Value(protocol.CtxTaintLevelKey{}).(types.TaintLevel)
	return v
}
func maxTaint(a, b types.TaintLevel) types.TaintLevel {
	if a > b {
		return a
	}
	return b
}
func stringField(m map[string]any, field string) string { v, _ := m[field].(string); return v }

func validateRoot(ctx context.Context, path string, allowed []string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", apperr.New(apperr.CodeInvalidInput, "project_preview: path must be absolute")
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInvalidInput, "project_preview: project root unavailable", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", apperr.New(apperr.CodeInvalidInput, "project_preview: path must be a project directory")
	}
	checkCtx := ctx
	if err := guard.CheckReadablePath(checkCtx, root, guard.ScopedPaths(checkCtx, allowed)); err != nil {
		return "", apperr.Wrap(apperr.CodeForbidden, "project_preview: project root outside allowed paths", err)
	}
	return root, nil
}

func detectLauncher(root string) (func(int) string, bool) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, false
	}
	root = resolvedRoot
	bin := filepath.Join(root, "node_modules", ".bin")
	if st, err := os.Stat(filepath.Join(bin, "vite")); err == nil && !st.IsDir() {
		if launcher, ok := resolveLauncher(root, filepath.Join(bin, "vite")); ok {
			return func(port int) string {
				return shellQuote(launcher) + " --host 127.0.0.1 --port " + strconv.Itoa(port) + " --strictPort"
			}, true
		}
	}
	if st, err := os.Stat(filepath.Join(bin, "next")); err == nil && !st.IsDir() {
		if launcher, ok := resolveLauncher(root, filepath.Join(bin, "next")); ok {
			return func(port int) string {
				return shellQuote(launcher) + " dev --hostname 127.0.0.1 -p " + strconv.Itoa(port)
			}, true
		}
	}
	if st, err := os.Stat(filepath.Join(root, "index.html")); err == nil && !st.IsDir() {
		return func(port int) string { return staticPythonCommand(port, root) }, true
	}
	return nil, false
}

func resolveLauncher(root, path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !isWithinRoot(root, resolved) {
		return "", false
	}
	return resolved, true
}

func isWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

const staticPythonServer = `import http.server, os, sys, urllib.parse
root = os.path.realpath(sys.argv[2])
class Handler(http.server.SimpleHTTPRequestHandler):
 def translate_path(self, path):
  decoded = urllib.parse.unquote(urllib.parse.urlsplit(path).path)
  candidate = os.path.realpath(os.path.join(root, decoded.lstrip('/')))
  try:
   safe = os.path.commonpath((root, candidate)) == root
  except ValueError:
   safe = False
  if not safe:
   self._polaris_blocked = True
   return root
  return candidate
 def send_head(self):
  self._polaris_blocked = False
  self.translate_path(self.path)
  if self._polaris_blocked:
   self.send_error(403, 'path outside preview root')
   return None
  return super().send_head()
http.server.ThreadingHTTPServer(('127.0.0.1', int(sys.argv[1])), Handler).serve_forever()`

func staticPythonCommand(port int, root string) string {
	return "python3 -c " + shellQuote(staticPythonServer) + " " + strconv.Itoa(port) + " " + shellQuote(root)
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeNetworkUnavailable, "project_preview: reserve loopback port", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func (p *previewTool) allocatePort() (int, error) {
	if p.portAllocator != nil {
		return p.portAllocator()
	}
	return freePort()
}

func portAvailable(port int) bool {
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	if err := listener.Close(); err != nil {
		return false
	}
	return true
}

func storageKey(session, root, jobID string) string {
	h := sha256.Sum256([]byte(session + "\x00" + root + "\x00" + jobID))
	return "project_preview:v1:" + hex.EncodeToString(h[:])
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
