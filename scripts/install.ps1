# 保持 UTF-8 无 BOM，避免 irm | iex 将 BOM 当作首条命令的一部分。
$PreviousErrorActionPreference = $ErrorActionPreference
$PreviousProgressPreference = $ProgressPreference
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
# 确保 Windows PowerShell 5.1 默认启用 TLS 1.2，防止全新系统访问 GitHub / astral.sh 报 SSL 握手失败
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Culture = [System.Globalization.CultureInfo]::InstalledUICulture.Name
$IsZh = ($Culture -match "zh")

function Write-Msg {
    param([string]$zh, [string]$en, [ConsoleColor]$Color = 'White')
    if ($IsZh) { Write-Host $zh -ForegroundColor $Color }
    else        { Write-Host $en -ForegroundColor $Color }
}

# 全局系统参数与路径配置（与 internal/config/system_params.go 保持单一事实源一致）
$Repo          = "polarisagi/polaris"
$BinName       = "polaris.exe"
$InstallDir    = "$env:USERPROFILE\.polaris\bin"
$DataDir       = "$env:USERPROFILE\.polaris"
$DepsDir       = "$DataDir\deps"
$LogDir        = "$DataDir\logs"
$LocalBinDir   = "$env:USERPROFILE\.local\bin"
$CargoBinDir   = "$env:USERPROFILE\.cargo\bin"
$Port          = 28888
$TaskName      = "PolarisAGI-Polaris"
$NodeVersion   = "24"
$DepsReady     = $true
$ServiceReady  = $false

