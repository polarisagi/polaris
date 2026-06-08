[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$Culture = [System.Globalization.CultureInfo]::InstalledUICulture.Name
$IsZh = ($Culture -match "zh")

function Write-Msg {
    param([string]$zh, [string]$en, [ConsoleColor]$Color = 'White')
    if ($IsZh) { Write-Host $zh -ForegroundColor $Color }
    else        { Write-Host $en -ForegroundColor $Color }
}

$InstallDir = "$env:USERPROFILE\.polarisagi\polaris\bin"
$DataDir    = "$env:USERPROFILE\.polarisagi\polaris"
$TaskName   = "PolarisAGI-Polaris"

Write-Msg -zh "🗑️  正在卸载 PolarisAGI Polaris..." `
           -en "🗑️  Uninstalling PolarisAGI Polaris..." -Color Cyan

# ── 1. 停止进程 ──────────────────────────────────────────────────────────────
$Proc = Get-Process -Name "polaris" -ErrorAction SilentlyContinue
if ($Proc) {
    Write-Msg -zh "🛑 正在停止运行中的进程..." -en "🛑 Stopping running process..." -Color Cyan
    Stop-Process -Name "polaris" -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
}

# ── 2. 移除任务计划 ──────────────────────────────────────────────────────────
Write-Msg -zh "⚙️  移除开机自启配置..." -en "⚙️  Removing startup configuration..." -Color Cyan
Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue | Out-Null

# 同时清理历史遗留的注册表自启
Remove-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" `
    -Name $TaskName -ErrorAction SilentlyContinue

# ── 3. 删除二进制 ────────────────────────────────────────────────────────────
$FinalExe = "$InstallDir\polaris.exe"
if (Test-Path $FinalExe) {
    Write-Msg -zh "🗑️  删除程序文件: $FinalExe" -en "🗑️  Removing binary: $FinalExe" -Color Cyan
    Remove-Item -Path $FinalExe -Force -ErrorAction SilentlyContinue
}

Write-Host ""
Write-Msg -zh "✅ 卸载完成！" -en "✅ Uninstallation complete!" -Color Green
Write-Host ""
Write-Msg -zh "⚠️  数据目录已保留（含数据库、配置、模型）:" `
           -en "⚠️  Data directory kept (DB, configs, models):" -Color Yellow
Write-Host "   $DataDir"
Write-Msg -zh "   彻底清除所有数据请手动执行:" -en "   To fully remove all data, run:" -Color Yellow
Write-Host "   Remove-Item -Path `"$DataDir`" -Recurse -Force"
Write-Host ""
if ($IsZh) { Read-Host "按回车键退出" } else { Read-Host "Press Enter to exit" }
