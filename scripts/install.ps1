# 保持 UTF-8 无 BOM，避免 irm | iex 将 BOM 当作首条命令的一部分。
$PreviousErrorActionPreference = $ErrorActionPreference
$PreviousProgressPreference = $ProgressPreference
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
# 确保 Windows PowerShell 5.1 默认启用 TLS 1.2，兼容固定 HTTPS 下载入口。
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
$TaskName      = "PolarisAGI-Polaris"
$NodeVersion   = "24"
$DepsReady     = $true
$ServiceReady  = $false
$ServiceRegistered = $false
$ServiceLaunchAttempted = $false
$script:VisualCInstallAttempted = $false
$script:PreferredNodeDir = ''
$script:PreferredPythonDir = ''

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
        $script:PreferredNodeDir,
        $script:PreferredPythonDir,
        $InstallDir,
        $LocalBinDir,
        $CargoBinDir,
        "$DepsDir\node",
        "$DepsDir\uv",
        "$DepsDir\ffmpeg\bin",
        "$DepsDir\git\cmd",
        "$DepsDir\git\bin",
        "$env:ProgramFiles\Git\cmd",
        "$env:ProgramFiles\Git\bin",
        "${env:ProgramFiles(x86)}\Git\cmd",
        "${env:ProgramFiles(x86)}\Git\bin",
        "$env:LOCALAPPDATA\Programs\Git\cmd",
        "$env:LOCALAPPDATA\Programs\Git\bin",
        "$env:ProgramFiles\nodejs",
        "$env:USERPROFILE\scoop\shims",
        "$env:LOCALAPPDATA\Microsoft\WinGet\Links",
        "$env:APPDATA\Python\Python312\Scripts",
        "$env:LOCALAPPDATA\Programs\Python\Python312",
        "$env:LOCALAPPDATA\Programs\Python\Python312\Scripts",
        "$env:ProgramFiles\Python312",
        "$env:ProgramFiles\Python312\Scripts"
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
$ProxyHosts  = @('https://ghproxy.net', 'https://mirror.ghproxy.com')
$DownloadBase = $env:POLARIS_DOWNLOAD_BASE
if ($DownloadBase) {
    $DownloadUri = $null
    if (-not [Uri]::TryCreate($DownloadBase, [UriKind]::Absolute, [ref]$DownloadUri) -or
        $DownloadUri.Scheme -ne 'https' -or -not $DownloadUri.Host -or $DownloadUri.Query -or $DownloadUri.Fragment) {
        throw 'POLARIS_DOWNLOAD_BASE must be a valid HTTPS base URL without query or fragment.'
    }
    $DownloadBase = $DownloadBase.TrimEnd('/')
}
if ($env:UV_PYTHON_INSTALL_MIRROR) {
    $PythonMirrorUri = $null
    if (-not [Uri]::TryCreate($env:UV_PYTHON_INSTALL_MIRROR, [UriKind]::Absolute, [ref]$PythonMirrorUri) -or $PythonMirrorUri.Scheme -ne 'https') {
        throw 'UV_PYTHON_INSTALL_MIRROR must be an HTTPS URL.'
    }
}

$Region = if ($env:POLARIS_REGION) { $env:POLARIS_REGION.ToLowerInvariant() } else { 'auto' }
if ($Region -notin @('auto', 'cn', 'global')) { throw 'POLARIS_REGION must be auto/cn/global' }
$IsMainland = ($Region -eq 'cn')
Write-Msg -zh "🌐 下载源偏好：$Region" -en "🌐 Download source preference: $Region" -Color Cyan
$CandidateURLs = [System.Collections.ArrayList]::new()
if ($DownloadBase) { $CandidateURLs.Add("$DownloadBase/$ArchiveName") | Out-Null }
if ($Region -eq 'cn') {
    foreach ($Proxy in $ProxyHosts) { $CandidateURLs.Add("$Proxy/$GithubURL") | Out-Null }
    $CandidateURLs.Add($GithubURL) | Out-Null
} else {
    $CandidateURLs.Add($GithubURL) | Out-Null
    foreach ($Proxy in $ProxyHosts) { $CandidateURLs.Add("$Proxy/$GithubURL") | Out-Null }
}
if ($Region -eq 'auto') {
    $ProbePath = Join-Path ([System.IO.Path]::GetTempPath()) ("polaris-source-probe-" + [guid]::NewGuid().ToString('N'))
    $ProbedURLs = New-Object 'System.Collections.Generic.List[string]'
    try {
        foreach ($Url in $CandidateURLs) {
            try {
                Invoke-WebRequest -Uri "$Url.sha256" -OutFile $ProbePath -UseBasicParsing -TimeoutSec 6 -ErrorAction Stop
                $ProbedURLs.Add($Url)
            } catch { }
            Remove-Item -LiteralPath $ProbePath -Force -ErrorAction SilentlyContinue
        }
        $OrderedURLs = New-Object 'System.Collections.ArrayList'
        $PreferredUrl = if ($DownloadBase) { "$DownloadBase/$ArchiveName" } else { $null }
        if ($PreferredUrl) { $OrderedURLs.Add($PreferredUrl) | Out-Null }
        if ($ProbedURLs.Count) {
            foreach ($Url in $ProbedURLs) { if ($Url -ne $PreferredUrl -and -not $OrderedURLs.Contains($Url)) { $OrderedURLs.Add($Url) | Out-Null } }
            foreach ($Url in $CandidateURLs) { if ($Url -ne $PreferredUrl -and -not $OrderedURLs.Contains($Url)) { $OrderedURLs.Add($Url) | Out-Null } }
        } else {
            foreach ($Proxy in $ProxyHosts) { $OrderedURLs.Add("$Proxy/$GithubURL") | Out-Null }
            $OrderedURLs.Add($GithubURL) | Out-Null
        }
        $CandidateURLs = $OrderedURLs
    } finally { Remove-Item -LiteralPath $ProbePath -Force -ErrorAction SilentlyContinue }
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
                    if ($LASTEXITCODE -eq 0) {
                        $script:WorkingPythonExe = $c.Source
                        $script:PreferredPythonDir = Split-Path $c.Source -Parent
                        Update-SessionPath
                        return $true
                    }
                } catch { }
            }
        }
    }
    return $false
}