Write-Msg -zh "🌌 正在安装/更新 PolarisAGI Polaris..." `
           -en "🌌 Installing/Updating PolarisAGI Polaris..." -Color Cyan

$WorkDir = Join-Path ([System.IO.Path]::GetTempPath()) ("polaris-install-" + [guid]::NewGuid().ToString('N'))
try {
New-Item -ItemType Directory -Path $WorkDir | Out-Null
# ── 1. 创建目录与 PATH 管理工具函数 ──────────────────────────────────────────
foreach ($dir in @($InstallDir, $LogDir, $DepsDir, $LocalBinDir)) {
    if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
}

# 将目录幂等插入到用户级 Path 的最前面（确保排在 %LOCALAPPDATA%\Microsoft\WindowsApps 之前，
# 防止全新 Windows 10/11 的微软商店 python.exe / python3.exe 空壳别名劫持命令解析）
function Add-UserPathFront {
    param([string]$DirToAdd)
    if ([string]::IsNullOrWhiteSpace($DirToAdd) -or -not (Test-Path -LiteralPath $DirToAdd)) { return }
    $Normalized = $DirToAdd.TrimEnd('\')

    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (-not $UserPath) { $UserPath = "" }
    $Parts = @($UserPath -split ';' | Where-Object {
        -not [string]::IsNullOrWhiteSpace($_) -and ($_.TrimEnd('\') -ne $Normalized)
    })
    $NewUserPath = (@($Normalized) + $Parts) -join ';'
    try {
        [Environment]::SetEnvironmentVariable("Path", $NewUserPath, "User")
    } catch { }

    $SessionParts = @($env:Path -split ';' | Where-Object {
        -not [string]::IsNullOrWhiteSpace($_) -and ($_.TrimEnd('\') -ne $Normalized)
    })
    $env:Path = (@($Normalized) + $SessionParts) -join ';'
}

# 从注册表重新加载 Machine + User Path 并探测常见安装目录，解决 winget 安装后当前会话找不到命令的问题
function Update-SessionPath {
    $MachinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
    $UserPath    = [Environment]::GetEnvironmentVariable("Path", "User")
    $Combined    = "$env:Path;$UserPath;$MachinePath"

    $ExtraDirs = @(
        $InstallDir,
        $LocalBinDir,
        $CargoBinDir,
        "$DepsDir\node",
        "$DepsDir\git\cmd",
        "$env:ProgramFiles\Git\cmd",
        "${env:ProgramFiles(x86)}\Git\cmd",
        "$env:LOCALAPPDATA\Programs\Git\cmd",
        "$env:ProgramFiles\nodejs"
    )
    $Seen = @{}
    $Result = New-Object System.Collections.Generic.List[string]
    foreach ($p in ($ExtraDirs + ($Combined -split ';'))) {
        if (-not [string]::IsNullOrWhiteSpace($p)) {
            $key = $p.TrimEnd('\').ToLowerInvariant()
            if (-not $Seen.ContainsKey($key) -and (Test-Path -LiteralPath $p)) {
                $Seen[$key] = $true
                $Result.Add($p.TrimEnd('\'))
            }
        }
    }
    $env:Path = $Result -join ';'
}

Add-UserPathFront -DirToAdd $LocalBinDir
Add-UserPathFront -DirToAdd $InstallDir
Update-SessionPath

# ── 2.2 确定架构与网络环境（前置到依赖安装前，以便依赖安装同样复用镜像源）─────
$NativeArch = $env:PROCESSOR_ARCHITEW6432
if (-not $NativeArch) { $NativeArch = $env:PROCESSOR_ARCHITECTURE }
$Arch = switch ($NativeArch) {
    'ARM64' { 'arm64' }
    'AMD64' { 'amd64' }
    default { throw "Unsupported Windows architecture: $NativeArch (64-bit Windows required)" }
}
$ArchiveName = "polaris-windows-$Arch.zip"
$GithubURL   = "https://github.com/$Repo/releases/latest/download/$ArchiveName"
$ProxyHosts  = @("https://ghproxy.net", "https://mirror.ghproxy.com")

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
$IsMainland    = Test-IsMainlandChina
$ActiveProxy   = ""

if (-not $IsMainland) {
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
            if (-not $ActiveProxy) { $ActiveProxy = $p }
            $CandidateURLs.Add("$p/$GithubURL") | Out-Null
            Write-Msg -zh "   ✅ 可用镜像: $p" -en "   ✅ Reachable proxy: $p" -Color Green
        } catch {
            Write-Msg -zh "   ⚠️  不可用: $p" -en "   ⚠️  Unreachable: $p" -Color Yellow
        }
    }
    $CandidateURLs.Add($GithubURL) | Out-Null  # 直连兜底
}

# ── 2.5 环境依赖预检与自动安装 (Dependencies) ────────────────────────────────
Write-Msg -zh "🔍 正在检查并自动安装必要运行环境..." -en "🔍 Checking and installing required dependencies..." -Color Cyan
$ProgressPreference = 'SilentlyContinue'

function Test-WorkingGit {
    Update-SessionPath
    $cmd = Get-Command "git" -ErrorAction SilentlyContinue
    if (-not $cmd) { return $false }
    try {
        $null = & $cmd.Source --version 2>$null
        return ($LASTEXITCODE -eq 0)
    } catch {
        return $false
    }
}

function Test-WorkingUv {
    Update-SessionPath
    foreach ($name in @('uv', 'uvx')) {
        $cmd = Get-Command $name -ErrorAction SilentlyContinue
        if (-not $cmd) { return $false }
        try {
            $null = & $cmd.Source --version 2>$null
            if ($LASTEXITCODE -ne 0) { return $false }
        } catch { return $false }
    }
    return $true
}

# 真实性检测：必须排除 %LOCALAPPDATA%\Microsoft\WindowsApps 下的微软商店跳转空壳
function Test-WorkingPython {
    Update-SessionPath
    foreach ($name in @("python3", "python")) {
        $cmds = @(Get-Command $name -All -ErrorAction SilentlyContinue)
        foreach ($c in $cmds) {
            if ($c.Source -and ($c.Source -notmatch "WindowsApps")) {
                try {
                    $null = & $c.Source -c "import sys; sys.exit(0 if sys.version_info.major >= 3 else 1)" 2>$null
                    if ($LASTEXITCODE -eq 0) { $script:WorkingPythonExe = $c.Source; return $true }
                } catch { }
            }
        }
    }
    return $false
}

function Test-WorkingNode {
    Update-SessionPath
    $nodeCmd = Get-Command "node" -ErrorAction SilentlyContinue
    $npxCmd  = Get-Command "npx.cmd" -ErrorAction SilentlyContinue
    if (-not $nodeCmd -or -not $npxCmd) { return $false }
    try {
        $null = & $nodeCmd.Source -e 'process.exit(parseInt(process.versions.node,10)>=22?0:1)' 2>$null
        if ($LASTEXITCODE -ne 0) { return $false }
        $null = & $npxCmd.Source --version 2>$null
        if ($LASTEXITCODE -ne 0) { return $false }
        $null = & npm.cmd --version 2>$null
        return ($LASTEXITCODE -eq 0)
    } catch {
        return $false
    }
}

# 1. Git 检查与自动安装（优先 winget，失败或无 winget 时自动回退便携版 MinGit）
if (-not (Test-WorkingGit)) {
    Write-Msg -zh "   📦 缺少 Git，正在自动安装..." -en "   📦 Git not found. Installing automatically..." -Color Gray
    if (Get-Command "winget" -ErrorAction SilentlyContinue) {
        try {
            & winget install --id Git.Git -e --source winget --accept-package-agreements --accept-source-agreements --silent
        } catch { }
        Update-SessionPath
    }

    if (-not (Test-WorkingGit)) {
        Write-Msg -zh "   📦 正在下载便携版 MinGit..." -en "   📦 Downloading portable MinGit..." -Color Gray
        $MinGitArch = if ($Arch -eq "arm64") { "arm64" } else { "64-bit" }
        # 官方 API 的摘要独立于镜像；无法取得可信摘要时不执行镜像中的程序。
        $GitAsset = $null
        try {
            $GitRelease = Invoke-RestMethod -Uri "https://api.github.com/repos/git-for-windows/git/releases/latest" -TimeoutSec 30 -ErrorAction Stop
            $GitAsset = $GitRelease.assets | Where-Object { $_.name -match "^MinGit-[0-9.]+-$MinGitArch\.zip$" } | Select-Object -First 1
        } catch { }
        $GhMinGit = if ($GitAsset) { $GitAsset.browser_download_url } else { "" }
        $GitHash = if ($GitAsset -and $GitAsset.digest -match '^sha256:([a-fA-F0-9]{64})$') { $Matches[1].ToLowerInvariant() } else { "" }

        $GitUrls = @()
        if ($IsMainland) {
            if ($ActiveProxy -and $GhMinGit -and $GitHash) { $GitUrls += "$ActiveProxy/$GhMinGit" }
            if ($GhMinGit -and $GitHash) { $GitUrls += $GhMinGit }
        } else {
            if ($GhMinGit -and $GitHash) { $GitUrls += $GhMinGit }
        }

        $MinGitZip = "$WorkDir\mingit.zip"
        foreach ($u in $GitUrls) {
            try {
                Invoke-WebRequest -Uri $u -OutFile $MinGitZip -UseBasicParsing -TimeoutSec 180 -ErrorAction Stop
                if ((Get-FileHash -LiteralPath $MinGitZip -Algorithm SHA256).Hash.ToLowerInvariant() -ne $GitHash) { throw "MinGit checksum mismatch" }
                $GitDest = "$DepsDir\git"
                $GitExtract = Join-Path $WorkDir 'git-extract'
                if (Test-Path -LiteralPath $GitExtract) { Remove-Item -LiteralPath $GitExtract -Recurse -Force }
                Expand-Archive -LiteralPath $MinGitZip -DestinationPath $GitExtract -Force
                if (-not (Test-Path -LiteralPath (Join-Path $GitExtract 'cmd/git.exe'))) { throw 'MinGit archive does not contain cmd/git.exe' }
                # 解压及结构验证成功后才替换旧依赖，失败下载不能破坏原安装。
                if (Test-Path -LiteralPath $GitDest) { Remove-Item -LiteralPath $GitDest -Recurse -Force }
                Move-Item -LiteralPath $GitExtract -Destination $GitDest
                Remove-Item -LiteralPath $MinGitZip -Force -ErrorAction SilentlyContinue
                Add-UserPathFront -DirToAdd "$GitDest\cmd"
                break
            } catch {
                Write-Msg -zh "   ⚠️ MinGit 安装尝试失败：$($_.Exception.Message)" `
                           -en "   ⚠️ MinGit installation attempt failed: $($_.Exception.Message)" -Color Yellow
                Remove-Item -LiteralPath $MinGitZip -Force -ErrorAction SilentlyContinue
            }
        }
    }

    if (-not (Test-WorkingGit)) {
        Write-Msg -zh "⚠️  Git 自动安装未完成，请稍后手动安装 Git (https://git-scm.com)。" `
                   -en "⚠️  Git auto-install did not complete. Please install Git manually later (https://git-scm.com)." -Color Yellow
    }
}

# uv 的失败处理会调用 exit；必须隔离到子进程，避免关闭 irm | iex 所在的终端。
function Invoke-UvInstaller {
    $InstallerText = Invoke-RestMethod -Uri "https://astral.sh/uv/install.ps1" -TimeoutSec 60 -ErrorAction Stop
    $InstallerPath = Join-Path $WorkDir 'uv-install.ps1'
    # 子进程按文件读取，BOM 确保 Windows PowerShell 5.1 正确识别 UTF-8。
    [System.IO.File]::WriteAllText($InstallerPath, $InstallerText, [System.Text.UTF8Encoding]::new($true))
    $PowerShellExe = (Get-Process -Id $PID).Path
    & $PowerShellExe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $InstallerPath
    if ($LASTEXITCODE -ne 0) { throw "uv installer failed (exit code $LASTEXITCODE); see output above." }
}

# 2. uv 检查与自动安装
if (-not (Test-WorkingUv)) {
    Write-Msg -zh "   📦 正在安装 uv..." -en "   📦 Installing uv..." -Color Gray
    $SavedUvDir = $env:UV_INSTALL_DIR
    $SavedUvBase = $env:UV_INSTALLER_GITHUB_BASE_URL
    try {
        $env:UV_INSTALL_DIR = $LocalBinDir
        if ($IsMainland -and $ActiveProxy) {
            $env:UV_INSTALLER_GITHUB_BASE_URL = "$ActiveProxy/https://github.com"
        }
        try {
            Invoke-UvInstaller
        } catch {
            Write-Msg -zh "   ⚠️ uv 安装失败：$($_.Exception.Message)，尝试官方源..." `
                       -en "   ⚠️ uv installation failed: $($_.Exception.Message). Retrying official source..." -Color Yellow
            Remove-Item Env:\UV_INSTALLER_GITHUB_BASE_URL -ErrorAction SilentlyContinue
            try {
                Invoke-UvInstaller
            } catch {
                Write-Msg -zh "   ⚠️ uv 重试失败：$($_.Exception.Message)" `
                           -en "   ⚠️ uv retry failed: $($_.Exception.Message)" -Color Yellow
            }
        }
    } finally {
        $env:UV_INSTALL_DIR = $SavedUvDir
        $env:UV_INSTALLER_GITHUB_BASE_URL = $SavedUvBase
    }
    Add-UserPathFront -DirToAdd $LocalBinDir
    Add-UserPathFront -DirToAdd $CargoBinDir
    Update-SessionPath
}

function Install-ManagedPython {
    $SavedPythonMirror = $env:UV_PYTHON_INSTALL_MIRROR
    $PythonMirrors = @('')
    if ($IsMainland -and $ActiveProxy) {
        $PythonMirrors = @("$ActiveProxy/https://github.com/astral-sh/python-build-standalone/releases/download", '')
    } elseif ($SavedPythonMirror) {
        $PythonMirrors = @($SavedPythonMirror, '')
    }
    try {
        foreach ($Mirror in $PythonMirrors) {
            $env:UV_PYTHON_INSTALL_MIRROR = $Mirror
            try {
                & uv python install 3.12
                if ($LASTEXITCODE -eq 0) { return }
                throw "uv python install failed (exit code $LASTEXITCODE)"
            } catch {
                Write-Msg -zh "   ⚠️ Python 下载尝试失败：$($_.Exception.Message)" `
                           -en "   ⚠️ Python download attempt failed: $($_.Exception.Message)" -Color Yellow
            }
        }
        Write-Msg -zh "⚠️ Python 安装失败，请检查上面的错误信息。" `
                   -en "⚠️ Python installation failed. Check the errors above." -Color Yellow
    } finally {
        $env:UV_PYTHON_INSTALL_MIRROR = $SavedPythonMirror
    }
}

# 3. Python 3 检查与自动安装（通过 uv 安装独立 Python，补齐 python3.exe 并置于 WindowsApps 之前）
if (-not (Test-WorkingPython) -and (Test-WorkingUv)) {
    Write-Msg -zh "   🐍 正在通过 uv 安装独立 Python 3 环境..." -en "   🐍 Installing standalone Python 3 via uv..." -Color Gray
    Install-ManagedPython
}

# 已有 Python 也需前置其真实目录；只检查“任意候选可用”会让商店别名继续抢先。
$PythonExe = $null
if (Test-WorkingPython) {
    $PythonExe = $script:WorkingPythonExe
} elseif (Test-WorkingUv) {
    try {
        $PythonExe = (& uv python find --managed-python 3.12 2>$null | Select-Object -First 1)
        if ($LASTEXITCODE -ne 0) { $PythonExe = $null }
    } catch { $PythonExe = $null }
}
if ($PythonExe -and (Test-Path -LiteralPath $PythonExe)) {
    $PythonDir = Split-Path "$PythonExe".Trim() -Parent
    $Py3Exe = Join-Path $PythonDir "python3.exe"
    if (-not (Test-Path -LiteralPath $Py3Exe)) {
        Copy-Item -LiteralPath $PythonExe -Destination $Py3Exe -Force -ErrorAction SilentlyContinue
    }
    Add-UserPathFront -DirToAdd $PythonDir
}

# 4. Node.js 检查与自动安装（优先 winget，失败/无 winget/大陆网络自动回退官方/npmmirror 便携包）
if (-not (Test-WorkingNode)) {
    Write-Msg -zh "   🟢 正在安装 Node.js v$NodeVersion..." -en "   🟢 Installing Node.js v$NodeVersion..." -Color Gray
    if (-not $IsMainland -and (Get-Command "winget" -ErrorAction SilentlyContinue)) {
        try {
            & winget install --id OpenJS.NodeJS.LTS -e --source winget --accept-package-agreements --accept-source-agreements --silent
        } catch { }
        Update-SessionPath
    }

    if (-not (Test-WorkingNode)) {
        $NodeArch = if ($Arch -eq "arm64") { "arm64" } else { "x64" }
        $NodeMirrors = if ($IsMainland) {
            @("https://npmmirror.com/mirrors/node/latest-v$NodeVersion.x", "https://nodejs.org/dist/latest-v$NodeVersion.x")
        } else {
            @("https://nodejs.org/dist/latest-v$NodeVersion.x", "https://npmmirror.com/mirrors/node/latest-v$NodeVersion.x")
        }

        $NodeZip = "$WorkDir\node.zip"
        foreach ($baseUrl in $NodeMirrors) {
            try {
                $Shasums = Invoke-RestMethod -Uri "https://nodejs.org/dist/latest-v$NodeVersion.x/SHASUMS256.txt" -TimeoutSec 15 -ErrorAction Stop
                $MatchLine = ($Shasums -split "`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ -match "^[a-fA-F0-9]{64}\s+node-v$NodeVersion\.\d+\.\d+-win-$NodeArch\.zip$" } | Select-Object -First 1)
                if (-not $MatchLine) { continue }
                $Tokens = ($MatchLine.Trim() -split '\s+')
                $ExpectedHash = $Tokens[0].ToLowerInvariant()
                $ZipFileName  = $Tokens[1].Trim()

                Write-Msg -zh "   ⬇️  正在下载 Node.js ($ZipFileName)..." -en "   ⬇️  Downloading Node.js ($ZipFileName)..." -Color Gray
                Invoke-WebRequest -Uri "$baseUrl/$ZipFileName" -OutFile $NodeZip -UseBasicParsing -TimeoutSec 180 -ErrorAction Stop
                $ActualHash = (Get-FileHash -LiteralPath $NodeZip -Algorithm SHA256).Hash.ToLowerInvariant()
                if ($ActualHash -ne $ExpectedHash) {
                    throw 'Node archive checksum mismatch'
                }

                $NodeExtract = "$WorkDir\node-extract"
                $NodeDest    = "$DepsDir\node"
                if (Test-Path -LiteralPath $NodeExtract) { Remove-Item -LiteralPath $NodeExtract -Recurse -Force -ErrorAction SilentlyContinue }
                Expand-Archive -LiteralPath $NodeZip -DestinationPath $NodeExtract -Force
                Remove-Item -LiteralPath $NodeZip -Force -ErrorAction SilentlyContinue

                $InnerDir = Get-ChildItem -LiteralPath $NodeExtract -Directory | Select-Object -First 1
                if (-not $InnerDir) { throw 'Node archive has no root directory' }
                foreach ($Required in @('node.exe', 'npm.cmd', 'npx.cmd')) {
                    if (-not (Test-Path -LiteralPath (Join-Path $InnerDir.FullName $Required))) { throw "Node archive is missing $Required" }
                }
                if (Test-Path -LiteralPath $NodeDest) { Remove-Item -LiteralPath $NodeDest -Recurse -Force }
                Move-Item -LiteralPath $InnerDir.FullName -Destination $NodeDest
                Remove-Item -LiteralPath $NodeExtract -Recurse -Force -ErrorAction SilentlyContinue
                Add-UserPathFront -DirToAdd $NodeDest
                break
            } catch {
                Write-Msg -zh "   ⚠️ Node.js 安装尝试失败：$($_.Exception.Message)" `
                           -en "   ⚠️ Node.js installation attempt failed: $($_.Exception.Message)" -Color Yellow
                Remove-Item -LiteralPath $NodeZip -Force -ErrorAction SilentlyContinue
            }
        }
    }

    if (Test-WorkingNode) {
        if ($IsMainland) {
            try { & npm.cmd config set registry https://registry.npmmirror.com 2>$null } catch { }
        }
    } else {
        Write-Msg -zh "⚠️  Node.js 自动安装未完成，请稍后手动安装 Node.js (https://nodejs.org)。" `
                   -en "⚠️  Node.js auto-install did not complete. Please install Node.js manually later (https://nodejs.org)." -Color Yellow
    }
}

