package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"unicode/utf16"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const windowsRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

// HKCU 只属于当前登录用户，不能借任务调度器的运行级别推断创建权限。
func windowsServiceRegistration(run func(string, ...string) error) (bool, string) {
	if windowsRunRegistered(run) {
		return true, windowsRunKey + `\` + windowsTaskName
	}
	return windowsTaskRegistered(run), windowsTaskName
}

func windowsRunRegistered(run func(string, ...string) error) bool {
	return run("reg.exe", "query", windowsRunKey, "/v", windowsTaskName) == nil
}

func windowsTaskRegistered(run func(string, ...string) error) bool {
	return run("schtasks", "/query", "/tn", windowsTaskName) == nil
}

func windowsLoginInstall(scriptPath string, run func(string, ...string) error) error {
	// 已选的普通用户入口保持稳定，重复安装不再尝试创建另一个后端。
	if windowsRunRegistered(run) {
		if err := windowsRemoveTask(run); err != nil {
			return err
		}
		return windowsRunInstall(scriptPath, run)
	}
	command := windowsLoginCommand(scriptPath)
	err := run("schtasks", "/create", "/f", "/tn", windowsTaskName,
		"/tr", command, "/sc", "onlogon", "/rl", "LIMITED")
	if err == nil {
		err = run("schtasks", "/run", "/tn", windowsTaskName)
		if err == nil {
			fmt.Printf("%s 已注册计划任务并启动：%s\n", clr(ansiOk, "✓"), windowsTaskName)
			return nil
		}
	}
	slog.Warn("service: 计划任务不可用，回退当前用户登录自启动", "err", err)
	if cleanupErr := windowsRemoveTask(run); cleanupErr != nil {
		return cleanupErr
	}
	return windowsRunInstall(scriptPath, run)
}

func windowsLoginCommand(scriptPath string) string {
	return `powershell.exe -NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File "` + scriptPath + `"`
}

func windowsRunInstall(scriptPath string, run func(string, ...string) error) error {
	command := windowsLoginCommand(scriptPath)
	// Run 的命令行上限是 260 个 UTF-16 字符，超限不能报告注册成功。
	if len(utf16.Encode([]rune(command))) > 260 {
		return apperr.New(apperr.CodeInvalidInput, "service: 登录自启动命令超过 Windows Run 的 260 字符上限，请使用较短安装路径")
	}
	if err := run("reg.exe", "add", windowsRunKey, "/v", windowsTaskName, "/t", "REG_SZ", "/d", command, "/f"); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "service: 当前用户登录自启动注册失败", err)
	}
	// 外层 PowerShell 只负责分离启动，不等待守护进程退出；内层仍读取唯一启动脚本。
	launch := "Start-Process -FilePath 'powershell.exe' -ArgumentList " + powershellLiteral(strings.TrimPrefix(command, "powershell.exe ")) + " -WindowStyle Hidden -ErrorAction Stop"
	if err := run("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", launch); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "service: 登录自启动已注册，但立即后台启动失败", err)
	}
	fmt.Printf("%s 已注册当前用户登录自启动并启动后台进程，无需管理员权限：%s\n", clr(ansiOk, "✓"), windowsTaskName)
	return nil
}

func powershellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func windowsRemoveTask(run func(string, ...string) error) error {
	if !windowsTaskRegistered(run) {
		return nil
	}
	if err := run("schtasks", "/end", "/tn", windowsTaskName); err != nil {
		slog.Debug("service: 结束计划任务失败（可能尚未运行）", "err", err)
	}
	if err := run("schtasks", "/delete", "/f", "/tn", windowsTaskName); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "service: 无法移除旧计划任务，拒绝创建重复自启入口，请检查当前账户任务权限", err)
	}
	return nil
}

func windowsLoginUninstall(exe string, run func(string, ...string) error) error {
	if windowsTaskRegistered(run) {
		if err := windowsRemoveTask(run); err != nil {
			return err
		}
	}
	if windowsRunRegistered(run) {
		if err := run("reg.exe", "delete", windowsRunKey, "/v", windowsTaskName, "/f"); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "service: 移除当前用户登录自启动失败", err)
		}
	}
	// 同路径筛选且排除本 CLI，避免卸载时终止其他安装目录或自身进程。
	stop := fmt.Sprintf("Get-Process -Name polaris -ErrorAction SilentlyContinue | Where-Object { $_.Id -ne %d -and $_.Path -eq %s } | Stop-Process -Force -ErrorAction Stop", os.Getpid(), powershellLiteral(exe))
	if err := run("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", stop); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "service: 停止当前用户后台进程失败", err)
	}
	return nil
}