function Test-WorkingNode {
    Update-SessionPath
    foreach ($NodeCmd in @(Get-Command 'node' -All -ErrorAction SilentlyContinue)) {
        try {
            $NodeDir = Split-Path $NodeCmd.Source -Parent
            $NpmPath = Join-Path $NodeDir 'npm.cmd'
            $NpxPath = Join-Path $NodeDir 'npx.cmd'
            if (-not (Test-Path -LiteralPath $NpmPath)) {
                $NpmCommand = Get-Command 'npm.cmd' -All -ErrorAction SilentlyContinue | Select-Object -First 1
                if ($NpmCommand) { $NpmPath = $NpmCommand.Source }
            }
            if (-not (Test-Path -LiteralPath $NpxPath)) {
                $NpxCommand = Get-Command 'npx.cmd' -All -ErrorAction SilentlyContinue | Select-Object -First 1
                if ($NpxCommand) { $NpxPath = $NpxCommand.Source }
            }
            if (-not (Test-Path -LiteralPath $NpmPath) -and -not (Get-Command 'npm.cmd' -ErrorAction SilentlyContinue)) { continue }
            if (-not (Test-Path -LiteralPath $NpxPath) -and -not (Get-Command 'npx.cmd' -ErrorAction SilentlyContinue)) { continue }
            $null = & $NodeCmd.Source -e 'process.exit(parseInt(process.versions.node,10)>=22?0:1)' 2>$null
            if ($LASTEXITCODE -ne 0) { continue }
            $null = & $NpmPath --version 2>$null
            if ($LASTEXITCODE -ne 0) { continue }
            $null = & $NpxPath --version 2>$null
            if ($LASTEXITCODE -eq 0) {
                $script:WorkingNodeExe = $NodeCmd.Source
                $script:WorkingNpmCmd = $NpmPath
                $script:WorkingNpxCmd = $NpxPath
                $script:PreferredNodeDir = $NodeDir
                Update-SessionPath
                return $true
            }
        } catch { }
    }
    return $false
}

function Get-NativePowerShell {
    $Relative = 'System32\WindowsPowerShell\v1.0\powershell.exe'
    if ($env:PROCESSOR_ARCHITEW6432) { $Relative = 'Sysnative\WindowsPowerShell\v1.0\powershell.exe' }
    return (Join-Path $env:WINDIR $Relative)
}

function Invoke-PolarisLibraryProbe {
    param([string[]]$Libraries)
    $Files = ($Libraries | ForEach-Object { "'" + $_.Replace("'", "''") + "'" }) -join ','
    # 子进程按本机位数加载，避免 WOW64 误报，并在退出时释放所有 DLL 句柄。
    $Code = @'
$ErrorActionPreference = 'Stop'
try {
    Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class PolarisNativeProbe {
    [DllImport("kernel32.dll")] public static extern uint SetErrorMode(uint mode);
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true, EntryPoint="LoadLibraryExW")]
    public static extern IntPtr Load(string path, IntPtr reserved, uint flags);
}
"@
    [void][PolarisNativeProbe]::SetErrorMode(0x8001)
    foreach ($File in @(__FILES__)) {
        $Handle = [PolarisNativeProbe]::Load($File, [IntPtr]::Zero, 0x1100)
        if ($Handle -eq [IntPtr]::Zero) {
            $ErrorCode = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
            @{ready=$false; code=$ErrorCode; detail="$File : $([ComponentModel.Win32Exception]::new($ErrorCode).Message)"} | ConvertTo-Json -Compress
            exit 1
        }
    }
    @{ready=$true; code=0; detail=''} | ConvertTo-Json -Compress
} catch {
    @{ready=$false; code=-1; detail=$_.Exception.Message} | ConvertTo-Json -Compress
    exit 1
}
'@
    $Encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($Code.Replace('__FILES__', $Files)))
    $Shell = Get-NativePowerShell
    $Output = & $Shell -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand $Encoded
    $ExitCode = $LASTEXITCODE
    $Result = ($Output -join "`n") | ConvertFrom-Json -ErrorAction Stop
    if ($null -eq $Result -or $Result.ready -isnot [bool] -or ($ExitCode -ne 0 -and $Result.ready)) { throw 'Invalid native library probe result.' }
    return $Result
}