foreach ($dep in @('Git', 'Uv', 'Python', 'Node')) {
    $Ready = & "Test-Working$dep"
    if (-not $Ready) {
        $DepsReady = $false
        Write-Msg -zh "⚠️ 运行环境尚未就绪: $dep" -en "⚠️ Dependency not ready: $dep" -Color Yellow
    }
}
if ($DepsReady) { Write-Msg -zh "✅ 基础环境准备就绪。" -en "✅ Dependencies ready." -Color Green }


# ── 4. 下载（逐源重试）────────────────────────────────────────
Write-Msg -zh "⬇️  开始下载 Polaris（逐源重试）..." `
           -en "⬇️  Downloading Polaris (with source retries)..." -Color Cyan

$ZipPath       = "$WorkDir\polaris.zip"
$PartPath      = "$ZipPath.part"
$Downloaded    = $false
$DownloadedUrl = ""

foreach ($src in $CandidateURLs) {
    Write-Msg -zh "   尝试: $src" -en "   Trying: $src" -Color Gray
    try {
        Remove-Item -LiteralPath $PartPath -Force -ErrorAction SilentlyContinue
        # OutFile 直接落盘，避免将整个发行包装入 PowerShell 的 Content 内存对象。
        Invoke-WebRequest -Uri $src -OutFile $PartPath -UseBasicParsing -TimeoutSec 300 -ErrorAction Stop

        Move-Item -LiteralPath $PartPath -Destination $ZipPath -Force
        $Downloaded    = $true
        $DownloadedUrl = $src
        break
    } catch {
        Write-Msg -zh "   此源失败：$($_.Exception.Message)，尝试下一个..." `
                   -en "   Source failed: $($_.Exception.Message), trying next..." -Color Yellow
    }
}

