package ollamamgr

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// ensureBinDir 确保二进制存放目录存在
func ensureBinDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".polaris", "bin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// EnsureOllama 下载并安装本地独立的 Ollama。
// httpClient 由调用方注入（应为 safeHTTPClient），使下载流量经过 SafeDialer + GithubProxy，
// 避免绕过 SSRF 过滤（XR-06）并支持代理加速（对中国大陆 GitHub 访问尤为关键）。
func EnsureOllama(ctx context.Context, httpClient *http.Client) (string, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	dir, err := ensureBinDir()
	if err != nil {
		return "", err
	}

	binName := "ollama"
	if runtime.GOOS == "windows" {
		binName = "ollama.exe"
	}
	binPath := filepath.Join(dir, binName)

	// 如果文件已存在且有执行权限，直接返回
	if info, err := os.Stat(binPath); err == nil && !info.IsDir() {
		// 基本检查存在即可，可以做更严格的检查
		return binPath, nil
	}

	slog.Info("polaris: Ollama binary not found locally, starting silent download...", "path", binPath)

	// 拼接下载地址 (格式: ollama-linux-amd64 / ollama-darwin)
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	// 注意各平台特殊处理
	downloadName := fmt.Sprintf("ollama-%s-%s", goos, goarch)
	if goos == "windows" {
		downloadName += ".exe"
	} else if goos == "darwin" {
		// macOS 官方只发布一个通用 fat binary（同时支持 amd64 和 arm64），文件名为 ollama-darwin。
		// 不区分 amd64/arm64，否则 arm64 会拼成 ollama-darwin-arm64 导致 404。
		downloadName = "ollama-darwin"
	}

	url := fmt.Sprintf("https://github.com/ollama/ollama/releases/latest/download/%s", downloadName)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// 使用注入的 httpClient（含 SafeDialer + GithubProxy），禁止使用 http.DefaultClient
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("unexpected status code downloading ollama: %d", resp.StatusCode)
	}

	// 先写到临时文件
	tmpPath := binPath + ".tmp"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return "", err
	}

	_, err = io.Copy(out, resp.Body)
	out.Close()
	if err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to write ollama binary: %w", err)
	}

	// 重命名
	if err := os.Rename(tmpPath, binPath); err != nil {
		return "", fmt.Errorf("failed to install ollama binary: %w", err)
	}

	slog.Info("polaris: Ollama binary downloaded and installed successfully", "path", binPath)
	return binPath, nil
}

// StartOllama 在后台启动 ollama serve
func StartOllama(ctx context.Context, binPath string) (*exec.Cmd, error) {
	slog.Info("polaris: Starting local Ollama engine in background...")
	cmd := exec.CommandContext(ctx, binPath, "serve")

	// 将输出重定向到 devnull 或者丢弃，避免污染主进程日志
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start ollama: %w", err)
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
		return nil, fmt.Errorf("ollama failed to become ready on port 11434")
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
		return fmt.Errorf("failed to pull model %s: %w", modelName, err)
	}

	slog.Info("polaris: Model pulled successfully", "model", modelName)
	return nil
}
