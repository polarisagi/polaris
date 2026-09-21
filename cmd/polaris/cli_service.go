// `polaris service` —— 把守护进程注册给系统的服务管理器，开机自启、异常重启。
//
// ADR: docs/arch/decisions/ADR-0096-desktop-shell-and-daemon-client-split.md 决策一
// （守护进程的生命周期独立于任何 GUI）。无桌面的服务器部署同样受益。
//
// 三平台都用**用户级**注册（launchd LaunchAgent / systemd --user / 计划任务），
// 不用系统级：数据目录在用户 home 下，以 root 跑会把文件属主弄成 root，随后用户
// 直接运行 polaris 会得到一串权限错误，而错误信息不会提到"你之前装过服务"。
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/polarisagi/polaris/internal/runtimeinfo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// serviceLabel 沿用 scripts/install.sh 自 2026 年起使用的标签，不另起新名：
// 换标签会让已装机器上同时存在两个注册项，两个实例同时起来时后者被单实例锁拒绝，
// 而用户看到的只是"服务时好时坏"。
const serviceLabel = "com.polarisagi.polaris"

// windowsTaskName 是 Windows 计划任务名。与 serviceLabel 不同：scripts/install.ps1
// 自 2026 年起用的就是这个名字，换名同样会留下两个注册项（理由见上）。
const windowsTaskName = "PolarisAGI-Polaris"

func runServiceCmd(args []string) error {
	if len(args) == 0 {
		printServiceHelp()
		return nil
	}
	switch args[0] {
	case "install":
		return serviceInstall()
	case "uninstall":
		return serviceUninstall()
	case "status":
		if len(args) > 1 && args[1] == "--json" {
			return serviceStatusJSON()
		}
		return serviceStatus()
	default:
		printServiceHelp()
		return apperr.New(apperr.CodeInvalidInput, "未知子命令: "+args[0])
	}
}

func printServiceHelp() {
	fmt.Println()
	fmt.Println(clr(ansiBold, "polaris service") + " — 守护进程的系统服务注册")
	fmt.Println()
	fmt.Println("  install     注册为用户级服务并立即启动（开机自启）")
	fmt.Println("  uninstall   停止并移除注册")
	fmt.Println("  status      显示注册状态与运行状态（--json 输出机器可读结果，供桌面外壳启动判定）")
	fmt.Println()
}

// servicePATH 返回写进服务单元的 PATH。
//
// 取**安装时所在交互式 shell 的 PATH**，而不是在这里重新拼一串猜测的目录：
// launchd / systemd 启动的进程拿到的是最小 PATH，而 polaris 会外派子进程
// （MCP 服务器常用 npx、插件可能调 python/node）。旧版 install.sh 为此手工拼了
// homebrew + cargo + nvm current 一串路径——那串会随用户环境变化而失真，
// 而失真的表现是"某些扩展在前台跑得好好的，装成服务就用不了"。
// 用户此刻的 PATH 就是那个已经能跑通的环境，直接固化它最准。
func servicePATH(installDir string) string {
	p := os.Getenv("PATH")
	if p == "" {
		p = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	// 二进制自身所在目录必须在内：服务里 `polaris` 调自己或同目录工具时要能找到。
	if installDir != "" && !strings.Contains(p, installDir) {
		p = installDir + ":" + p
	}
	return p
}

// serviceExecPath 返回当前二进制的绝对路径，并解析符号链接。
// 服务单元里必须写实际路径：写一个 symlink，等 Homebrew/安装脚本换了指向，
// 服务会静默启动到另一个版本上。
func serviceExecPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "service: 无法确定自身路径", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe, nil //nolint:nilerr // 解析失败时退回原路径，不阻断安装
	}
	return resolved, nil
}

func serviceInstall() error {
	exe, err := serviceExecPath()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return launchdInstall(exe)
	case "linux":
		return systemdInstall(exe)
	case "windows":
		return schtasksInstall(exe)
	default:
		return apperr.New(apperr.CodeUnimplemented, "service: 不支持的平台 "+runtime.GOOS)
	}
}

func serviceUninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return launchdUninstall()
	case "linux":
		return systemdUninstall()
	case "windows":
		return schtasksUninstall()
	default:
		return apperr.New(apperr.CodeUnimplemented, "service: 不支持的平台 "+runtime.GOOS)
	}
}

// serviceStatus 分别报告"注册状态"与"运行状态"，二者是独立的两件事：
// 注册了但没跑、跑着但没注册（手工启动）都是常见且合法的状态，合成一个
// "是否正常"会把用户引向错误的处置。
// serviceRegistered 判定是否已注册，并返回单元文件路径（Windows 为任务名）。
func serviceRegistered() (bool, string) {
	unit, err := serviceUnitPath()
	if err != nil {
		return false, ""
	}
	if runtime.GOOS == "windows" {
		// 查询成功即已注册；schtasks 对不存在的任务返回非零。
		return runTool("schtasks", "/query", "/tn", windowsTaskName) == nil, unit
	}
	_, statErr := os.Stat(unit)
	return statErr == nil, unit
}

func serviceStatus() error {
	ok, unit := serviceRegistered()
	registered := "未注册"
	if ok {
		registered = "已注册 → " + unit
	}
	fmt.Printf("%s  服务注册  %s\n", clr(ansiAccent, "●"), registered)

	layout, err := cliDataLayout()
	if err != nil {
		return err
	}
	alive := probeInstanceLock(layout.RunLock)
	st, err := runtimeinfo.Read(runtimeinfo.Paths{PID: layout.RunPID, Port: layout.RunPort, Token: layout.RunToken})
	switch {
	case !alive && err == nil:
		fmt.Printf("%s  守护进程  未运行（run/ 有上次异常退出的残留文件，下次启动会自动覆盖）\n", clr(ansiWarn, "●"))
	case !alive:
		fmt.Printf("%s  守护进程  未运行\n", clr(ansiWarn, "●"))
	case apperr.IsCode(err, apperr.CodeNotFound):
		fmt.Printf("%s  守护进程  正在启动（已持锁，尚未发布端口）\n", clr(ansiAccent, "●"))
	case err != nil:
		fmt.Printf("%s  守护进程  运行时状态异常：%v\n", clr(ansiError, "●"), err)
	default:
		fmt.Printf("%s  守护进程  运行中  pid=%d  port=%d\n", clr(ansiOk, "●"), st.PID, st.Port)
	}
	return nil
}

// serviceUnitPath 返回本平台服务单元文件的路径。
func serviceUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "service: 无法确定用户目录", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", "polaris.service"), nil
	case "windows":
		// 计划任务没有单元文件，返回任务名本身。注册与否由 serviceRegistered
		// 走 schtasks /query 判定，不靠文件是否存在——此前这里返回一个永远不
		// 存在的占位路径，于是 Windows 上的状态恒为「未注册」。
		return windowsTaskName, nil
	default:
		return "", apperr.New(apperr.CodeUnimplemented, "service: 不支持的平台 "+runtime.GOOS)
	}
}

// writeUnit 写入单元文件（0600，父目录自动创建）。
func writeUnit(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "service: 创建目录失败 "+filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "service: 写入单元文件失败 "+path, err)
	}
	return nil
}

// runToolBestEffort 执行清理类命令：预清理（先 unload 再 load、先 stop 再 delete）
// 在"目标本就不存在"时必然失败，那不是错误，故不中断流程。但失败必须留痕——
// 否则真正的失败（如权限不足导致旧单元卸不掉，新单元加载到一半）与正常情况在
// 输出上一模一样（HE-1）。
func runToolBestEffort(name string, args ...string) {
	if err := runTool(name, args...); err != nil {
		slog.Debug("service: 预清理命令失败（通常是目标本就不存在）",
			"cmd", name, "args", strings.Join(args, " "), "err", err)
	}
}