if (-not $Downloaded) {
    Write-Msg -zh "❌ 所有下载源均失败，请检查网络连接或稍后重试。" `
               -en "❌ All download sources failed. Check your network or retry later." -Color Red
    Remove-Item -LiteralPath $PartPath -Force -ErrorAction SilentlyContinue
    throw "All Polaris download sources failed."
}

# ── 5. 校验 & 解压 ───────────────────────────────────────────────────────────
Write-Msg -zh "📦 正在校验并解压..." -en "📦 Verifying and extracting..." -Color Cyan

# 5a. SHA-256 完整性校验（与 install.sh 对齐，使用 PowerShell 原生 Get-FileHash）
$ShaFile    = "$ZipPath.sha256"
$ShaFetched = $false
Remove-Item -LiteralPath $ShaFile -Force -ErrorAction SilentlyContinue

try {
    Invoke-WebRequest -Uri "$GithubURL.sha256" -OutFile $ShaFile -UseBasicParsing -TimeoutSec 30 -ErrorAction Stop
    $ShaFetched = $true
} catch {
    if ($env:POLARIS_ALLOW_MIRROR_CHECKSUM -eq "1" -and $DownloadedUrl -ne $GithubURL) {
        Write-Msg -zh "⚠️  无法从官方直连源获取校验和文件。检测到 POLARIS_ALLOW_MIRROR_CHECKSUM=1，回退到二进制同源 ($DownloadedUrl) 获取校验和（风险自负）。" `
                   -en "⚠️  Failed to fetch checksum from official source. POLARIS_ALLOW_MIRROR_CHECKSUM=1 detected; falling back to binary source ($DownloadedUrl) at your own risk." -Color Yellow
        try {
            Invoke-WebRequest -Uri "$DownloadedUrl.sha256" -OutFile $ShaFile -UseBasicParsing -TimeoutSec 30 -ErrorAction Stop
            $ShaFetched = $true
        } catch { }
    }
}

