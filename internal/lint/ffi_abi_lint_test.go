package lint_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/polarisagi/polaris/internal/ffi"
)

// ─── inv_FFIABIConstantsInSync ───────────────────────────────────────────────
//
// Go 侧 ffi.ExpectedABIMajor/Minor 与 Rust 侧 SUBSTRATE_ABI_MAJOR/MINOR 必须
// 逐字一致。此前二者仅靠人工同步 + 运行期校验：
//   - major 不匹配 → dylib 加载时 panic（进程起不来才发现）
//   - minor 不匹配 → 只 slog.Warn（生产上很可能没人看见）
//
// 也就是说 minor 漂移可以一直带到线上而无人察觉，major 漂移则要等到部署时
// 才炸。这类"两份常量必须相等"的约束是 100% 机械可检的，不该靠人自觉——
// 与 make deadcode / make docs-refs 同一思路，在 CI 里拦住。
//
// 本测试只读 Rust 源码文本（正则提取常量），不需要编译 Rust，
// 因此在没有 cargo 工具链的环境里同样能跑。

var (
	rustABIMajorRe = regexp.MustCompile(`(?m)^const SUBSTRATE_ABI_MAJOR:\s*u16\s*=\s*(\d+)\s*;`)
	rustABIMinorRe = regexp.MustCompile(`(?m)^const SUBSTRATE_ABI_MINOR:\s*u16\s*=\s*(\d+)\s*;`)
)

func Test_inv_FFIABIConstantsInSync(t *testing.T) {
	root := repoRoot(t)
	libPath := filepath.Join(root, "rust", "substrate", "src", "lib.rs")

	src, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatalf("inv_FFIABIConstantsInSync: 读取 %s 失败: %v", libPath, err)
	}

	rustMajor := extractRustABIConst(t, rustABIMajorRe, src, "SUBSTRATE_ABI_MAJOR")
	rustMinor := extractRustABIConst(t, rustABIMinorRe, src, "SUBSTRATE_ABI_MINOR")

	if rustMajor != uint64(ffi.ExpectedABIMajor) {
		t.Errorf("inv_FFIABIConstantsInSync VIOLATED: ABI major 不一致——"+
			"Rust SUBSTRATE_ABI_MAJOR=%d，Go ffi.ExpectedABIMajor=%d。"+
			"major 不匹配会在 dylib 加载时 panic；改动任一侧必须同步另一侧"+
			"（并在 rust/substrate/src/lib.rs 的版本注释中记录变更原因）",
			rustMajor, ffi.ExpectedABIMajor)
	}
	if rustMinor != uint64(ffi.ExpectedABIMinor) {
		t.Errorf("inv_FFIABIConstantsInSync VIOLATED: ABI minor 不一致——"+
			"Rust SUBSTRATE_ABI_MINOR=%d，Go ffi.ExpectedABIMinor=%d。"+
			"minor 不匹配在运行期只 slog.Warn，漂移可以一直带到线上无人察觉，"+
			"因此必须在此拦住",
			rustMinor, ffi.ExpectedABIMinor)
	}
}

func extractRustABIConst(t *testing.T, re *regexp.Regexp, src []byte, name string) uint64 {
	t.Helper()
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("inv_FFIABIConstantsInSync: 在 lib.rs 中未找到 %s 常量定义——"+
			"若该常量被改名或移动，请同步更新本测试的正则", name)
	}
	v, err := strconv.ParseUint(string(m[1]), 10, 16)
	if err != nil {
		t.Fatalf("inv_FFIABIConstantsInSync: 解析 %s 值 %q 失败: %v", name, m[1], err)
	}
	return v
}
