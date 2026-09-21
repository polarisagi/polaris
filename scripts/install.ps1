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
$Port       = 28888
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

# ── 2.5 环境依赖预检与自动安装 (Dependencies) ────────────────────────────────
Write-Msg -zh "🔍 正在检查并自动安装必要运行环境..." -en "🔍 Checking and installing required dependencies..." -Color Cyan

if (-not (Get-Command "git" -ErrorAction SilentlyContinue)) {
    Write-Msg -zh "⚠️  缺少 Git。正在尝试安装..." -en "⚠️  Git not found. Attempting to install..." -Color Yellow
    try { winget install --id Git.Git -e --source winget } catch { Write-Msg -zh "⚠️ 请手动安装 Git" -en "⚠️ Please install Git manually" -Color Red }
}

if (-not (Get-Command "uv" -ErrorAction SilentlyContinue)) {
    Write-Msg -zh "   📦 正在安装 uv..." -en "   📦 Installing uv..." -Color Gray
    try { Invoke-WebRequest -Uri "https://astral.sh/uv/install.ps1" -UseBasicParsing | Invoke-Expression } catch { }
    $env:Path += ";$env:USERPROFILE\.cargo\bin;$env:USERPROFILE\.local\bin"
}

if (-not (Get-Command "python" -ErrorAction SilentlyContinue) -and -not (Get-Command "python3" -ErrorAction SilentlyContinue)) {
    Write-Msg -zh "   🐍 正在通过 uv 安装 Python..." -en "   🐍 Installing Python via uv..." -Color Gray
    try { 
        uv python install 3 
        $UvPythonExe = uv python find 3
        if ($UvPythonExe) {
            $UvPythonDir = Split-Path $UvPythonExe -Parent
            $env:Path += ";$UvPythonDir"
            $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
            if ($UserPath -notmatch [regex]::Escape($UvPythonDir)) {
                [Environment]::SetEnvironmentVariable("Path", "$UserPath;$UvPythonDir", "User")
            }
        }
    } catch { }
}

if (-not (Get-Command "node" -ErrorAction SilentlyContinue)) {
    Write-Msg -zh "   🟢 正在安装 Node.js..." -en "   🟢 Installing Node.js..." -Color Gray
    try { winget install --id OpenJS.NodeJS -e --source winget } catch {
        Write-Msg -zh "⚠️ 请手动安装 Node.js" -en "⚠️ Please install Node.js manually" -Color Red
    }
}
Write-Msg -zh "✅ 基础环境准备就绪。" -en "✅ Dependencies ready." -Color Green

# ── 3. 确定架构与下载候选列表 ────────────────────────────────────────────────
$Arch        = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$ArchiveName = "polaris-windows-$Arch.zip"
$GithubURL   = "https://github.com/$Repo/releases/latest/download/$ArchiveName"
$ProxyHosts  = @("https://ghproxy.net", "https://mirror.ghproxy.com")

# 判断当前网络环境是否处于中国大陆
function Test-IsMainlandChina {
    $Timeout = 2

    # 尝试 1: ipinfo.io
    try {
        $Country = (Invoke-RestMethod -Uri "https://ipinfo.io/country" -TimeoutSec $Timeout -ErrorAction Stop).Trim()
        if ([string]::IsNullOrEmpty($Country) -eq $false) {
            return ($Country -eq "CN")
        }
    } catch {}

    # 尝试 2: cloudflare trace
    try {
        $Trace = Invoke-RestMethod -Uri "https://1.1.1.1/cdn-cgi/trace" -TimeoutSec $Timeout -ErrorAction Stop
        if ($Trace -match "loc=([A-Z]{2})") {
            return ($Matches[1] -eq "CN")
        }
    } catch {}

    # 尝试 3: ip.sb
    try {
        $IpSb = Invoke-RestMethod -Uri "https://api.ip.sb/geoip" -TimeoutSec $Timeout -ErrorAction Stop
        if ($IpSb.country_code) {
            return ($IpSb.country_code -eq "CN")
        }
    } catch {}

    # 降级：如果全部失败，测速 Github
    try {
        Invoke-WebRequest -Uri "https://github.com" -UseBasicParsing -TimeoutSec 1 -Method Head -ErrorAction Stop | Out-Null
        return $false
    } catch {
        return $true
    }
}