// runTool 执行系统服务管理命令，把 stderr 一并带回错误信息。
// 只打印"命令失败"而丢掉 stderr 的话，用户拿到的就是一个无法自查的退出码。
func runTool(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("service: %s %s 失败: %s", name, strings.Join(args, " "), strings.TrimSpace(string(out))), err)
	}
	return nil
}

// ── macOS: launchd LaunchAgent ───────────────────────────────────────────────

func launchdInstall(exe string) error {
	unit, err := serviceUnitPath()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, ".polarisagi", "polaris", "logs")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>serve</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s/service.out.log</string>
  <key>StandardErrorPath</key><string>%s/service.err.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>%s</string>
    <key>PATH</key><string>%s</string>
  </dict>
</dict>
</plist>
`, serviceLabel, exe, logDir, logDir, home, servicePATH(filepath.Dir(exe)))

	if err = os.MkdirAll(logDir, 0o700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "service: 创建日志目录失败", err)
	}
	if err = writeUnit(unit, plist); err != nil {
		return err
	}
	// 先 unload 再 load：重复安装时 load 会因"已加载"报错，而那不是失败。
	runToolBestEffort("launchctl", "unload", unit)
	if err = runTool("launchctl", "load", unit); err != nil {
		return err
	}
	fmt.Printf("%s 已注册并启动：%s\n", clr(ansiOk, "✓"), unit)
	return nil
}

func launchdUninstall() error {
	unit, err := serviceUnitPath()
	if err != nil {
		return err
	}
	runToolBestEffort("launchctl", "unload", unit)
	if err = os.Remove(unit); err != nil && !os.IsNotExist(err) {
		return apperr.Wrap(apperr.CodeInternal, "service: 删除单元文件失败 "+unit, err)
	}
	fmt.Printf("%s 已移除注册\n", clr(ansiOk, "✓"))
	return nil
}

// ── Linux: systemd user unit ─────────────────────────────────────────────────

func systemdInstall(exe string) error {
	unit, err := serviceUnitPath()
	if err != nil {
		return err
	}
	content := fmt.Sprintf(`[Unit]
Description=Polaris AI Agent
After=network-online.target

[Service]
Type=simple
ExecStart=%s serve
Environment=PATH=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, exe, servicePATH(filepath.Dir(exe)))
	if err = writeUnit(unit, content); err != nil {
		return err
	}
	if err = runTool("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err = runTool("systemctl", "--user", "enable", "--now", "polaris.service"); err != nil {
		return err
	}
	// 用户级 systemd 默认在最后一个会话退出时随之停止；linger 才能真正开机自启。
	// 失败不阻断安装（某些发行版/容器无 loginctl），但必须提示，否则用户会以为
	// 已经配好，直到重启后发现服务没起来。
	if lingerErr := runTool("loginctl", "enable-linger", os.Getenv("USER")); lingerErr != nil {
		fmt.Printf("%s 已注册并启动，但 enable-linger 失败：注销后服务会停止。手动执行：loginctl enable-linger $USER\n", clr(ansiWarn, "!"))
	} else {
		fmt.Printf("%s 已注册并启动：%s\n", clr(ansiOk, "✓"), unit)
	}
	return nil
}

func systemdUninstall() error {
	unit, err := serviceUnitPath()
	if err != nil {
		return err
	}
	runToolBestEffort("systemctl", "--user", "disable", "--now", "polaris.service")
	if err = os.Remove(unit); err != nil && !os.IsNotExist(err) {
		return apperr.Wrap(apperr.CodeInternal, "service: 删除单元文件失败 "+unit, err)
	}
	runToolBestEffort("systemctl", "--user", "daemon-reload")
	fmt.Printf("%s 已移除注册\n", clr(ansiOk, "✓"))
	return nil
}

// ── Windows: 计划任务（登录时触发）──────────────────────────────────────────

