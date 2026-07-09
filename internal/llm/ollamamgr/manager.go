package ollamamgr

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// EnsureOllama 下载并安装本地独立的 Ollama，或者使用系统已安装的 Ollama。
// httpClient 由调用方注入（应为 safeHTTPClient），使下载流量经过 SafeDialer + GithubProxy，
// 避免绕过 SSRF 过滤（XR-06）并支持代理加速（对中国大陆 GitHub 访问尤为关键）。
func EnsureOllama(ctx context.Context, httpClient *http.Client, binDir string) (string, error) {
	// 首先检查系统是否已经全局安装了 Ollama
	if globalPath := findGlobalOllama(); globalPath != "" {
		slog.Info("polaris: Found global Ollama installation", "path", globalPath)
		return globalPath, nil
	}

	// XR-06 fail-closed：调用方必须注入经过 SafeDialer 封装的 HTTP 客户端，
	// nil 时拒绝执行而非降级到 DefaultClient（会绕过五阶段网络隔离过滤）。
	if httpClient == nil {
		return "", apperr.New(apperr.CodeInternal, "ollamamgr: httpClient is required; use substrate.NewSafeHTTPClient")
	}

	distDir := filepath.Join(binDir, "ollama-dist")
	binName := "ollama"
	if runtime.GOOS == "windows" {
		binName = "ollama.exe"
	}
	binPath := filepath.Join(distDir, binName)
	legacyBinPath := filepath.Join(binDir, binName)

	// 如果文件已存在且有执行权限，直接返回
	if info, err := os.Stat(binPath); err == nil && !info.IsDir() {
		return binPath, nil
	} else if info, err := os.Stat(legacyBinPath); err == nil && !info.IsDir() {
		return legacyBinPath, nil
	}

	slog.Info("polaris: Ollama binary not found locally, starting silent download...", "path", binPath)

	downloadName := getDownloadName()
	url := fmt.Sprintf("https://github.com/ollama/ollama/releases/latest/download/%s", downloadName)

	tmpArchive := filepath.Join(binDir, "ollama-archive.tmp")
	if err := downloader.DownloadFile(ctx, httpClient, url, tmpArchive); err != nil {
		return "", apperr.Wrap(apperr.CodeNetworkUnavailable, "failed to download ollama", err)
	}
	defer os.Remove(tmpArchive)

	if err := extractOllamaArchive(downloadName, tmpArchive, distDir); err != nil {
		return "", err
	}

	binPath = locateOllamaBinary(distDir, binName, binPath)

	slog.Info("polaris: Ollama binary downloaded and installed successfully", "path", binPath)
	return binPath, nil
}

func getDownloadName() string {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	downloadName := fmt.Sprintf("ollama-%s-%s", goos, goarch)
	switch goos {
	case "windows":
		downloadName += ".zip"
	case "darwin":
		downloadName = "ollama-darwin.tgz"
	case "linux":
		downloadName += ".tar.zst"
	}
	return downloadName
}

func extractOllamaArchive(downloadName, tmpArchive, distDir string) error {
	if err := os.MkdirAll(distDir, 0755); err != nil {
		return err
	}

	slog.Info("polaris: Extracting Ollama archive...", "archive", downloadName)
	if strings.HasSuffix(downloadName, ".zip") {
		if err := extractZip(tmpArchive, distDir); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "failed to extract zip", err)
		}
	} else {
		cmd := exec.Command("tar", "-xf", tmpArchive, "-C", distDir)
		if err := cmd.Run(); err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("failed to extract archive %s", downloadName), err)
		}
	}
	return nil
}

func locateOllamaBinary(distDir, binName, defaultBinPath string) string {
	if _, err := os.Stat(defaultBinPath); os.IsNotExist(err) {
		subPath := filepath.Join(distDir, "bin", binName)
		if _, err := os.Stat(subPath); err == nil {
			return subPath
		}

		err = filepath.Walk(distDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && info.Name() == binName {
				defaultBinPath = path
			}
			return nil
		})
		if err != nil {
			slog.Warn("polaris: Error walking directory while locating ollama", "error", err)
		}
	}
	return defaultBinPath
}

// extractZip 解压 zip 文件到目标目录
func extractZip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(dest, f.Name)
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("illegal file path: %s", fpath))
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(fpath, os.ModePerm); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
			return err
		}
		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}
		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// findGlobalOllama 查找系统全局安装的 Ollama
func findGlobalOllama() string {
	// 1. 尝试从 PATH 环境变量中查找
	if p, err := exec.LookPath("ollama"); err == nil {
		if checkOllamaExecutable(p) {
			return p
		}
	}

	// 2. 尝试各个平台的默认安装路径
	var commonPaths []string
	home, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "darwin":
		commonPaths = []string{
			"/usr/local/bin/ollama",
			"/opt/homebrew/bin/ollama",
			"/Applications/Ollama.app/Contents/Resources/ollama",
		}
	case "windows":
		commonPaths = []string{
			filepath.Join(home, "AppData", "Local", "Programs", "Ollama", "ollama.exe"),
			`C:\Program Files\Ollama\ollama.exe`,
		}
	case "linux":
		commonPaths = []string{
			"/usr/local/bin/ollama",
			"/usr/bin/ollama",
			"/bin/ollama",
			"/opt/ollama/bin/ollama",
		}
	}

	for _, p := range commonPaths {
		if checkOllamaExecutable(p) {
			return p
		}
	}

	return ""
}

// checkOllamaExecutable 检查给定的路径是否是一个可用的 ollama 可执行文件
func checkOllamaExecutable(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}

	// 在 Windows 之外的系统上检查执行权限
	if runtime.GOOS != "windows" && info.Mode()&0111 == 0 {
		return false
	}

	// 尝试运行 ollama -v 确认它能正常执行
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, p, "-v")
	if err := cmd.Run(); err != nil {
		return false
	}

	return true
}

// StartOllama 在后台启动 ollama serve
func StartOllama(ctx context.Context, binPath string) (*exec.Cmd, error) {
	slog.Info("polaris: Starting local Ollama engine in background...")
	cmd := exec.CommandContext(ctx, binPath, "serve")

	// 将输出重定向到 devnull 或者丢弃，避免污染主进程日志
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to start ollama", err)
	}

	// 轮询等待端口启动
	client := &http.Client{Timeout: 2 * time.Second}
	ready := false
	for i := 0; i < 30; i++ { // 等待最多 30 秒
		resp, err := client.Get("http://localhost:11434/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(1 * time.Second)
	}

	if !ready {
		// 如果没启动成功，杀死僵尸进程
		_ = cmd.Process.Kill()
		return nil, apperr.New(apperr.CodeTimeout, "ollama failed to become ready on port 11434")
	}

	slog.Info("polaris: Local Ollama engine is ready")
	return cmd, nil
}

// EnsureModel 执行 ollama pull 下载或更新模型
func EnsureModel(ctx context.Context, binPath string, modelName string) error {
	slog.Info("polaris: Pulling embedding model... This might take a while.", "model", modelName)
	cmd := exec.CommandContext(ctx, binPath, "pull", modelName)
	// 可以捕获输出打印进度，但为了静默这里暂时丢弃
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("failed to pull model %s", modelName), err)
	}

	slog.Info("polaris: Model pulled successfully", "model", modelName)
	return nil
}
