package updater

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

func (m *Manager) applyUpdate(archivePath string) error {
	exePath, err := m.executableFn()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "resolve executable", err)
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	newBinPath := exePath + ".new"
	newLibDir := filepath.Join(filepath.Dir(exePath), "lib.new")
	os.RemoveAll(newLibDir) // 清理可能残留的临时目录
	if err := os.MkdirAll(newLibDir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mkdir failed", err)
	}

	if err := extractFiles(archivePath, newBinPath, newLibDir); err != nil {
		os.Remove(newBinPath)   //nolint:errcheck
		os.RemoveAll(newLibDir) //nolint:errcheck
		os.Remove(archivePath)  //nolint:errcheck
		return apperr.Wrap(apperr.CodeInternal, "extract failed", err)
	}
	os.Remove(archivePath) //nolint:errcheck

	if err := os.Chmod(newBinPath, 0o755); err != nil {
		os.Remove(newBinPath)   //nolint:errcheck
		os.RemoveAll(newLibDir) //nolint:errcheck
		return apperr.Wrap(apperr.CodeInternal, "chmod failed", err)
	}

	targetLibDir := filepath.Join(filepath.Dir(exePath), "lib")

	// 原子替换（Unix 可替换运行中文件；Windows 需延迟脚本）
	errRename := os.Rename(newBinPath, exePath)
	if errRename != nil {
		if runtime.GOOS != "windows" {
			os.Remove(newBinPath)   //nolint:errcheck
			os.RemoveAll(newLibDir) //nolint:errcheck
			return apperr.Wrap(apperr.CodeInternal, "replace failed", errRename)
		}
		if scriptErr := m.writeWindowsUpdateScript(exePath, newBinPath, targetLibDir, newLibDir); scriptErr != nil {
			os.Remove(newBinPath)   //nolint:errcheck
			os.RemoveAll(newLibDir) //nolint:errcheck
			return apperr.Wrap(apperr.CodeInternal, "replace failed (windows)", scriptErr)
		}
		return nil
	}

	return replaceUnixLibs(newLibDir, targetLibDir)
}

func replaceUnixLibs(newLibDir, targetLibDir string) error {
	files, _ := os.ReadDir(newLibDir)
	if err := os.MkdirAll(targetLibDir, 0o755); err != nil {
		slog.Warn("updater: failed to create target lib dir", "err", err)
	}
	for _, f := range files {
		srcFile := filepath.Join(newLibDir, f.Name())
		dstFile := filepath.Join(targetLibDir, f.Name())
		os.Remove(dstFile) //nolint:errcheck // 先移除旧的（如果是 running 的 .so，Remove 只会 unlink inode）
		if err := os.Rename(srcFile, dstFile); err != nil {
			slog.Warn("updater: failed to rename lib", "src", srcFile, "dst", dstFile, "err", err)
		}
	}
	os.RemoveAll(newLibDir) //nolint:errcheck
	return nil
}

// fetchReleaseAsset 按 downloader 候选节点顺序拉取一个 release 附属小文件
// （.sha256 / .sha256.sig），返回内容与"是否取自 GitHub 直连"。
//
// 候选顺序即信任顺序：CandidateURLs 首个元素是原始 GitHub URL，其后才是镜像。
// 上限 1MB——这两类文件都只有几十到几百字节，给足余量即可，防止镜像返回
// 一个巨大响应体把内存打满。
func (m *Manager) fetchReleaseAsset(ctx context.Context, rawURL, label string) (data []byte, fromUpstream bool, err error) {
	var lastErr error
	for i, u := range downloader.CandidateURLs(ctx, m.client, rawURL) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if reqErr != nil {
			lastErr = apperr.Wrap(apperr.CodeInternal, label+" request", reqErr)
			continue
		}
		resp, doErr := m.client.Do(req)
		if doErr != nil {
			lastErr = apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("download %s from %s", label, u), doErr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = apperr.New(apperr.CodeInternal, fmt.Sprintf("%s from %s: HTTP %d", label, u, resp.StatusCode))
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("read %s from %s", label, u), readErr)
			continue
		}
		return body, i == 0, nil
	}
	return nil, false, lastErr
}

