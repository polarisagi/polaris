package skill

import (
	"os"
	"path/filepath"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// FilesystemWasmLoader 从本地文件系统加载 Wasm 字节码。
// 约定路径: {baseDir}/{skillID}.wasm（skillID 去掉 "skill:" 前缀）。
// 适用场景: 开发调试、已安装到本地路径的技能（extensions/skill/marketplace/{ext_id}/impl.wasm）。
type FilesystemWasmLoader struct {
	baseDir string
}

// NewFilesystemWasmLoader 构造文件系统 Wasm 加载器。
// baseDir 默认为 "skills/builtin"。
func NewFilesystemWasmLoader(baseDir string) *FilesystemWasmLoader {
	if baseDir == "" {
		baseDir = "skills/builtin"
	}
	return &FilesystemWasmLoader{baseDir: baseDir}
}

// LoadWasm 读取 {baseDir}/{skillID}.wasm 文件。
func (l *FilesystemWasmLoader) LoadWasm(skillID string) ([]byte, error) {
	// 去掉 "skill:" 前缀（注册约定）
	name := skillID
	if len(name) > 6 && name[:6] == "skill:" {
		name = name[6:]
	}
	path := filepath.Join(l.baseDir, name+".wasm")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "wasm_loader: read file "+path, err)
	}
	return data, nil
}