if (-not $ShaFetched) {
    Write-Msg -zh "❌ 无法从官方直连源获取校验和文件，出于供应链安全考虑已中止安装。" `
               -en "❌ Failed to download checksum file from official source. Aborting install for supply-chain safety." -Color Red
    Write-Msg -zh "   若你所在网络无法直连 GitHub，可先执行 `$env:POLARIS_ALLOW_MIRROR_CHECKSUM='1'` 后重新运行本脚本以回退到镜像源校验和。" `
               -en "   If your network cannot reach GitHub directly, set `$env:POLARIS_ALLOW_MIRROR_CHECKSUM='1'` and re-run this script to fall back to mirror checksum." -Color Yellow
    Remove-Item -LiteralPath $ZipPath, $ShaFile -Force -ErrorAction SilentlyContinue
    throw "Cannot obtain trusted Polaris checksum."
}

$ExpectedSha = ((Get-Content -LiteralPath $ShaFile -Raw).Trim() -split '\s+')[0].ToLowerInvariant()
$ActualSha   = (Get-FileHash -LiteralPath $ZipPath -Algorithm SHA256).Hash.ToLowerInvariant()
Remove-Item -LiteralPath $ShaFile -Force -ErrorAction SilentlyContinue

if ([string]::IsNullOrWhiteSpace($ExpectedSha) -or ($ExpectedSha -ne $ActualSha)) {
    Write-Msg -zh "❌ 下载的安装包校验和不匹配，可能遭遇中间人篡改，安装已中止。" `
               -en "❌ Downloaded package checksum mismatch; possible tampering detected. Installation aborted." -Color Red
    Remove-Item -LiteralPath $ZipPath -Force -ErrorAction SilentlyContinue
    throw "Polaris archive checksum mismatch."
}
Write-Msg -zh "✅ 校验和匹配，归档完整性已确认。" -en "✅ Checksum verified, archive integrity confirmed." -Color Green

# 5b. 验证 zip 结构完整性
Add-Type -AssemblyName System.IO.Compression.FileSystem
try {
    $ZipFile = [System.IO.Compression.ZipFile]::OpenRead($ZipPath)
    $ZipFile.Dispose()
} catch {
    Write-Msg -zh "❌ 归档文件损坏，已删除，请重新运行安装脚本。" `
               -en "❌ Archive corrupted, deleted. Re-run the install script." -Color Red
    Remove-Item -LiteralPath $ZipPath -Force -ErrorAction SilentlyContinue
    throw "Polaris archive is corrupt."
}

