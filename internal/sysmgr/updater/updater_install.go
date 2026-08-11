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
// 两种模式，取决于发布签名是否已开通（internal/sysmgr/updater/releasekeys/ 是否
// 有公钥）：
//
//   - **已开通（fail-closed）**：必须取到 <archive>.sha256.sig 且用内嵌公钥验签通过。
//     验签成立后校验值来自哪个节点就无关紧要了——镜像伪造不出签名。这正是签名
//     相对纯 checksum 的全部价值所在，也是本函数存在的理由。
//   - **未开通**：退回纯 checksum。此时校验值取自镜像意味着信任锚点已转移到镜像
//     运营方（归档大概率走同一镜像，两者被同一方替换时 SHA-256 比对必然通过），
//     SHA-256 从「防篡改」退化为「防传输损坏」。不阻断更新——GitHub 完全不可达的
//     环境不降级等于无法升级——但必须留 Error 级痕迹 + 指标，让"本次更新在弱信任
//     模式下完成"可被审计。
//
// 2026-08-10 注：本函数原先只有第二种模式，且注释写的是「checksums.txt 不走
// ghproxy 代理：即使镜像被篡改，仍以 GitHub 的校验值为权威」——与代码实际行为
// （CandidateURLs 含代理节点）直接矛盾。读注释的人会以为信任锚点还在 GitHub，
// 实际上早已可以整体落到镜像上。签名开通后该矛盾才真正消解。
func (m *Manager) anchorChecksumTrust(ctx context.Context, version, archiveName string, checksumData []byte, fromUpstream bool) error {
	if len(m.releaseKeys) == 0 {
		slog.Warn("updater: 发布签名尚未开通（内嵌信任根为空），本次仅做 SHA-256 校验",
			"archive", archiveName, "version", version,
			"howto", "internal/sysmgr/updater/releasekeys/README.md")
		metrics.GlobalUpdaterSigningNotProvisionedTotal.Add(1)
		if !fromUpstream {
			slog.Error("updater: 校验值取自镜像而非 GitHub 直连，本次更新处于弱信任模式"+
				"（镜像若被污染，SHA-256 校验无法发现替换）",
				"archive", archiveName, "version", version)
			metrics.GlobalUpdaterWeakTrustVerifyTotal.Add(1)
		}
		return nil
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