Write-Msg -zh "🌐 正在检测网络环境归属地及代理情况..." `
           -en "🌐 Detecting network geolocation and VPN..." -Color Cyan

$CandidateURLs = [System.Collections.ArrayList]::new()

if (-not (Test-IsMainlandChina)) {
    Write-Msg -zh "✅ 当前网络为海外 IP 或已开启全局代理，将使用直连。" `
               -en "✅ Network is outside mainland China or VPN active. Using direct connection." -Color Green
    $CandidateURLs.Add($GithubURL) | Out-Null
    foreach ($p in $ProxyHosts) { $CandidateURLs.Add("$p/$GithubURL") | Out-Null }
} else {
    Write-Msg -zh "⚠️  检测到当前网络位于中国大陆且未全局代理，切换镜像代理..." `
               -en "⚠️  Mainland China network detected without VPN. Switching to proxy mirrors..." -Color Yellow
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

# 复制解压目录内容到安装目录（处理可能存在的一层包裹目录）
$RootItems = @(Get-ChildItem -Path $ExtractDir)
if ($RootItems.Count -eq 1 -and $RootItems[0].PSIsContainer) {
    $SrcPath = "$($RootItems[0].FullName)\*"
} else {
    $SrcPath = "$ExtractDir\*"
}
Copy-Item -Path $SrcPath -Destination $InstallDir -Recurse -Force
$FinalExe = "$InstallDir\$BinName"
Remove-Item -Path $ExtractDir -Recurse -Force -ErrorAction SilentlyContinue

if (-not (Test-Path $FinalExe)) {
    Write-Msg -zh "❌ 归档中未找到 $BinName。" -en "❌ $BinName not found in archive." -Color Red
    if ($IsZh) { Read-Host "按回车键退出" } else { Read-Host "Press Enter to exit" }
    exit 1
}

Unblock-File -Path $FinalExe -ErrorAction SilentlyContinue

Write-Msg -zh "✅ 程序及依赖已安装: $InstallDir" -en "✅ Binary and dependencies installed: $InstallDir" -Color Green

# ── 6. 配置开机自启 ──────────────────────────────────────────────────────────
# 委托给 `polaris service install`，本脚本**不再自写**任务计划。
# 两份实现必然漂移其一：任务名、启动参数、权限级别各写一遍，改了一处忘了另一处
# 的结果是"装出来的任务和 polaris service status 看到的不是同一个"。
# 单一实现见 cmd/polaris/cli_service.go（ADR-0096 决策一）。
Write-Msg -zh "⚙️  注册系统服务..." -en "⚙️  Registering system service..." -Color Cyan

# 移除旧的注册表自启（历史遗留清理）
Remove-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" `
    -Name $TaskName -ErrorAction SilentlyContinue

& $FinalExe service install
if ($LASTEXITCODE -eq 0) {
    Write-Msg -zh "✅ 服务已注册并启动，登录时自动运行。" `
               -en "✅ Service registered and started, auto-starts on login." -Color Green
} else {
    Write-Msg -zh "⚠️  服务注册失败，可稍后手动执行：$FinalExe service install" `
               -en "⚠️  Service registration failed. Run manually later: $FinalExe service install" -Color Yellow
}

# ── 7. 等待就绪 ──────────────────────────────────────────────────────────────
# 不再额外 Start-Process：service install 已经启动了守护进程，重复启动会被单实例
# 锁拒绝，在日志里留下一条看起来像故障的 ALREADY_EXISTS。
Start-Sleep -Seconds 2

Write-Host ""
Write-Msg -zh "🎉 安装完成！Polaris 已在后台运行，下次登录将自动启动。" `
           -en "🎉 Installation complete! Polaris is running and will auto-start on next login." -Color Green
# 地址取自守护进程自己报告的实际端口（配置 port = 0 时由内核分配）
$ConsoleUrl = "http://127.0.0.1:$Port"
try {
    $rt = & $FinalExe service status --json | ConvertFrom-Json
    if ($rt.base_url) { $ConsoleUrl = $rt.base_url }
} catch { }
Write-Msg -zh "   请访问: $ConsoleUrl" -en "   Visit: $ConsoleUrl" -Color Yellow
Write-Host ""
if ($IsZh) { Read-Host "按回车键退出" } else { Read-Host "Press Enter to exit" }
