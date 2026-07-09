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

// verifyChecksum 从 GitHub 直接下载 checksums.txt，验证 archivePath 的 SHA-256。
// checksums.txt 不走 ghproxy 代理：即使镜像被篡改，仍以 GitHub 的校验值为权威。
//
//nolint:gocyclo
func (m *Manager) verifyChecksum(ctx context.Context, version, archiveName, archivePath string) error {
	checksumURL := fmt.Sprintf(
		"https://github.com/%s/%s/releases/download/%s/%s.sha256",
		repoOwner, repoName, version, archiveName,
	)

	c := m.client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}

	var data []byte
	var downloadErr error

	// 尝试 downloader 提供的候选节点（直连优先，失败后尝试代理）
	// 这里放宽了严格的不走代理限制：直连 GitHub 会优先尝试，若完全被阻断则降级使用镜像。
	// 这对在中国大陆完全无法访问 GitHub 的环境是必须的。
	for _, u := range downloader.CandidateURLs(ctx, c, checksumURL) {
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
		break
	}

	if downloadErr != nil {
		return downloadErr
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