func schtasksInstall(exe string) error {
	// /rl LIMITED：保持普通用户权限，与"数据目录在用户 home 下"一致（见文件头）。
	if err := runTool("schtasks", "/create", "/f", "/tn", windowsTaskName,
		"/tr", `"`+exe+`" serve`, "/sc", "onlogon", "/rl", "LIMITED"); err != nil {
		return err
	}
	if err := runTool("schtasks", "/run", "/tn", windowsTaskName); err != nil {
		return err
	}
	fmt.Printf("%s 已注册计划任务并启动：%s\n", clr(ansiOk, "✓"), windowsTaskName)
	return nil
}

func schtasksUninstall() error {
	runToolBestEffort("schtasks", "/end", "/tn", windowsTaskName)
	if err := runTool("schtasks", "/delete", "/f", "/tn", windowsTaskName); err != nil {
		return err
	}
	fmt.Printf("%s 已移除计划任务\n", clr(ansiOk, "✓"))
	return nil
}

// serviceRuntimeJSON 是 `polaris service status --json` 的输出结构。
//
// 它是**桌面外壳的服务发现入口**（ADR-0096 决策七）：外壳不自己拼 run/ 路径，
// 而是问这个二进制。路径 SSoT 在 Go 侧的 config.DataLayout，Rust 侧再实现一遍
// 必然漂移其一——漂移的表现是"外壳连不上一个正在运行的服务"，且两边各自看起来
// 都对。这也顺带解决了 data_dir 被 config.toml 覆盖时外壳找不到 run/ 的问题。
type serviceRuntimeJSON struct {
	Registered bool   `json:"registered"`
	UnitPath   string `json:"unit_path"`
	Running    bool   `json:"running"`
	PID        int    `json:"pid,omitempty"`
	Port       int    `json:"port,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	Token      string `json:"token,omitempty"`
	DataDir    string `json:"data_dir"`
	BinPath    string `json:"bin_path"`
	LogDir     string `json:"log_dir"`
	// Stale 表示 run/ 下有残留状态文件，但没有进程持有单实例锁——上次异常退出
	// （kill -9、断电）没来得及清理。客户端应按"未运行"处理，直接拉起即可：
	// 守护进程启动时会原子覆盖这些文件。
	Stale bool `json:"stale,omitempty"`
	// Problem 记录"有状态文件但读不出来"的原因（权限过宽、内容损坏）。
	// 与 Running=false 是两回事：后者是没在跑，前者是在跑但拿不到凭证——
	// 外壳对这两种情况的处置不同（拉起 vs 报凭证错误，不得重复拉起）。
	Problem string `json:"problem,omitempty"`
}

// serviceStatusJSON 输出机器可读的运行时状态。
func serviceStatusJSON() error {
	layout, err := cliDataLayout()
	if err != nil {
		return err
	}
	out := serviceRuntimeJSON{
		DataDir: layout.Root,
		BinPath: filepath.Join(layout.Bin, "polaris"+execSuffix()),
		LogDir:  layout.Logs,
	}
	out.Registered, out.UnitPath = serviceRegistered()

	// 判活以单实例锁为准，不以文件存在为准：kill -9 之后 run/ 文件会残留，
	// 按文件判活会把死进程报成"运行中"，客户端随后连向没人监听的端口。
	alive := probeInstanceLock(layout.RunLock)

	st, rerr := runtimeinfo.Read(runtimeinfo.Paths{PID: layout.RunPID, Port: layout.RunPort, Token: layout.RunToken})
	switch {
	case !alive:
		// 没人持锁：无论文件在不在，都是未运行。文件在就标记陈旧。
		out.Stale = rerr == nil
	case rerr == nil:
		out.Running = true
		out.PID, out.Port, out.Token = st.PID, st.Port, st.Token
		out.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", st.Port)
	case apperr.IsCode(rerr, apperr.CodeNotFound):
		// 持锁但还没发布端口：守护进程正在启动（取锁在 §4.05，发布在 §11.9，
		// 中间是整套装配）。报 running 且无端口，客户端据此等待而非拉起。
		out.Running = true
	default:
		out.Problem = rerr.Error()
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "service: 输出 JSON 失败", err)
	}
	return nil
}

// execSuffix 返回本平台可执行文件后缀。
func execSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