$ExtractDir = "$WorkDir\extracted"
if (Test-Path -LiteralPath $ExtractDir) { Remove-Item -LiteralPath $ExtractDir -Recurse -Force }

Expand-Archive -LiteralPath $ZipPath -DestinationPath $ExtractDir -Force
Remove-Item -LiteralPath $ZipPath -Force

# 复制解压目录内容到安装目录（处理可能存在的一层包裹目录）
$RootItems = @(Get-ChildItem -LiteralPath $ExtractDir)
if ($RootItems.Count -eq 1 -and $RootItems[0].PSIsContainer) {
    $SrcDir = $RootItems[0].FullName
} else {
    $SrcDir = $ExtractDir
}
$NewExe = Join-Path $SrcDir $BinName
if (-not (Test-Path -LiteralPath $NewExe)) { throw "Archive does not contain $BinName; existing installation preserved." }
& $NewExe version | Out-Null
if ($LASTEXITCODE -ne 0) { throw "New binary cannot run on this OS; existing installation preserved." }

# 下载、校验及预检成功后才停止旧任务，避免下载失败造成服务停机。
try { & schtasks /end /tn $TaskName 2>$null | Out-Null } catch { }
# 桌面外壳也会占用安装目录；只停止本安装目录里的进程，避免影响其他实例。
$InstallPrefix = $InstallDir.TrimEnd('\') + '\'
$OldProc = @(Get-Process -Name 'polaris', 'polaris-desktop*' -ErrorAction SilentlyContinue | Where-Object {
    $_.Path -and $_.Path.StartsWith($InstallPrefix, [System.StringComparison]::OrdinalIgnoreCase)
})
if ($OldProc.Count) {
    $OldProc | Stop-Process -Force -ErrorAction Stop
    $OldProc | Wait-Process -Timeout 10 -ErrorAction Stop
}
Get-ChildItem -LiteralPath $SrcDir -Force | ForEach-Object {
    Copy-Item -LiteralPath $_.FullName -Destination $InstallDir -Recurse -Force
}
$FinalExe = "$InstallDir\$BinName"
Remove-Item -LiteralPath $ExtractDir -Recurse -Force -ErrorAction SilentlyContinue

if (-not (Test-Path -LiteralPath $FinalExe)) {
    Write-Msg -zh "❌ 归档中未找到 $BinName。" -en "❌ $BinName not found in archive." -Color Red
    throw "Installed Polaris executable is missing."
}

Unblock-File -LiteralPath $FinalExe -ErrorAction SilentlyContinue

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
    $ServiceReady = $true
    Write-Msg -zh "✅ 服务已注册并启动，登录时自动运行。" `
               -en "✅ Service registered and started, auto-starts on login." -Color Green
} else {
    Write-Msg -zh "⚠️  服务注册失败，可稍后手动执行：$FinalExe service install" `
               -en "⚠️  Service registration failed. Run manually later: $FinalExe service install" -Color Yellow
}