// anchorChecksumTrust 确立"校验值本身可信"这一前提，是整条更新链的信任锚点。
//
// # 信任锚点必须独立于"从哪里下载"
//
// 归档可以来自任何地方——它的完整性由校验值承接。真正需要独立锚定的是**校验值
// 本身**：一旦校验值也从镜像取得，信任锚点就整体转移给了镜像运营方，归档与校验值
// 被同一方替换时 SHA-256 比对必然通过，"校验"退化为"自洽性检查"。
//
// 因此校验值必须由以下两个锚点之一支撑，二者皆无则**拒绝安装**：
//
//   - **锚点 A — 发布签名**（强）：<archive>.sha256.sig 经内嵌公钥验签通过。
//     此锚点独立于传输路径，校验值取自任何镜像都安全——镜像没有私钥。
//     这是签名相对纯 checksum 的全部价值所在。
//   - **锚点 B — GitHub 直连 TLS**（弱但可用）：校验值取自 CandidateURLs 首元素
//     （原始 github.com URL）。锚点是 GitHub 的证书与基础设施，虽不如 A 独立
//     （信任面含 CA 体系与 GitHub 自身），但至少不受镜像运营方控制。
//
// # 为什么"两者皆无"必须拒装而非告警放行
//
// 2026-08-11 收紧（此前是 Warn + 放行）：告警放行等于把一个无法证明来源的二进制
// 装进用户机器，而 polaris 装完会自我替换并重启、且持有 LLM 凭据与工具执行能力。
// 「留了日志」不构成放行理由——没有人在更新成功时读日志。
//
// 代价边界：GitHub 完全不可达（无代理的大陆网络）且签名未开通时，自动更新会被
// 拒绝，用户需手动下载。这不是遗漏而是刻意取舍——该场景下确实无法证明产物来源。
// 开通发布签名即可让这条路径重新可用**且安全**（锚点 A 不依赖能否直连 GitHub），
// 流程见 internal/sysmgr/updater/releasekeys/README.md。
//
// 2026-08-10 注：本函数原注释写的是「checksums.txt 不走 ghproxy 代理：即使镜像
// 被篡改，仍以 GitHub 的校验值为权威」——与代码实际行为（CandidateURLs 含代理
// 节点）直接矛盾。读注释的人会以为信任锚点还在 GitHub，实际上早已可以整体落到
// 镜像上。上述两锚点模型才真正兑现了那句话原本承诺的性质。
func (m *Manager) anchorChecksumTrust(ctx context.Context, version, archiveName string, checksumData []byte, fromUpstream bool) error {
	if len(m.releaseKeys) == 0 {
		metrics.GlobalUpdaterSigningNotProvisionedTotal.Add(1)
		if fromUpstream {
			// 锚点 B 成立：校验值取自 github.com 直连，未经镜像之手。
			slog.Warn("updater: 发布签名尚未开通，本次信任锚点为 GitHub 直连 TLS（弱于发布签名）",
				"archive", archiveName, "version", version,
				"howto", "internal/sysmgr/updater/releasekeys/README.md")
			return nil
		}
		// 两个锚点都不成立 —— 无法证明该产物来自官方，拒绝安装。
		metrics.GlobalUpdaterWeakTrustVerifyTotal.Add(1)
		slog.Error("updater: 拒绝安装——无可用信任锚点",
			"archive", archiveName, "version", version)
		return apperr.New(apperr.CodeInternal,
			"updater: 拒绝安装 "+archiveName+"：校验值只能取自镜像（GitHub 直连不可达），"+
				"而本二进制未内嵌发布公钥，无法证明该产物来自官方。"+
				"镜像同时提供归档与校验值时，SHA-256 比对必然通过，起不到防篡改作用。\n"+
				"处置：(a) 让维护者开通发布签名（见 internal/sysmgr/updater/releasekeys/README.md），"+
				"开通后经镜像更新亦安全；(b) 或从 GitHub Releases 手动下载并自行核对校验值。")
	}

	sigURL := fmt.Sprintf(
		"https://github.com/%s/%s/releases/download/%s/%s.sha256.sig",
		repoOwner, repoName, version, archiveName,
	)
	sigData, _, err := m.fetchReleaseAsset(ctx, sigURL, archiveName+".sha256.sig")
	if err != nil {
		// 签名已开通却取不到签名文件：可能是产物缺失（流水线出问题）、也可能是
		// 中间人剥离了 .sig 想把客户端降级回纯 checksum 模式。两种都必须拒绝，
		// 否则"开通签名"这件事可以被网络侧单方面撤销（signature stripping）。
		metrics.GlobalUpdaterSignatureRejectedTotal.Add(1)
		return apperr.Wrap(apperr.CodeInternal,
			"updater: 发布签名已开通但取不到 "+archiveName+".sha256.sig，拒绝安装"+
				"（可能是发布流水线未产出签名，或签名在传输中被剥离）", err)
	}
	if err := verifyWithKeys(m.releaseKeys, checksumData, string(sigData)); err != nil {
		metrics.GlobalUpdaterSignatureRejectedTotal.Add(1)
		return apperr.Wrap(apperr.CodeInternal, "updater: "+archiveName+".sha256 签名验证失败，拒绝安装", err)
	}
	slog.Info("updater: 校验值签名验证通过，信任锚点为内嵌发布公钥（取自镜像亦安全）",
		"archive", archiveName, "version", version, "from_upstream", fromUpstream)
	metrics.GlobalUpdaterSignatureVerifiedTotal.Add(1)
	return nil
}

