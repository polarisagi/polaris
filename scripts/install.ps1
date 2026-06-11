[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$Culture = [System.Globalization.CultureInfo]::InstalledUICulture.Name
$IsZh = ($Culture -match "zh")

function Write-Msg {
    param([string]$zh, [string]$en, [ConsoleColor]$Color = 'White')
    if ($IsZh) { Write-Host $zh -ForegroundColor $Color }
    else        { Write-Host $en -ForegroundColor $Color }
}

$Repo       = "polarisagi/polaris"
$BinName    = "polaris.exe"
$InstallDir = "$env:USERPROFILE\.polarisagi\polaris\bin"
$DataDir    = "$env:USERPROFILE\.polarisagi\polaris"
$LogDir     = "$DataDir\logs"
$Port       = 29999
$TaskName   = "PolarisAGI-Polaris"

Write-Msg -zh "🌌 正在安装/更新 PolarisAGI Polaris..." `
           -en "🌌 Installing/Updating PolarisAGI Polaris..." -Color Cyan

# ── 1. 创建目录 ──────────────────────────────────────────────────────────────
foreach ($dir in @($InstallDir, $LogDir)) {
    if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
}

# ── 2. 停止旧进程 ────────────────────────────────────────────────────────────
$OldProc = Get-Process -Name "polaris" -ErrorAction SilentlyContinue
if ($OldProc) {
    Write-Msg -zh "🛑 正在停止旧进程..." -en "🛑 Stopping existing process..." -Color Cyan
    Stop-Process -Name "polaris" -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
}

# ── 3. 确定架构与下载候选列表 ────────────────────────────────────────────────
$Arch        = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$ArchiveName = "polaris-windows-$Arch.zip"
$GithubURL   = "https://github.com/$Repo/releases/latest/download/$ArchiveName"
$ProxyHosts  = @("https://ghproxy.net", "https://mirror.ghproxy.com")

# 与 Go 端 autoProbe 保持一致：500ms 内可达 github.com → 海外/VPN 直连
Write-Msg -zh "🌐 正在检测网络环境（500ms 阈值）..." `
           -en "🌐 Detecting network environment (500ms threshold)..." -Color Cyan

$CandidateURLs = [System.Collections.ArrayList]::new()
$IsDirectReachable = $false

try {
    $Res = Invoke-WebRequest -Uri "https://github.com" -UseBasicParsing -TimeoutSec 0.5 `
                             -Method Head -ErrorAction Stop
    $IsDirectReachable = $true
    Write-Msg -zh "✅ GitHub 低延迟直连（<500ms），无需代理。" `
               -en "✅ GitHub low-latency direct (<500ms), no proxy needed." -Color Green
} catch { }

if ($IsDirectReachable) {
    $CandidateURLs.Add($GithubURL) | Out-Null
    foreach ($p in $ProxyHosts) { $CandidateURLs.Add("$p/$GithubURL") | Out-Null }
} else {
    Write-Msg -zh "⚠️  GitHub 响应慢，判定为中国大陆网络，切换镜像代理..." `
               -en "⚠️  GitHub slow, likely China mainland, switching to proxy mirrors..." -Color Yellow
    foreach ($p in $ProxyHosts) {
        try {
            Invoke-WebRequest -Uri $p -UseBasicParsing -TimeoutSec 5 -Method Head -ErrorAction Stop | Out-Null
            $CandidateURLs.Add("$p/$GithubURL") | Out-Null
            Write-Msg -zh "   ✅ 可用镜像: $p" -en "   ✅ Reachable proxy: $p" -Color Green
        } catch {
            Write-Msg -zh "   ⚠️  不可用: $p" -en "   ⚠️  Unreachable: $p" -Color Yellow
        }
    }
    $CandidateURLs.Add($GithubURL) | Out-Null  # 直连兜底
}

# ── 4. 下载（支持断点续传，逐源重试）────────────────────────────────────────
Write-Msg -zh "⬇️  开始下载（支持断点续传）..." `
           -en "⬇️  Downloading (with resume support)..." -Color Cyan

$ZipPath    = "$InstallDir\polaris-temp.zip"
$PartPath   = "$ZipPath.part"
$Downloaded = $false
$ProgressPreference = 'SilentlyContinue'

foreach ($src in $CandidateURLs) {
    Write-Msg -zh "   尝试: $src" -en "   Trying: $src" -Color Gray
    try {
        # 读取已下载字节数（断点续传）
        $Offset = 0
        if (Test-Path $PartPath) {
            $Offset = (Get-Item $PartPath).Length
        }

        if ($Offset -gt 0) {
            Write-Msg -zh "   续传自 $Offset 字节..." -en "   Resuming from $Offset bytes..." -Color Gray
            # 发送 Range 头实现续传
            $Headers = @{ Range = "bytes=$Offset-" }
            $Resp = Invoke-WebRequest -Uri $src -Headers $Headers -UseBasicParsing -ErrorAction Stop
            if ($Resp.StatusCode -eq 206) {
                # 追加模式
                $Stream = [System.IO.FileStream]::new($PartPath, [System.IO.FileMode]::Append)
                $Stream.Write($Resp.Content, 0, $Resp.Content.Length)
                $Stream.Close()
            } elseif ($Resp.StatusCode -eq 200) {
                # 服务端不支持 Range，重新全量下载
                [System.IO.File]::WriteAllBytes($PartPath, $Resp.Content)
            } else {
                throw "HTTP $($Resp.StatusCode)"
            }
        } else {
            # 全量下载
            Invoke-WebRequest -Uri $src -OutFile $PartPath -UseBasicParsing -ErrorAction Stop
        }

        Move-Item -Path $PartPath -Destination $ZipPath -Force
        $Downloaded = $true
        break
    } catch {
        Write-Msg -zh "   此源失败：$($_.Exception.Message)，尝试下一个..." `
                   -en "   Source failed: $($_.Exception.Message), trying next..." -Color Yellow
    }
}

if (-not $Downloaded) {
    Write-Msg -zh "❌ 所有下载源均失败，请检查网络连接或稍后重试。" `
               -en "❌ All download sources failed. Check your network or retry later." -Color Red
    Remove-Item -Path $PartPath -Force -ErrorAction SilentlyContinue
    if ($IsZh) { Read-Host "按回车键退出" } else { Read-Host "Press Enter to exit" }
    exit 1
}

# ── 5. 校验 & 解压 ───────────────────────────────────────────────────────────
Write-Msg -zh "📦 正在校验并解压..." -en "📦 Verifying and extracting..." -Color Cyan

# 验证 zip 完整性
try {
    $ZipFile = [System.IO.Compression.ZipFile]::OpenRead($ZipPath)
    $ZipFile.Dispose()
} catch {
    Write-Msg -zh "❌ 归档文件损坏，已删除，请重新运行安装脚本。" `
               -en "❌ Archive corrupted, deleted. Re-run the install script." -Color Red
    Remove-Item -Path $ZipPath -Force -ErrorAction SilentlyContinue
    if ($IsZh) { Read-Host "按回车键退出" } else { Read-Host "Press Enter to exit" }
    exit 1
}

$ExtractDir = "$InstallDir\polaris-extract-temp"
if (Test-Path $ExtractDir) { Remove-Item -Path $ExtractDir -Recurse -Force }

Add-Type -AssemblyName System.IO.Compression.FileSystem
Expand-Archive -Path $ZipPath -DestinationPath $ExtractDir -Force
Remove-Item -Path $ZipPath -Force

# 复制整个解压目录内容到安装目录
Copy-Item -Path "$ExtractDir\*" -Destination $InstallDir -Recurse -Force
$FinalExe = "$InstallDir\$BinName"
Remove-Item -Path $ExtractDir -Recurse -Force -ErrorAction SilentlyContinue

if (-not (Test-Path $FinalExe)) {
    Write-Msg -zh "❌ 归档中未找到 $BinName。" -en "❌ $BinName not found in archive." -Color Red
    if ($IsZh) { Read-Host "按回车键退出" } else { Read-Host "Press Enter to exit" }
    exit 1
}

Unblock-File -Path $FinalExe -ErrorAction SilentlyContinue

Write-Msg -zh "✅ 程序及依赖已安装: $InstallDir" -en "✅ Binary and dependencies installed: $InstallDir" -Color Green

# ── 6. 配置开机自启（Windows 任务计划）───────────────────────────────────────
Write-Msg -zh "⚙️  配置开机自启（任务计划程序）..." `
           -en "⚙️  Configuring startup (Task Scheduler)..." -Color Cyan

# 移除旧的注册表自启（历史遗留清理）
Remove-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" `
    -Name $TaskName -ErrorAction SilentlyContinue

try {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue | Out-Null

    $Action    = New-ScheduledTaskAction -Execute $FinalExe
    $Trigger   = New-ScheduledTaskTrigger -AtLogOn
    $Principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Limited
    $Settings  = New-ScheduledTaskSettingsSet -ExecutionTimeLimit 0 -MultipleInstances IgnoreNew

    Register-ScheduledTask -TaskName $TaskName -Action $Action -Trigger $Trigger `
        -Principal $Principal -Settings $Settings -Force | Out-Null

    Write-Msg -zh "✅ 已注册任务计划：登录时自动后台启动。" `
               -en "✅ Task Scheduler registered: auto-starts on login." -Color Green
} catch {
    Write-Msg -zh "⚠️  任务计划注册失败，回退到注册表自启。" `
               -en "⚠️  Task Scheduler failed, falling back to registry startup." -Color Yellow
    Set-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" `
        -Name $TaskName -Value "`"$FinalExe`""
}

# ── 7. 启动服务 ──────────────────────────────────────────────────────────────
Write-Msg -zh "🚀 正在启动 Polaris..." -en "🚀 Starting Polaris..." -Color Cyan
Start-Process -FilePath $FinalExe -WindowStyle Hidden -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2

Write-Host ""
Write-Msg -zh "🎉 安装完成！Polaris 已在后台运行，下次登录将自动启动。" `
           -en "🎉 Installation complete! Polaris is running and will auto-start on next login." -Color Green
Write-Msg -zh "   请访问: http://127.0.0.1:$Port" -en "   Visit: http://127.0.0.1:$Port" -Color Yellow
Write-Host ""
if ($IsZh) { Read-Host "按回车键退出" } else { Read-Host "Press Enter to exit" }