# ── 6.5 桌面外壳安装 ─────────────────────────────────────────────────────────
# 统一归档内含 desktop/ 目录（有桌面版的平台才存在）。将桌面外壳 exe 保留在
# 安装目录内，并创建开始菜单快捷方式使其可被发现。
$DesktopSrc = "$InstallDir\desktop"
if (Test-Path -LiteralPath $DesktopSrc) {
    $DesktopExe = Get-ChildItem -LiteralPath $DesktopSrc -Filter "*.exe" | Select-Object -First 1
    if ($DesktopExe) {
        # 创建开始菜单快捷方式
        $StartMenuDir = "$env:APPDATA\Microsoft\Windows\Start Menu\Programs"
        $ShortcutPath = "$StartMenuDir\Polaris.lnk"
        try {
            $WScriptShell = New-Object -ComObject WScript.Shell
            $Shortcut = $WScriptShell.CreateShortcut($ShortcutPath)
            $Shortcut.TargetPath = $DesktopExe.FullName
            $Shortcut.WorkingDirectory = $InstallDir
            $Shortcut.Description = "Polaris AI Agent Desktop"
            $Shortcut.Save()
            Write-Msg -zh "✅ 桌面外壳已安装，开始菜单快捷方式已创建。" `
                       -en "✅ Desktop shell installed, Start Menu shortcut created." -Color Green
        } catch {
            Write-Msg -zh "⚠️  快捷方式创建失败，桌面外壳仍可在 $($DesktopExe.FullName) 直接运行。" `
                       -en "⚠️  Shortcut creation failed. Desktop shell is still at $($DesktopExe.FullName)" -Color Yellow
        }
    }
}