// verifyChecksum 下载 <archive>.sha256（及其签名）并校验 archivePath 的 SHA-256。
// 信任锚点的建立见 anchorChecksumTrust。
func (m *Manager) verifyChecksum(ctx context.Context, version, archiveName, archivePath string) error {
	if m.client == nil {
		return apperr.New(apperr.CodeInternal, "updater: safe http client not injected")
	}

	checksumURL := fmt.Sprintf(
		"https://github.com/%s/%s/releases/download/%s/%s.sha256",
		repoOwner, repoName, version, archiveName,
	)
	data, fromUpstream, err := m.fetchReleaseAsset(ctx, checksumURL, archiveName+".sha256")
	if err != nil {
		return err
	}

	if err := m.anchorChecksumTrust(ctx, version, archiveName, data, fromUpstream); err != nil {
		return err
	}

	// 格式：<sha256hex>  <filename> (单行文件)
	var expectedHash []byte
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 && len(fields[0]) == 64 {
			var errDecode error
			expectedHash, errDecode = hex.DecodeString(fields[0])
			if errDecode != nil {
				return apperr.Wrap(apperr.CodeInternal, "invalid checksum hex", errDecode)
			}
			break
		}
	}
	if expectedHash == nil {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("valid SHA-256 hash not found in %s.sha256", archiveName))
	}

	// 计算已下载归档的 SHA-256
	f, err := os.Open(archivePath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "open archive for hash", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "hash archive", err)
	}
	actualHash := h.Sum(nil)

	// 恒定时间比较，防御时序攻击
	if !hmac.Equal(actualHash, expectedHash) {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("SHA-256 mismatch: expected %x, got %x", expectedHash, actualHash))
	}

	slog.Info("updater: SHA-256 verified", "archive", archiveName)
	return nil
}

// extractFiles 从归档中提取 polaris 二进制和 lib 目录。
func extractFiles(archivePath, destBinPath, destLibDir string) error {
	binaryNames := map[string]bool{"polaris": true, "polaris.exe": true}
	mapper := func(name string) (string, bool) {
		nameStr := filepath.ToSlash(filepath.Clean(name))
		parts := strings.Split(nameStr, "/")
		if binaryNames[filepath.Base(nameStr)] && len(parts) <= 2 {
			// 允许根目录或一个顶层目录下的二进制文件
			return destBinPath, true
		}
		if len(parts) >= 2 && parts[len(parts)-2] == "lib" {
			// 允许 lib 目录下的文件
			return filepath.Join(destLibDir, filepath.Base(nameStr)), true
		}
		return "", false
	}

	if strings.HasSuffix(archivePath, ".zip") {
		if err := downloader.ExtractZip(archivePath, filepath.Dir(destBinPath), mapper); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "extract zip", err)
		}
		return nil
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "open archive for extract", err)
	}
	defer f.Close()
	if err := downloader.ExtractTarGz(f, filepath.Dir(destBinPath), mapper); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "extract tar.gz", err)
	}
	return nil
}

func (m *Manager) defaultRestart(exePath string) {
	slog.Info("updater: exiting for service manager restart", "path", exePath)
	m.exitFn(0)
}

func (m *Manager) writeWindowsUpdateScript(exePath, newBinPath, targetLibDir, newLibDir string) error {
	script := fmt.Sprintf(`@echo off
timeout /t 2 /nobreak >nul
move /Y "%s" "%s"
if exist "%s" (
    xcopy /Y /E /Q "%s\*" "%s\"
    rmdir /S /Q "%s"
)
start "" "%s"
del "%%~f0"
`, newBinPath, exePath, newLibDir, newLibDir, targetLibDir, newLibDir, exePath)
	scriptPath := exePath + ".update.bat"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "write windows update script", err)
	}
	concurrent.SafeGo(context.Background(), "sysmgr.updater.windows_delayed_exit", func(context.Context) {
		time.Sleep(200 * time.Millisecond)
		m.exitFn(0)
	})
	return nil
}

func semverCompare(a, b string) int {
	parse := func(s string) ([3]int, string) {
		pre := ""
		if i := strings.IndexAny(s, "-+"); i >= 0 {
			pre = s[i:]
			s = s[:i]
		}
		parts := strings.SplitN(s, ".", 3)
		var n [3]int
		for i, p := range parts {
			if i >= 3 {
				break
			}
			v, _ := strconv.Atoi(p)
			n[i] = v
		}
		return n, pre
	}
	va, prea := parse(a)
	vb, preb := parse(b)
	for i := range va {
		if va[i] < vb[i] {
			return -1
		}
		if va[i] > vb[i] {
			return 1
		}
	}

	if prea == preb {
		return 0
	}
	if prea == "" { // a has no pre, so a is greater
		return 1
	}
	if preb == "" { // b has no pre, so b is greater
		return -1
	}
	// Both have pre, simple string comparison
	if prea < preb {
		return -1
	}
	return 1
}