function Test-WorkingVisualC {
    $NativeArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $Names = @('vcruntime140.dll', 'msvcp140.dll')
    if ($NativeArch -eq 'AMD64') { $Names += 'vcruntime140_1.dll' }
    $SystemDir = if ($env:PROCESSOR_ARCHITEW6432) { 'Sysnative' } else { 'System32' }
    $Files = @($Names | ForEach-Object { Join-Path (Join-Path $env:WINDIR $SystemDir) $_ })
    foreach ($File in $Files) {
        if (-not (Test-Path -LiteralPath $File -PathType Leaf)) { return $false }
    }
    return (Invoke-PolarisLibraryProbe @($Files | ForEach-Object { $_.Replace('\Sysnative\', '\System32\') })).ready
}

function Install-MicrosoftComponent {
    param([string]$Name, [string]$Uri, [string]$Arguments, [string]$ReadyFunction)
    $Setup = Join-Path $WorkDir ($Name + '-setup.exe')
    Invoke-WebRequest -Uri $Uri -OutFile $Setup -UseBasicParsing -TimeoutSec 180 -ErrorAction Stop
    $Signature = Get-AuthenticodeSignature -LiteralPath $Setup
    if ($Signature.Status -ne 'Valid' -or $Signature.SignerCertificate.Subject -notmatch 'O=Microsoft Corporation(?:,|$)') {
        throw "$Name installer Microsoft signature verification failed."
    }
    $Process = Start-Process -FilePath $Setup -ArgumentList $Arguments -PassThru -ErrorAction Stop
    $null = $Process.Handle
    if (-not $Process.WaitForExit(300000)) {
        $Process.Kill()
        throw "$Name installation timed out."
    }
    # 安装器退出码不能代替运行验证；3010/1638 仅在组件已实际可用时接受。
    if ($Process.ExitCode -notin @(0, 3010, 1638) -or -not (& $ReadyFunction)) {
        throw "$Name installation is not ready (exit code $($Process.ExitCode)); a Windows restart may be required."
    }
}

function Install-VisualCRuntime {
    param([switch]$Repair)
    if (-not $Repair -and (Test-WorkingVisualC)) { return }
    $NativeArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $VCArch = switch ($NativeArch) { 'AMD64' { 'x64' } 'ARM64' { 'arm64' } default { throw "Unsupported runtime architecture: $NativeArch" } }
    Write-Msg -zh '⬇️ 正在补齐微软 VC++ 运行库（Windows 可能显示授权提示）...' -en '⬇️ Installing Microsoft VC++ runtime (Windows may request authorization)...' -Color Cyan
    $script:VisualCInstallAttempted = $true
    Install-MicrosoftComponent -Name 'VC-runtime' -Uri "https://aka.ms/vc14/vc_redist.$VCArch.exe" -Arguments '/install /passive /norestart' -ReadyFunction 'Test-WorkingVisualC'
}

function Assert-PolarisNativeRuntime {
    param([string]$Root)
    $Library = Join-Path $Root 'lib\substrate.dll'
    if (-not (Test-Path -LiteralPath $Library -PathType Leaf)) { throw 'Polaris archive does not contain substrate.dll; existing installation preserved.' }
    $Result = Invoke-PolarisLibraryProbe @($Library)
    # 已有 CRT 可能过旧；仅依赖/导出缺失尝试补齐最新版，不把架构错误当缺运行库。
    if (-not $Result.ready -and $Result.code -in @(126, 127) -and -not $script:VisualCInstallAttempted) {
        Install-VisualCRuntime -Repair
        $Result = Invoke-PolarisLibraryProbe @($Library)
    }
    if (-not $Result.ready) { throw "Polaris native runtime cannot load: $($Result.detail); existing installation preserved." }
}

Install-VisualCRuntime

function Test-WorkingBash {
    Update-SessionPath
    $Commands = @(Get-Command 'bash.exe' -All -ErrorAction SilentlyContinue)
    foreach ($Command in $Commands) {
        try {
            $null = & $Command.Source -c 'exit 0' 2>$null
            if ($LASTEXITCODE -eq 0) { return $true }
        } catch { }
    }
    return $false
}

function Expand-PortableGit {
    param([string]$Archive, [string]$Destination)
    & $Archive '-y' ("-o" + $Destination) | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Portable Git extraction failed (exit code $LASTEXITCODE)." }
}

# 包管理器复用用户已有安装；缺少时固定资产仍提供后备。
function Install-DependencyPackage {
    param([string]$WingetId, [string]$ScoopId, [string]$ChocoId)
    foreach ($Manager in @('winget', 'scoop', 'choco')) {
        if (-not (Get-Command $Manager -ErrorAction SilentlyContinue)) { continue }
        try {
            switch ($Manager) {
                winget { & winget install --id $WingetId -e --source winget --accept-package-agreements --accept-source-agreements --silent --disable-interactivity | Out-Host }
                scoop { & scoop install $ScoopId | Out-Host }
                choco { & choco install $ChocoId -y --no-progress | Out-Host }
            }
            if ($LASTEXITCODE -eq 0) { Update-SessionPath; return $true }
            Write-Msg -zh "⚠️ $Manager 安装 $WingetId 失败：$LASTEXITCODE" -en "⚠️ $Manager failed to install ${WingetId}: $LASTEXITCODE" -Color Yellow
        } catch { Write-Msg -zh "⚠️ $Manager 安装失败：$($_.Exception.Message)" -en "⚠️ $Manager failed: $($_.Exception.Message)" -Color Yellow }
    }
    return $false
}

function Get-PortableArtifactUrls {
    param([string]$Name, [string]$OfficialUrl, [string]$MainlandUrl = '')
    $Urls = New-Object 'System.Collections.Generic.List[string]'
    if ($DownloadBase) { $Urls.Add("$DownloadBase/$Name") }
    if ($MainlandUrl) {
        if ($IsMainland) { $Urls.Add($MainlandUrl); $Urls.Add($OfficialUrl) }
        else { $Urls.Add($OfficialUrl); $Urls.Add($MainlandUrl) }
    } elseif ($OfficialUrl -match '^https://github\.com/') {
        foreach ($Proxy in $ProxyHosts) { $Urls.Add("$Proxy/$OfficialUrl") }
        $Urls.Add($OfficialUrl)
    } else { $Urls.Add($OfficialUrl) }
    if ($Region -eq 'auto' -and $Urls.Count -gt 1) {
        $PreferredUrl = if ($DownloadBase) { "$DownloadBase/$Name" } else { $null }
        $Probed = New-Object 'System.Collections.Generic.List[string]'
        foreach ($Url in $Urls) {
            try {
                Invoke-WebRequest -Uri $Url -Method Head -UseBasicParsing -TimeoutSec 5 -ErrorAction Stop | Out-Null
                $Probed.Add($Url)
            } catch { }
        }
        foreach ($Url in $Urls) { if ($Url -ne $PreferredUrl -and -not $Probed.Contains($Url)) { $Probed.Add($Url) } }
        if ($PreferredUrl -and -not $Probed.Contains($PreferredUrl)) { $Probed.Insert(0, $PreferredUrl) }
        return @($Probed)
    }
    return @($Urls | Select-Object -Unique)
}

function Receive-VerifiedArtifact {
    param([string[]]$Urls, [string]$Destination, [string]$Sha256)
    foreach ($Url in $Urls) {
        try {
            Remove-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
            Invoke-WebRequest -Uri $Url -OutFile $Destination -UseBasicParsing -TimeoutSec 300 -ErrorAction Stop
            $Actual = (Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($Actual -ne $Sha256.ToLowerInvariant()) { throw 'SHA-256 mismatch' }
            return $true
        } catch {
            Write-Msg -zh "⚠️ 下载源失败：$Url ($($_.Exception.Message))" -en "⚠️ Download source failed: $Url ($($_.Exception.Message))" -Color Yellow
        }
    }
    return $false
}

function Replace-DependencyDirectory {
    param([string]$Staged, [string]$Destination, [scriptblock]$OnInstall, [scriptblock]$Validate)
    $Backup = "$Destination.previous"
    $PreviousUserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $PreviousSessionPath = $env:Path
    if (Test-Path -LiteralPath $Backup) { Remove-Item -LiteralPath $Backup -Recurse -Force }
    if (Test-Path -LiteralPath $Destination) { Move-Item -LiteralPath $Destination -Destination $Backup }
    try {
        Move-Item -LiteralPath $Staged -Destination $Destination
        if ($OnInstall) { & $OnInstall }
        if ($Validate -and -not (& $Validate)) { throw 'Installed dependency failed its runtime check' }
        if (Test-Path -LiteralPath $Backup) { Remove-Item -LiteralPath $Backup -Recurse -Force }
    } catch {
        if (Test-Path -LiteralPath $Destination) { Remove-Item -LiteralPath $Destination -Recurse -Force }
        if ((Test-Path -LiteralPath $Backup) -and -not (Test-Path -LiteralPath $Destination)) {
            Move-Item -LiteralPath $Backup -Destination $Destination
        }
        try { [Environment]::SetEnvironmentVariable('Path', $PreviousUserPath, 'User') } catch { }
        $env:Path = $PreviousSessionPath
        throw
    }
}

function Get-FixedArtifact {
    param([string]$Kind, [string]$NativeArch)
    switch ("$Kind/$NativeArch") {
        'git/amd64' { return [pscustomobject]@{Name='PortableGit-2.56.1-64-bit.7z.exe'; Url='https://github.com/git-for-windows/git/releases/download/v2.56.1.windows.1/PortableGit-2.56.1-64-bit.7z.exe'; Mainland='https://registry.npmmirror.com/-/binary/git-for-windows/v2.56.1.windows.1/PortableGit-2.56.1-64-bit.7z.exe'; Hash='9dd04a608ceb5f6ab32a73709e0ecb159d211da71b697c45ff81f6dd0c6b49c7'} }
        'git/arm64' { return [pscustomobject]@{Name='PortableGit-2.56.1-arm64.7z.exe'; Url='https://github.com/git-for-windows/git/releases/download/v2.56.1.windows.1/PortableGit-2.56.1-arm64.7z.exe'; Mainland='https://registry.npmmirror.com/-/binary/git-for-windows/v2.56.1.windows.1/PortableGit-2.56.1-arm64.7z.exe'; Hash='0dca0d9f760924a7e6752235d7dd6eb1a9b508ee714b6ed15d89252e5a58c5c9'} }
        'uv-wheel/amd64' { return [pscustomobject]@{Name='uv-0.13.7-py3-none-win_amd64.whl'; Url='https://files.pythonhosted.org/packages/8e/4f/041b59d2a38807a3e31ef41064cf3e45d2752fe0409d89040d52778049ae/uv-0.13.7-py3-none-win_amd64.whl'; Mainland='https://mirrors.aliyun.com/pypi/packages/8e/4f/041b59d2a38807a3e31ef41064cf3e45d2752fe0409d89040d52778049ae/uv-0.13.7-py3-none-win_amd64.whl'; Hash='2835e2ab397d0db86323b9975dce75ecd76aa9cfa0d6a2aea84734aae7f8c9b3'} }
        'uv-wheel/arm64' { return [pscustomobject]@{Name='uv-0.13.7-py3-none-win_arm64.whl'; Url='https://files.pythonhosted.org/packages/44/8d/1594dce4465d9699467a639b25aafd335ce1e5917f8bb60c9c0bed9e0859/uv-0.13.7-py3-none-win_arm64.whl'; Mainland='https://mirrors.aliyun.com/pypi/packages/44/8d/1594dce4465d9699467a639b25aafd335ce1e5917f8bb60c9c0bed9e0859/uv-0.13.7-py3-none-win_arm64.whl'; Hash='55e741a380a98008fc4985cf069855f36bc50242ef438e21a466786f5bc84da8'} }
        'node/amd64' { return [pscustomobject]@{Name='node-v24.22.3-win-x64.zip'; Url='https://nodejs.org/dist/v24.22.3/node-v24.22.3-win-x64.zip'; Mainland='https://npmmirror.com/mirrors/node/v24.22.3/node-v24.22.3-win-x64.zip'; Hash='e612af99f626ab415ea225af6bce5c971feb448cb5cae250bb1c3fa50c5f7d9f'} }
        'node/arm64' { return [pscustomobject]@{Name='node-v24.22.3-win-arm64.zip'; Url='https://nodejs.org/dist/v24.22.3/node-v24.22.3-win-arm64.zip'; Mainland='https://npmmirror.com/mirrors/node/v24.22.3/node-v24.22.3-win-arm64.zip'; Hash='b3cc74a295b39d4f273271e96f58c06dd2b35757ca12abec64c7d0598b434f14'} }
        'ffmpeg/amd64' { return [pscustomobject]@{Name='ffmpeg-9.0.2-essentials_build.zip'; Url='https://www.gyan.dev/ffmpeg/builds/packages/ffmpeg-9.0.2-essentials_build.zip'; Hash='60f467265b1e312373dbcd92200c2618a74850f98d3d078e94296bb3fa2047ba'} }
        'ffmpeg-fallback/amd64' { return [pscustomobject]@{Name='ffmpeg-N-128227-g4811625b5f-win64-lgpl.zip'; Url='https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/autobuild-2026-10-31-12-59/ffmpeg-N-128227-g4811625b5f-win64-lgpl.zip'; Hash='6550f7abc950128fbb0069b4552f2972baccf5fbd819f2fd0abbe4ceb8d48734'} }
        'ffmpeg/arm64' { return [pscustomobject]@{Name='ffmpeg-N-128227-g4811625b5f-winarm64-lgpl.zip'; Url='https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/autobuild-2026-10-31-12-59/ffmpeg-N-128227-g4811625b5f-winarm64-lgpl.zip'; Hash='742afe9b16d5aff391180d2f6be836b32e904f17ee5f304af7733642395582bd'} }
    }
    return $null
}

function Test-PortableGitRuntime {
    param([string]$Root)
    foreach ($Exe in @('cmd/git.exe', 'bin/bash.exe')) {
        & (Join-Path $Root $Exe) --version | Out-Null
        if ($LASTEXITCODE -ne 0) { return $false }
    }
    return $true
}

function Test-PortableUvRuntime {
    param([string]$Root)
    foreach ($Name in @('uv.exe', 'uvx.exe')) {
        & (Join-Path $Root $Name) --version | Out-Null
        if ($LASTEXITCODE -ne 0) { return $false }
    }
    return $true
}

function Test-PortableNodeRuntime {
    param([string]$Root)
    & (Join-Path $Root 'node.exe') --version | Out-Null
    if ($LASTEXITCODE -ne 0) { return $false }
    foreach ($Name in @('npm.cmd', 'npx.cmd')) {
        & (Join-Path $Root $Name) --version | Out-Null
        if ($LASTEXITCODE -ne 0) { return $false }
    }
    return $true
}

function Test-PortableFFmpegRuntime {
    param([string]$BinDirectory)
    foreach ($Name in @('ffmpeg.exe', 'ffprobe.exe')) {
        $Exe = Join-Path $BinDirectory $Name
        if (-not (Test-Path -LiteralPath $Exe)) { return $false }
        & $Exe -version | Out-Null
        if ($LASTEXITCODE -ne 0) { return $false }
    }
    return $true
}

function Install-PortableGit {
    $Artifact = Get-FixedArtifact 'git' $Arch
    $Urls = Get-PortableArtifactUrls $Artifact.Name $Artifact.Url $Artifact.Mainland
    $Archive = Join-Path $WorkDir $Artifact.Name
    if (-not (Receive-VerifiedArtifact $Urls $Archive $Artifact.Hash)) { return $false }
    $Stage = Join-Path $WorkDir 'git-extract'
    try {
        if (Test-Path -LiteralPath $Stage) { Remove-Item -LiteralPath $Stage -Recurse -Force }
        Expand-PortableGit -Archive $Archive -Destination $Stage
        foreach ($Required in @('cmd/git.exe', 'bin/bash.exe')) {
            if (-not (Test-Path -LiteralPath (Join-Path $Stage $Required))) { throw "Portable Git archive missing $Required" }
        }
        if (-not (Test-PortableGitRuntime $Stage)) { throw 'Portable Git runtime check failed' }
        $Destination = Join-Path $DepsDir 'git'
        Replace-DependencyDirectory $Stage $Destination { Add-UserPathFront (Join-Path $Destination 'cmd'); Add-UserPathFront (Join-Path $Destination 'bin'); Update-SessionPath } { (Test-WorkingGit) -and (Test-WorkingBash) }
        Add-UserPathFront -DirToAdd (Join-Path $Destination 'cmd')
        Add-UserPathFront -DirToAdd (Join-Path $Destination 'bin')
        return $true
    } catch {
        Write-Msg -zh "⚠️ PortableGit 安装失败：$($_.Exception.Message)" -en "⚠️ PortableGit installation failed: $($_.Exception.Message)" -Color Yellow
        return $false
    } finally { Remove-Item -LiteralPath $Archive -Force -ErrorAction SilentlyContinue }
}

function Install-PortableUv {
    $Artifact = Get-FixedArtifact 'uv-wheel' $Arch
    if (-not $Artifact) { return $false }
    $Stage = Join-Path $WorkDir 'uv-extract'
    $Urls = Get-PortableArtifactUrls $Artifact.Name $Artifact.Url
    $Archive = Join-Path $WorkDir 'uv-wheel.zip'
    if (-not (Receive-VerifiedArtifact $Urls $Archive $Artifact.Hash)) { return $false }
    try {
        if (Test-Path -LiteralPath $Stage) { Remove-Item -LiteralPath $Stage -Recurse -Force }
        Expand-Archive -LiteralPath $Archive -DestinationPath $Stage -Force
        if ($Artifact.Name -like '*.whl') {
            $ScriptsDirectory = Get-ChildItem -LiteralPath $Stage -Directory -Recurse | Where-Object { $_.Name -eq 'scripts' -and $_.Parent.Name -like '*.data' } | Select-Object -First 1
            if (-not $ScriptsDirectory) { throw 'uv wheel missing .data/scripts directory' }
            foreach ($Name in @('uv.exe', 'uvw.exe', 'uvx.exe')) {
                $Source = Join-Path $ScriptsDirectory.FullName $Name
                if (-not (Test-Path -LiteralPath $Source)) { throw "uv wheel missing $Name" }
                Copy-Item -LiteralPath $Source -Destination (Join-Path $Stage $Name) -Force
            }
        }
        foreach ($Name in @('uv.exe', 'uvx.exe')) {
            $Exe = Join-Path $Stage $Name
            if (-not (Test-Path -LiteralPath $Exe)) { throw "uv archive missing $Name" }
        }
        if (-not (Test-PortableUvRuntime $Stage)) { throw 'uv runtime check failed' }
        $Destination = Join-Path $DepsDir 'uv'
        Replace-DependencyDirectory $Stage $Destination { Add-UserPathFront $Destination; Update-SessionPath } { Test-WorkingUv }
        return (Test-WorkingUv)
    } catch {
        Write-Msg -zh "⚠️ uv 安装失败：$($_.Exception.Message)" -en "⚠️ uv installation failed: $($_.Exception.Message)" -Color Yellow
        return $false
    } finally { Remove-Item -LiteralPath $Archive -Force -ErrorAction SilentlyContinue }
}

function Install-PortableNode {
    $Artifact = Get-FixedArtifact 'node' $Arch
    $Urls = Get-PortableArtifactUrls $Artifact.Name $Artifact.Url $Artifact.Mainland
    $Archive = Join-Path $WorkDir $Artifact.Name
    if (-not (Receive-VerifiedArtifact $Urls $Archive $Artifact.Hash)) { return $false }
    $Stage = Join-Path $WorkDir 'node-extract'
    try {
        if (Test-Path -LiteralPath $Stage) { Remove-Item -LiteralPath $Stage -Recurse -Force }
        Expand-Archive -LiteralPath $Archive -DestinationPath $Stage -Force
        $Inner = Get-ChildItem -LiteralPath $Stage -Directory | Select-Object -First 1
        if (-not $Inner) { throw 'Node archive has no root directory' }
        foreach ($Name in @('node.exe', 'npm.cmd', 'npx.cmd')) {
            if (-not (Test-Path -LiteralPath (Join-Path $Inner.FullName $Name))) { throw "Node archive missing $Name" }
        }
        if (-not (Test-PortableNodeRuntime $Inner.FullName)) { throw 'Node/npm/npx runtime check failed' }
        $Ready = Join-Path $WorkDir 'node-ready'
        Move-Item -LiteralPath $Inner.FullName -Destination $Ready
        $Destination = Join-Path $DepsDir 'node'
        Replace-DependencyDirectory $Ready $Destination { Add-UserPathFront $Destination; Update-SessionPath } { Test-WorkingNode }
        Add-UserPathFront -DirToAdd $Destination
        Update-SessionPath
        return (Test-WorkingNode)
    } catch {
        Write-Msg -zh "⚠️ Node.js 安装失败：$($_.Exception.Message)" -en "⚠️ Node.js installation failed: $($_.Exception.Message)" -Color Yellow
        return $false
    } finally { Remove-Item -LiteralPath $Archive -Force -ErrorAction SilentlyContinue }
}

# 1. Git 检查与固定摘要便携归档
if (-not (Test-WorkingGit) -or -not (Test-WorkingBash)) {
    Write-Msg -zh "   📦 缺少 Git 或 Bash，正在从固定下载源安装..." -en "   📦 Git or Bash missing; downloading from fixed source..." -Color Gray
    if (-not (Test-WorkingGit)) { $null = Install-DependencyPackage 'Git.Git' 'git' 'git' }
    if ((Test-WorkingGit) -and (Test-WorkingBash)) { Update-SessionPath }
    else { $null = Install-PortableGit }
    if (-not (Test-WorkingGit) -or -not (Test-WorkingBash)) {
        Write-Msg -zh "⚠️ Git/Bash 自动安装未完成；请检查网络后重跑安装。" -en "⚠️ Git/Bash auto-install failed; check the network and rerun setup." -Color Yellow
    }
}

# uv wheel 避免引导依赖 Python；先复用已有包管理器。
if (-not (Test-WorkingUv)) {
    $null = Install-DependencyPackage 'astral-sh.uv' 'uv' 'uv'
    if (-not (Test-WorkingUv)) { $null = Install-PortableUv }
}
if (-not (Test-WorkingPython) -and (Test-WorkingUv)) {
    try {
        $ManagedPython = (& uv python find --managed-python 3.12 2>$null | Select-Object -First 1)
        if ($LASTEXITCODE -eq 0 -and $ManagedPython -and (Test-Path -LiteralPath $ManagedPython)) {
            $script:WorkingPythonExe = "$ManagedPython".Trim()
            $script:PreferredPythonDir = Split-Path $script:WorkingPythonExe -Parent
            Update-SessionPath
        }
    } catch { }
}
if (-not (Test-WorkingPython)) { $null = Install-DependencyPackage 'Python.Python.3.12' 'python' 'python312' }
function Install-ManagedPython {
    $SavedPythonMirror = $env:UV_PYTHON_INSTALL_MIRROR
    $Mirrors = if ($SavedPythonMirror) { @($SavedPythonMirror) } elseif ($Region -eq 'global') {
        @('', 'https://registry.npmmirror.com/-/binary/python-build-standalone', 'https://ghproxy.net/https://github.com/astral-sh/python-build-standalone/releases/download')
    } else {
        @('https://registry.npmmirror.com/-/binary/python-build-standalone', 'https://ghproxy.net/https://github.com/astral-sh/python-build-standalone/releases/download', '')
    }
    try {
        foreach ($Mirror in $Mirrors) {
            if ($Mirror) { $env:UV_PYTHON_INSTALL_MIRROR = $Mirror }
            else { $env:UV_PYTHON_INSTALL_MIRROR = $null }
            try {
                & uv python install 3.12
                if ($LASTEXITCODE -eq 0) { return }
                throw "uv python install failed (exit code $LASTEXITCODE)"
            } catch { Write-Msg -zh "⚠️ Python 下载源失败：$($_.Exception.Message)" -en "⚠️ Python source failed: $($_.Exception.Message)" -Color Yellow }
        }
    } catch {
        Write-Msg -zh "⚠️ Python 下载失败：$($_.Exception.Message)" -en "⚠️ Python download failed: $($_.Exception.Message)" -Color Yellow
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
    $script:PreferredPythonDir = $PythonDir
    Update-SessionPath
}

function Test-WorkingFFmpeg {
    Update-SessionPath
    foreach ($Name in @('ffmpeg', 'ffprobe')) {
        $Command = Get-Command $Name -ErrorAction SilentlyContinue
        if (-not $Command) { return $false }
        try {
            $null = & $Command.Source -version 2>$null
            if ($LASTEXITCODE -ne 0) { return $false }
        } catch { return $false }
    }
    return $true
}

function Install-FFmpeg {
    if (Test-WorkingFFmpeg) { return }
    $null = Install-DependencyPackage 'Gyan.FFmpeg' 'ffmpeg' 'ffmpeg'
    if (Test-WorkingFFmpeg) { return }
    Write-Msg -zh '⬇️ 正在补齐音视频运行环境 FFmpeg...' -en '⬇️ Installing FFmpeg for audio and video...' -Color Cyan
    $Artifacts = New-Object 'System.Collections.Generic.List[object]'
    if ($Arch -eq 'amd64') {
        $Artifacts.Add((Get-FixedArtifact 'ffmpeg' 'amd64'))
        $Artifacts.Add((Get-FixedArtifact 'ffmpeg-fallback' 'amd64'))
    } else {
        $Artifact = Get-FixedArtifact 'ffmpeg' 'arm64'
        if (-not $Artifact) { throw 'No verified Windows ARM64 FFmpeg artifact is configured.' }
        $Artifacts.Add($Artifact)
    }
    $Archive = Join-Path $WorkDir 'ffmpeg.zip'
    $Destination = Join-Path $DepsDir 'ffmpeg'
    foreach ($Artifact in $Artifacts) {
        $Urls = Get-PortableArtifactUrls $Artifact.Name $Artifact.Url
        if (-not (Receive-VerifiedArtifact $Urls $Archive $Artifact.Hash)) { continue }
        $Stage = Join-Path $WorkDir 'ffmpeg-extract'
        try {
            if (Test-Path -LiteralPath $Stage) { Remove-Item -LiteralPath $Stage -Recurse -Force }
            Expand-Archive -LiteralPath $Archive -DestinationPath $Stage -Force
            $FFmpeg = Get-ChildItem -LiteralPath $Stage -Recurse -Filter 'ffmpeg.exe' | Select-Object -First 1
            if (-not $FFmpeg) { throw 'FFmpeg archive missing executable' }
            $Root = $FFmpeg.Directory.Parent.FullName
            if (-not (Test-PortableFFmpegRuntime $FFmpeg.Directory.FullName)) { throw 'FFmpeg runtime check failed' }
            $Ready = Join-Path $WorkDir 'ffmpeg-ready'
            Move-Item -LiteralPath $Root -Destination $Ready
            $BinRelative = $FFmpeg.Directory.FullName.Substring($Root.Length).TrimStart('\', '/')
            $BinDir = Join-Path $Destination $BinRelative
            Add-UserPathFront -DirToAdd $BinDir
            Replace-DependencyDirectory $Ready $Destination { Add-UserPathFront -DirToAdd $BinDir; Update-SessionPath } { Test-WorkingFFmpeg }
            return
        } catch {
            Write-Msg -zh "⚠️ FFmpeg 安装源失败：$($_.Exception.Message)" -en "⚠️ FFmpeg source failed: $($_.Exception.Message)" -Color Yellow
        }
    }
    Remove-Item -LiteralPath $Archive -Force -ErrorAction SilentlyContinue
    Write-Msg -zh '⚠️ FFmpeg/ffprobe 未能补齐，媒体处理能力可能不可用。' -en '⚠️ FFmpeg/ffprobe could not be installed; media processing may be unavailable.' -Color Yellow
    return $false
}

# 4. Node.js 检查与固定版本便携包
if (-not (Test-WorkingNode)) {
    Write-Msg -zh "   🟢 正在安装 Node.js v24..." -en "   🟢 Installing Node.js v24..." -Color Gray
    if (-not (Get-Command 'node' -ErrorAction SilentlyContinue)) {
        $null = Install-DependencyPackage 'OpenJS.NodeJS.LTS' 'nodejs-lts' 'nodejs-lts'
    }
    if (-not (Test-WorkingNode)) { $null = Install-PortableNode }
    if (-not (Test-WorkingNode)) {
        Write-Msg -zh '⚠️ Node.js 自动安装未完成；请检查网络后重跑安装。' -en '⚠️ Node.js auto-install failed; check network and rerun.' -Color Yellow
    }
}
if ($env:POLARIS_CONFIGURE_MIRRORS -eq '1') {
    if ((Test-WorkingNode) -and -not $env:NPM_CONFIG_REGISTRY -and -not $env:NPM_CONFIG_USERCONFIG) {
        try {
            $RegistryOutput = @(& $script:WorkingNpmCmd config get registry 2>$null)
            $Registry = if ($RegistryOutput.Count) { [string]$RegistryOutput[0] } else { '' }
            if (-not $Registry -or $Registry.Trim() -match '^https://registry\.npmjs\.org/?$') {
                & $script:WorkingNpmCmd config set registry https://registry.npmmirror.com | Out-Null
                if ($LASTEXITCODE -ne 0) { throw 'npm config returned a failure exit code' }
            }
        } catch { Write-Msg -zh "⚠️ npm 用户源配置失败：$($_.Exception.Message)" -en "⚠️ Failed to configure the user npm registry: $($_.Exception.Message)" -Color Yellow }
    }
    if (-not $env:PIP_INDEX_URL -and -not $env:PIP_EXTRA_INDEX_URL -and -not $env:PIP_CONFIG_FILE -and (Test-WorkingPython)) {
        try {
            $OldErrorActionPreference = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            try { $PipOutput = @(& $script:WorkingPythonExe -m pip config get global.index-url 2>$null) }
            finally { $ErrorActionPreference = $OldErrorActionPreference }
            $PipIndex = if ($PipOutput.Count) { [string]$PipOutput[0] } else { '' }
            if (-not $PipIndex -or $PipIndex.Trim() -match '^https://pypi\.org/simple/?$') {
                & $script:WorkingPythonExe -m pip config --user set global.index-url https://mirrors.aliyun.com/pypi/simple/ | Out-Null
                if ($LASTEXITCODE -ne 0) { throw 'pip config returned a failure exit code' }
            }
        } catch { Write-Msg -zh "⚠️ pip 用户源配置失败：$($_.Exception.Message)" -en "⚠️ Failed to configure the user pip index: $($_.Exception.Message)" -Color Yellow }
    }
    $UvConfig = Join-Path $env:APPDATA 'uv/uv.toml'
    if (-not $env:UV_DEFAULT_INDEX -and -not $env:UV_INDEX_URL -and -not $env:UV_PYTHON_INSTALL_MIRROR -and
        -not $env:UV_CONFIG_FILE -and -not $env:PIP_CONFIG_FILE -and -not (Test-Path -LiteralPath $UvConfig)) {
        try {
            $UvDirectory = Split-Path $UvConfig -Parent
            New-Item -ItemType Directory -Force -Path $UvDirectory | Out-Null
            [IO.File]::WriteAllText($UvConfig, "python-install-mirror = `"https://registry.npmmirror.com/-/binary/python-build-standalone`"`n[[index]]`nurl = `"https://mirrors.aliyun.com/pypi/simple/`"`ndefault = true`n", [Text.UTF8Encoding]::new($false))
        } catch { Write-Msg -zh "⚠️ uv 用户源配置失败：$($_.Exception.Message)" -en "⚠️ Failed to configure the user uv index: $($_.Exception.Message)" -Color Yellow }
    }
}
# 5. FFmpeg / ffprobe 检查与自动安装
$null = Install-FFmpeg

foreach ($dep in @('Git', 'Bash', 'Uv', 'Python', 'Node', 'VisualC')) {
    $Ready = & "Test-Working$dep"
    if (-not $Ready) {
        $DepsReady = $false
        Write-Msg -zh "⚠️ 运行环境尚未就绪: $dep" -en "⚠️ Dependency not ready: $dep" -Color Yellow
    }
}
if (-not (Test-WorkingFFmpeg)) { Write-Msg -zh '⚠️ FFmpeg/ffprobe 未就绪，媒体处理可能不可用。' -en '⚠️ FFmpeg/ffprobe are unavailable; media processing may be unavailable.' -Color Yellow }
if (-not $DepsReady) { throw "Required dependencies remain unavailable; existing Polaris installation preserved." }
if ($DepsReady) { Write-Msg -zh "✅ 基础环境准备就绪。" -en "✅ Dependencies ready." -Color Green }


function Test-WebView2Runtime {
    $KeyName = 'SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}'
    foreach ($Hive in @([Microsoft.Win32.RegistryHive]::LocalMachine, [Microsoft.Win32.RegistryHive]::CurrentUser)) {
        foreach ($View in @([Microsoft.Win32.RegistryView]::Registry64, [Microsoft.Win32.RegistryView]::Registry32)) {
            $Base = $null
            $Key = $null
            try {
                $Base = [Microsoft.Win32.RegistryKey]::OpenBaseKey($Hive, $View)
                $Key = $Base.OpenSubKey($KeyName)
                $Version = $null
                if ($null -ne $Key -and [version]::TryParse([string]$Key.GetValue('pv', '0.0.0.0'), [ref]$Version) -and $Version -gt [version]'0.0.0.0') { return $true }
            } finally {
                if ($null -ne $Key) { $Key.Dispose() }
                if ($null -ne $Base) { $Base.Dispose() }
            }
        }
    }
    return $false
}

function Install-WebView2Runtime {
    if (Test-WebView2Runtime) { return }
    Write-Msg -zh '⬇️ 正在自动安装桌面界面所需的 WebView2...' -en '⬇️ Installing WebView2 for the desktop interface...' -Color Cyan
    Install-MicrosoftComponent -Name 'WebView2' -Uri 'https://go.microsoft.com/fwlink/p/?LinkId=2124703' -Arguments '/silent /install' -ReadyFunction 'Test-WebView2Runtime'
}

# ── 4. 下载（逐源重试）────────────────────────────────────────
Write-Msg -zh "⬇️  开始下载 Polaris（逐源重试）..." `
           -en "⬇️  Downloading Polaris (with source retries)..." -Color Cyan

$ZipPath       = "$WorkDir\polaris.zip"
$PartPath      = "$ZipPath.part"
$ShaFile       = "$ZipPath.sha256"
$SigFile       = "$ShaFile.sig"
$Downloaded    = $false
$DownloadedUrl = ""

# ── 5. 校验 & 解压 ───────────────────────────────────────────────────────────
Write-Msg -zh "📦 正在校验并解压..." -en "📦 Verifying and extracting..." -Color Cyan

# 公钥随安装器分发，不能从镜像取回；Windows 5.1 用 CNG 验证 P-256。
function Test-ReleaseSignature {
    param([string]$ChecksumPath, [string]$SignaturePath)
        $Der = [Convert]::FromBase64String([IO.File]::ReadAllText($SignaturePath).Trim())
        if ($Der.Length -lt 8 -or $Der[0] -ne 48 -or $Der[1] -ne ($Der.Length - 2)) { return $false }
        # 发布端使用 ASN.1 DER；CNG 接口要求定长 r || s。
        $Offset = 2; $Raw = New-Object byte[] 64
        for ($Part = 0; $Part -lt 2; $Part++) {
            if ($Offset + 2 -gt $Der.Length -or $Der[$Offset] -ne 2) { return $false }
            $Length = [int]$Der[$Offset + 1]; $Offset += 2
            if ($Length -lt 1 -or $Length -gt 33 -or $Offset + $Length -gt $Der.Length) { return $false }
            if ($Length -eq 33) { if ($Der[$Offset] -ne 0) { return $false }; $Offset++; $Length-- }
            [Array]::Copy($Der, $Offset, $Raw, ($Part * 32 + 32 - $Length), $Length); $Offset += $Length
        }
        if ($Offset -ne $Der.Length) { return $false }
        foreach ($SpkiText in $script:ReleasePublicKeys) {
            $Key = $null; $Verifier = $null
            try {
                $Spki = [Convert]::FromBase64String($SpkiText)
                [byte[]]$Blob = @(0x45, 0x43, 0x53, 0x31, 32, 0, 0, 0) + $Spki[($Spki.Length - 64)..($Spki.Length - 1)]
                $Key = [Security.Cryptography.CngKey]::Import($Blob, [Security.Cryptography.CngKeyBlobFormat]::EccPublicBlob)
                $Verifier = [Security.Cryptography.ECDsaCng]::new($Key)
                if ($Verifier.VerifyData([IO.File]::ReadAllBytes($ChecksumPath), $Raw, [Security.Cryptography.HashAlgorithmName]::SHA256)) { return $true }
            } catch { } finally {
                if ($null -ne $Verifier) { $Verifier.Dispose() }
                if ($null -ne $Key) { $Key.Dispose() }
            }
        }
        return $false
}
# RELEASE_PUBLIC_KEYS_BEGIN
$script:ReleasePublicKeys = @(
    'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAERq8RvYaXY9cy0aXOFTmU6sNNzIRikjdQoUZuy/iZ0HIAaZNDMmAYhxgqAdpIdUfiAZUZOCUxL/9ghaSAHZs3Bw=='
)
# RELEASE_PUBLIC_KEYS_END
foreach ($src in $CandidateURLs) {
    Write-Msg -zh "   尝试: $src" -en "   Trying: $src" -Color Gray
    try {
        Remove-Item -LiteralPath $PartPath,$ZipPath,$ShaFile,$SigFile -Force -ErrorAction SilentlyContinue
        Invoke-WebRequest -Uri $src -OutFile $PartPath -UseBasicParsing -TimeoutSec 300 -ErrorAction Stop
        Invoke-WebRequest -Uri "$src.sha256" -OutFile $ShaFile -UseBasicParsing -TimeoutSec 30 -ErrorAction Stop
        Invoke-WebRequest -Uri "$src.sha256.sig" -OutFile $SigFile -UseBasicParsing -TimeoutSec 30 -ErrorAction Stop
        if (-not (Test-ReleaseSignature $ShaFile $SigFile)) { throw 'Release signature invalid' }
        $ExpectedSha = ((Get-Content -LiteralPath $ShaFile -Raw).Trim() -split '\s+')[0].ToLowerInvariant()
        $ActualSha = (Get-FileHash -LiteralPath $PartPath -Algorithm SHA256).Hash.ToLowerInvariant()
        if ([string]::IsNullOrWhiteSpace($ExpectedSha) -or $ExpectedSha -ne $ActualSha) { throw 'Release SHA-256 mismatch' }
        Move-Item -LiteralPath $PartPath -Destination $ZipPath -Force
        $Downloaded = $true; $DownloadedUrl = $src
        break
    } catch {
        Write-Msg -zh "   发布包或校验文件失败：$($_.Exception.Message)，继续尝试下一源。" `
                   -en "   Artifact or verification failed: $($_.Exception.Message); trying next source." -Color Yellow
    }
}
if (-not $Downloaded) { throw 'No source provided a valid signed Polaris release set.' }
Remove-Item -LiteralPath $ShaFile,$SigFile -Force -ErrorAction SilentlyContinue
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

Assert-PolarisNativeRuntime -Root $SrcDir
if (Test-Path -LiteralPath (Join-Path $SrcDir 'desktop')) { Install-WebView2Runtime }
Write-Msg -zh '✅ 原生运行库已验证。' -en '✅ Native runtime verified.' -Color Green

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

# ── 6. 配置登录自启 ──────────────────────────────────────────────────────────
# 委托给 `polaris service install`，本脚本**不再自写**任务计划。
# 两份实现必然漂移其一：任务名、启动参数、权限级别各写一遍，改了一处忘了另一处
# 的结果是"装出来的任务和 polaris service status 看到的不是同一个"。
# 单一实现见 cmd/polaris/cli_service.go（ADR-0096 决策一）。
Write-Msg -zh "⚙️  配置当前用户登录自启动..." -en "⚙️  Configuring login autostart for the current user..." -Color Cyan

function Test-PolarisHTTPHealth($BaseUrl) {
    # 本机服务必须直连，避免系统代理把回环请求送到远端或返回代理错误页。
    foreach ($Route in @('/healthz', '/')) {
        # 健康接口存在不代表首页已注册，Tier 门控曾导致后台正常而控制台返回 404。
        $Request = [System.Net.WebRequest]::Create("$($BaseUrl.TrimEnd('/'))$Route")
        $Request.Proxy = $null
        $Request.Timeout = 1000
        $Request.ReadWriteTimeout = 1000
        $Request.AllowAutoRedirect = $false
        $Response = $null
        try {
            $Response = $Request.GetResponse()
            if ([int]$Response.StatusCode -ne 200) { return $false }
        } finally {
            if ($null -ne $Response) { $Response.Close() }
        }
    }
    return $true
}

function Get-PolarisRuntimeStatus {
    $StatusText = & $FinalExe service status --json
    if ($LASTEXITCODE -ne 0) { throw "Cannot query Polaris runtime status (exit code $LASTEXITCODE)" }
    $Status = $StatusText | ConvertFrom-Json -ErrorAction Stop
    # 状态不可读时不能盲目拉起第二个进程，避免与现有实例争用锁和日志文件。
    if ($null -eq $Status -or $Status.running -isnot [bool] -or $Status.problem) {
        throw 'Polaris runtime status is unavailable; inspect service status and logs.'
    }
    return $Status
}

try {
    & $FinalExe service install
    $ServiceRegistered = ($LASTEXITCODE -eq 0)
} catch {
    Write-Msg -zh "⚠️ 服务注册失败：$($_.Exception.Message)" `
               -en "⚠️ Service registration failed: $($_.Exception.Message)" -Color Yellow
}
if ($ServiceRegistered) {
    # Go CLI 可能选择普通用户 HKCU Run 后端，安装脚本不能再把它当遗留入口删除。
    $ServiceLaunchAttempted = $true
    Write-Msg -zh "✅ 服务已注册并启动，登录时自动运行。" `
               -en "✅ Service registered and started, auto-starts on login." -Color Green
} else {
    Write-Msg -zh "⚠️ 登录自启动未配置成功，将尝试以当前用户启动后台进程。" `
               -en "⚠️ Login autostart was not configured. Trying to start a background process as the current user." -Color Yellow
    Write-Msg -zh "   检查计划任务权限后，在当前账户重试：& '$FinalExe' service install" `
               -en "   Check Task Scheduler permissions, then retry as the same user: & '$FinalExe' service install" -Color Yellow
    try {
        $RuntimeStatus = Get-PolarisRuntimeStatus
        # 注册可能已成功而即时启动失败，状态查询比进程退出码更能区分这两步。
        if ($RuntimeStatus.registered -is [bool]) { $ServiceRegistered = $RuntimeStatus.registered }
        if (-not $RuntimeStatus.running) {
            Start-Process -FilePath $FinalExe -ArgumentList 'serve' -WorkingDirectory $InstallDir -WindowStyle Hidden `
                -RedirectStandardOutput (Join-Path $LogDir 'service-fallback.out.log') `
                -RedirectStandardError (Join-Path $LogDir 'service-fallback.err.log') -ErrorAction Stop
        }
        $ServiceLaunchAttempted = $true
    } catch {
        Write-Msg -zh "⚠️ 后台启动失败：$($_.Exception.Message)，请检查 $LogDir。" `
                   -en "⚠️ Background startup failed: $($_.Exception.Message). Check $LogDir." -Color Yellow
    }
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
# 注册成功和直接后台启动都必须确认实际就绪，不能把进程创建成功当作服务可用。
$ConsoleUrl = ""
if ($ServiceLaunchAttempted) {
    $ServiceReady = $false
    for ($Attempt = 0; $Attempt -lt 30; $Attempt++) {
        try {
            $rt = Get-PolarisRuntimeStatus
            if ($rt.running -and $rt.base_url -and (Test-PolarisHTTPHealth $rt.base_url)) {
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
    if ($ServiceRegistered) {
        Write-Msg -zh "🎉 安装完成，后台服务已就绪。" -en "🎉 Installation complete; service is ready." -Color Green
    } else {
        Write-Msg -zh "✅ 程序已安装，后台进程已就绪；登录自启动尚未配置成功。" `
                   -en "✅ Core installed and background process ready; login autostart is not configured." -Color Yellow
    }
} else {
    Write-Msg -zh "⚠️ 主程序已安装，但依赖或后台服务未就绪。可运行 polaris serve 查看错误，处理后重跑安装。" `
               -en "⚠️ Core installed, but dependencies or service need attention. Run polaris serve for diagnostics, then rerun." -Color Yellow
    Write-Msg -zh "   诊断状态：& '$FinalExe' service status --json（含本机访问令牌，请勿公开粘贴完整输出）" `
               -en "   Diagnose: & '$FinalExe' service status --json (contains a local access token; do not share the full output)" -Color Yellow
    Write-Msg -zh "   日志目录：$LogDir" -en "   Log directory: $LogDir" -Color Yellow
}
if ($ServiceReady) {
    Write-Msg -zh "   请访问: $ConsoleUrl" -en "   Visit: $ConsoleUrl" -Color Yellow
}
Write-Host ""
if (-not ($DepsReady -and $ServiceReady)) { throw "Polaris installation incomplete; see warnings above." }
} finally {
    Remove-Item -LiteralPath $WorkDir -Recurse -Force -ErrorAction SilentlyContinue
    $ProgressPreference = $PreviousProgressPreference
    $ErrorActionPreference = $PreviousErrorActionPreference
}