# ── 7. 等待就绪 ──────────────────────────────────────────────────────────────
# 不再额外 Start-Process：service install 已经启动了守护进程，重复启动会被单实例
# 锁拒绝，在日志里留下一条看起来像故障的 ALREADY_EXISTS。
$ConsoleUrl = ""
if ($ServiceReady) {
    $ServiceReady = $false
    for ($Attempt = 0; $Attempt -lt 30; $Attempt++) {
        try {
            $rt = & $FinalExe service status --json | ConvertFrom-Json
            if ($rt.running -and $rt.base_url) {
                $ConsoleUrl = $rt.base_url
                $ServiceReady = $true
                break
            }
        } catch { }
        Start-Sleep -Seconds 1
    }
}
Write-Host ""
if ($DepsReady -and $ServiceReady) {
    Write-Msg -zh "🎉 安装完成，后台服务已就绪。" -en "🎉 Installation complete; service is ready." -Color Green
} else {
    Write-Msg -zh "⚠️ 主程序已安装，但依赖或后台服务未就绪。可运行 polaris serve 查看错误，处理后重跑安装。" `
               -en "⚠️ Core installed, but dependencies or service need attention. Run polaris serve for diagnostics, then rerun." -Color Yellow
}
if (-not $ConsoleUrl) { $ConsoleUrl = "http://127.0.0.1:$Port" }
Write-Msg -zh "   请访问: $ConsoleUrl" -en "   Visit: $ConsoleUrl" -Color Yellow
Write-Host ""
if (-not ($DepsReady -and $ServiceReady)) { throw "Polaris installation incomplete; see warnings above." }
} finally {
    Remove-Item -LiteralPath $WorkDir -Recurse -Force -ErrorAction SilentlyContinue
    $ProgressPreference = $PreviousProgressPreference
    $ErrorActionPreference = $PreviousErrorActionPreference
}
