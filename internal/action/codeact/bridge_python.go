package codeact

import (
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// PythonPolarisModule 是注入到 Python 解释器环境的 polaris 客户端源码（ADR-0115 WP5，R2-15 携带 Token 鉴权）。
const PythonPolarisModule = `import os, sys, socket, json

class _PolarisClient:
    def __init__(self):
        self._sock_path = os.environ.get("POLARIS_BRIDGE_SOCKET", "")
        self._token = os.environ.get("POLARIS_BRIDGE_TOKEN", "")

    def _send(self, req):
        if not self._sock_path:
            raise RuntimeError("POLARIS_BRIDGE_SOCKET environment variable not set")
        if self._token:
            req["token"] = self._token
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self._sock_path)
        try:
            payload = json.dumps(req) + "\n"
            s.sendall(payload.encode("utf-8"))
            buf = b""
            while b"\n" not in buf:
                chunk = s.recv(4096)
                if not chunk:
                    break
                buf += chunk
            line = buf.split(b"\n")[0].decode("utf-8")
            return json.loads(line)
        finally:
            s.close()

    def call(self, tool_name, **kwargs):
        resp = self._send({"action": "call", "tool": tool_name, "args": kwargs})
        if not resp.get("ok"):
            raise RuntimeError(resp.get("error", f"tool {tool_name} failed"))
        return resp.get("result")

    def call_many(self, calls):
        normalized = []
        for c in calls:
            if isinstance(c, dict):
                normalized.append({"tool": c.get("tool", ""), "args": c.get("args", {})})
            elif isinstance(c, (list, tuple)) and len(c) == 2:
                normalized.append({"tool": c[0], "args": c[1]})
        resp = self._send({"action": "call_many", "calls": normalized})
        if not resp.get("ok"):
            raise RuntimeError(resp.get("error", "call_many failed"))
        return resp.get("results")

    def tools(self, query=None):
        resp = self._send({"action": "tools"})
        if not resp.get("ok"):
            raise RuntimeError(resp.get("error", "list tools failed"))
        found = resp.get("tools") or []
        if query:
            q = str(query).lower()
            found = [t for t in found if q in t.get("name", "").lower() or q in t.get("description", "").lower()]
        return found

_client = _PolarisClient()
call = _client.call
call_many = _client.call_many
tools = _client.tools
`

// EnsurePolarisPythonModule 确保指定目录下存在 polaris.py 模块。
func EnsurePolarisPythonModule(dir string) error {
	path := filepath.Join(dir, "polaris.py")
	if err := os.WriteFile(path, []byte(PythonPolarisModule), 0o600); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ensure polaris module", err)
	}
	return nil
}
