package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	_ "embed"

	"github.com/polarisagi/polaris/pkg/apperr"
)

//go:embed kernel_manifest.json
var kernelManifestJSON []byte

// VerifyKernelIntegrity checks the SHA-256 hashes of immutable kernel packages against the embedded manifest.
// If there is a mismatch or a file is missing/added, it returns an error.
func VerifyKernelIntegrity() error {
	var manifest map[string]string
	if err := json.Unmarshal(kernelManifestJSON, &manifest); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to unmarshal kernel manifest", err)
	}

	currentManifest := make(map[string]string)
	releaseMode := false

	for _, dir := range ImmutableKernelPackages() {
		// 如果核心源码目录不存在，说明是作为 Release 发布的独立二进制运行，而非源码运行模式。
		//
		// 注意这里用的是**相对路径**，因此判定结果取决于进程的工作目录：
		// 在仓库根目录跑发布版二进制会被判成源码模式，进而要求内嵌 manifest 与
		// 当前工作树逐字节一致（改过任何被覆盖的 .go 文件就会拒绝启动）。
		// 这对开发者是预期行为（`make generate-manifest` 即可），对终端用户则
		// 需要"恰好在一个含 internal/ 的目录里启动"才会误判，概率极低但非零。
		// 保持现状不改为绝对路径：源码模式本就该跟着工作目录走，改成基于
		// 可执行文件位置反而会让仓库内的开发运行失去校验。
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			// 源码目录不存在，进入 Release 二进制校验模式
			releaseMode = true
			break
		}

		if err := hashPackageDir(dir, currentManifest); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "VerifyKernelIntegrity", err)
		}
	}

	if releaseMode {
		// Release 模式：校验二进制自身哈希
		return verifyBinarySeal()
	}

	// Verify all expected files are present and match
	for path, expectedHash := range manifest {
		actualHash, ok := currentManifest[path]
		if !ok {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("integrity violation: missing immutable kernel file %s", path))
		}
		if actualHash != expectedHash {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("integrity violation: hash mismatch for %s (expected %s, got %s)", path, expectedHash, actualHash))
		}
	}

	// Verify no new unexpected files are present
	for path := range currentManifest {
		if _, ok := manifest[path]; !ok {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("integrity violation: unexpected new file in immutable kernel package %s", path))
		}
	}

	return nil
}

// verifyBinarySeal 计算当前可执行文件的 SHA-256，与附加的 .sha256 封印文件比对。
//
// **本函数当前在生产环境恒为空转**——发布产物不携带 .sha256 旁挂文件，见下方
// os.IsNotExist 分支的说明。保留它是因为它对"检出磁盘损坏"仍有价值；要让它真正
// 生效，需在 release.yml 打包阶段随二进制一并产出 `<binary>.sha256`
// （Makefile `build-release` 目标已有该逻辑，但无人调用）。
//
// 若将来决定开启，注意 OTA 自更新会原地替换二进制：`extractFiles` 必须同步替换
// 旁挂封印，否则下次启动必然 seal mismatch —— 那会把一个"检出损坏"的辅助检查
// 变成一类必然发生的 brick。
func verifyBinarySeal() error {
	exe, err := os.Executable()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "binary seal: cannot resolve executable path", err)
	}
	sidecar := exe + ".sha256"
	data, err := os.ReadFile(sidecar)
	if os.IsNotExist(err) {
		// 无封印文件 → 放行，但**必须说清本次没有校验任何东西**。
		//
		// 2026-08-11 补：此处原本是静默 `return nil`（注释写"打印警告后放行"，
		// 而代码里并没有任何打印）。后果是启动第一道「内核完整性校验 (L4)」在
		// 生产环境恒为空转，且其"通过"与真正校验通过的输出完全一致——正是本仓库
		// 反复吃亏的 ADR-0091 模式。
		//
		// 空转不是偶发：生成旁挂封印的只有 Makefile `build-release` 目标，而
		// release.yml 走的是 `build-backend`，没有任何地方调用 build-release，
		// 所以**发布产物从不携带 .sha256 旁挂文件**，每个用户每次启动都走这条分支。
		//
		// 刻意不改为 fail-closed：旁挂 hash 对"能改二进制的攻击者"毫无约束
		// （他连 sidecar 一起改就行），真实价值只在检出**损坏**（下载残缺、磁盘
		// 错误、OTA 替换中断）。供应链真实性已由发布签名在更新时承担（ADR-0095），
		// 此处拒绝启动只会凭空造出一类 brick。
		slog.Warn("integrity: 未找到二进制封印文件，本次未校验二进制完整性",
			"sidecar", sidecar,
			"note", "供应链真实性由更新时的发布签名验证承担（ADR-0095）；本检查仅用于检出磁盘损坏")
		return nil
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "binary seal: cannot read .sha256 sidecar", err)
	}
	expected := strings.TrimSpace(string(data))

	f, err := os.Open(exe)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "binary seal: cannot open executable", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "binary seal: hash read failed", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"CRITICAL: binary seal mismatch (expected %s, got %s)", expected, actual))
	}
	return nil
}

func hashPackageDir(dir string, currentManifest map[string]string) error {
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "hashPackageDir", err)
		}
		if !info.IsDir() && filepath.Ext(path) == ".go" {
			f, err := os.Open(path)
			if err != nil {
				return apperr.Wrap(apperr.CodeInternal, "hashPackageDir", err)
			}
			defer f.Close()
			h := sha256.New()
			if _, err := io.Copy(h, f); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "hashPackageDir", err)
			}
			currentManifest[path] = hex.EncodeToString(h.Sum(nil))
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("failed to walk immutable package %s", dir), err)
	}
	return nil
}
