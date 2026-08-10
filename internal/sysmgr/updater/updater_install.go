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

// verifyChecksum 下载 <archive>.sha256 并校验 archivePath 的 SHA-256。
//
// **信任模型（2026-08-10 订正 + 加固）**：本函数**优先直连 GitHub**取校验值；
// 仅当直连完全不可达时才降级到镜像。降级会让校验值与归档可能来自同一个被污染的
// 镜像，此时 SHA-256 只证明"下到的和该镜像宣称的一致"，不再证明"和 GitHub 发布的
// 一致"——校验强度从「防篡改」退化为「防传输损坏」。
//
// 本函数原注释写的是「checksums.txt 不走 ghproxy 代理：即使镜像被篡改，仍以
// GitHub 的校验值为权威」，与代码实际行为（`downloader.CandidateURLs` 含代理节点）
// 直接矛盾。该矛盾是有安全后果的注释漂移：读注释的人会以为供应链信任锚点还在
// GitHub，实际上早已可以整体落到镜像上。保留降级能力（中国大陆完全无法访问
// GitHub 的环境下，不降级等于无法更新），但把降级这件事变成**显式、可观测、
// 会告知用户**的事件，而不是一行与事实相反的注释。
//
// 彻底的解法是非对称签名（cosign/sigstore）——校验值本身带发布者签名，镜像再怎么
// 篡改也伪造不出签名。该项需要 release 流水线接入签名步骤，属独立立项，
// 见 `docs/arch/decisions/ADR-0095-updater-supply-chain-and-schema-downgrade-guard.md`。
//
//nolint:gocyclo
func (m *Manager) verifyChecksum(ctx context.Context, version, archiveName, archivePath string) error {
	checksumURL := fmt.Sprintf(
		"https://github.com/%s/%s/releases/download/%s/%s.sha256",
		repoOwner, repoName, version, archiveName,
	)

	c := m.client
	if c == nil {
		return apperr.New(apperr.CodeInternal, "updater: safe http client not injected")
	}

	var data []byte
	var downloadErr error

	// 候选节点顺序即信任顺序：CandidateURLs 首个元素是原始 GitHub URL（直连），
	// 其后才是镜像。fromUpstream 记录本次校验值最终取自哪一档，供下方告警。
	fromUpstream := false
	for i, u := range downloader.CandidateURLs(ctx, c, checksumURL) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			downloadErr = apperr.Wrap(apperr.CodeInternal, "checksum request", err)
			continue
		}
		resp, err := c.Do(req)
		if err != nil {
			downloadErr = apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("download %s.sha256 from %s", archiveName, u), err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			downloadErr = apperr.New(apperr.CodeInternal, fmt.Sprintf("%s.sha256 from %s: HTTP %d", archiveName, u, resp.StatusCode))
			continue
		}

		data, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 最多 1MB
		resp.Body.Close()
		if err != nil {
			downloadErr = apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("read %s.sha256 from %s", archiveName, u), err)
			continue
		}
		// 成功下载
		downloadErr = nil
		fromUpstream = i == 0
		break
	}

	if downloadErr != nil {
		return downloadErr
	}

	if !fromUpstream {
		// 校验值来自镜像 = 供应链信任锚点已从 GitHub 转移到镜像运营方。
		// 归档大概率也走同一镜像，两者被同一方替换时 SHA-256 比对必然通过。
		// 不阻断更新（否则 GitHub 不可达的环境永远无法升级），但必须留下
		// Error 级痕迹 + 指标，让"这次更新是在弱信任模式下完成的"可被审计。
		slog.Error("updater: 校验值取自镜像而非 GitHub 直连，本次更新处于弱信任模式"+
			"（镜像若被污染，SHA-256 校验无法发现替换）",
			"archive", archiveName, "version", version)
		metrics.GlobalUpdaterWeakTrustVerifyTotal.Add(1)
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
