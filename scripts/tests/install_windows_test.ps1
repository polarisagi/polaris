$ErrorActionPreference = 'Stop'
$Root = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
$Source = Join-Path $Root 'scripts/install.ps1'
# 网络响应可能保留 BOM；ParseFile 会吞掉它，不能覆盖 irm | iex 的入口行为。
$SourceBytes = [System.IO.File]::ReadAllBytes($Source)
$Text = [System.Text.Encoding]::UTF8.GetString($SourceBytes)
if ($Text[0] -eq [char]0xFEFF) { throw 'Installer must use UTF-8 without BOM for irm | iex' }
$Tokens = $null
$ParseErrors = $null
$Ast = [System.Management.Automation.Language.Parser]::ParseInput($Text, [ref]$Tokens, [ref]$ParseErrors)
if ($ParseErrors.Count) { throw ($ParseErrors | Out-String) }
if ($Ast.Find({ param($Node) $Node -is [System.Management.Automation.Language.ExitStatementAst] }, $true)) {
    throw 'Installer must throw on failure instead of exiting the caller PowerShell'
}
# 只执行首段初始化，验证字符串管道入口，不触发下载、安装和注册表写入。
& {
    $ErrorActionPreference = 'Continue'
    $Prologue = $Text.Substring(0, $Text.IndexOf('[Console]::OutputEncoding'))
    $Prologue | Invoke-Expression -ErrorAction Stop
    if ($PreviousErrorActionPreference -ne 'Continue' -or $ErrorActionPreference -ne 'Stop') {
        throw 'Installer prologue did not preserve and set ErrorActionPreference'
    }
}
$null = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $Root 'scripts/uninstall.ps1'), [ref]$Tokens, [ref]$ParseErrors)
if ($ParseErrors.Count) { throw ($ParseErrors | Out-String) }

# 只加载待测函数，不运行安装主体或写入用户注册表。
foreach ($Name in @('Update-SessionPath', 'Test-WorkingPython', 'Test-WorkingNode', 'Test-WorkingUv', 'Invoke-UvInstaller', 'Install-ManagedPython', 'Get-PolarisRuntimeStatus')) {
    $FunctionAst = $Ast.FindAll({ param($Node) $Node -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true) |
        Where-Object { $_.Name -eq $Name } | Select-Object -First 1
    . ([scriptblock]::Create($FunctionAst.Extent.Text))
}
function Assert-True($Condition, $Message) { if (-not $Condition) { throw $Message } }
$TestDir = Join-Path ([System.IO.Path]::GetTempPath()) ('polaris-win-test-' + [guid]::NewGuid().ToString('N'))
$SavedPath = $env:Path
$SavedProfile = $env:USERPROFILE
$SavedProgramFiles = $env:ProgramFiles
$SavedLocalAppData = $env:LOCALAPPDATA
$SavedArch = $env:PROCESSOR_ARCHITECTURE
$SavedNativeArch = $env:PROCESSOR_ARCHITEW6432
try {
    New-Item -ItemType Directory -Path $TestDir | Out-Null
    & {
        # 直接执行安装和收尾片段，防止“进程已创建”被误报为“服务已就绪”。
        $Start = $Text.IndexOf('try {', $Text.IndexOf('function Get-PolarisRuntimeStatus'))
        $End = $Text.IndexOf('# ── 6.5', $Start)
        $RegisterService = [scriptblock]::Create($Text.Substring($Start, $End - $Start))
        $Start = $Text.IndexOf('# ── 7.')
        $End = $Text.LastIndexOf('} finally {')
        $FinishInstall = [scriptblock]::Create($Text.Substring($Start, $End - $Start))
        $FinalExe = 'Invoke-MockPolaris'
        $InstallDir = Join-Path $TestDir 'installation [中文] with spaces'
        $LogDir = Join-Path $TestDir 'logs'
        $TaskName = 'Polaris-test'
        function Write-Host { param($Object) }
        function Write-Msg { param($zh, $en, $Color) $script:ServiceMessages.Add($en) }
        function Start-Sleep { param($Seconds) }
        function Test-PolarisHTTPHealth {
            param($BaseUrl)
            Assert-True ($BaseUrl -eq 'http://127.0.0.1:29999') 'Health probe ignored actual runtime port'
            if ($script:ServiceScenario -eq 'http-failed') { throw 'mock connection refused' }
            return $script:ServiceScenario -ne 'http-unhealthy'
        }
        function Remove-ItemProperty { param($Path, $Name, $ErrorAction) $script:LegacyRemoved = $true }
        function Invoke-MockPolaris {
            param($Command, $Action, $Format)
            $global:LASTEXITCODE = 0
            if ($Action -eq 'install') {
                if ($script:ServiceScenario -eq 'registered') { return }
                if ($script:ServiceScenario -eq 'native-throw') { throw 'mock Access is denied' }
                $global:LASTEXITCODE = 1
                return
            }
            if ($script:ServiceScenario -eq 'status-failed') { $global:LASTEXITCODE = 1; return }
            if ($script:ServiceScenario -eq 'invalid-status') { return '{"running":"unknown"}' }
            if ($script:ServiceScenario -eq 'status-problem') { return '{"running":false,"problem":"mock runtime read failure"}' }
            if ($script:ServiceScenario -eq 'initializing' -and $script:StatusCalls -eq 0) {
                $script:StatusCalls++
                return '{"running":true}'
            }
            $Running = $script:ServiceScenario -in @('registered', 'already-running', 'initializing') -or $script:BackgroundStarted
            if ($script:ServiceScenario -eq 'timeout') { $Running = $false }
            @{registered=($script:ServiceScenario -in @('registered', 'registered-launch-failed')); running=$Running; base_url=$(if ($Running) { 'http://127.0.0.1:29999' } else { '' })} | ConvertTo-Json -Compress
        }
        function Start-Process {
            [CmdletBinding()]
            param($FilePath, $ArgumentList, $WorkingDirectory, $WindowStyle, $RedirectStandardOutput, $RedirectStandardError)
            Assert-True ($FilePath -eq $FinalExe -and $ArgumentList -eq 'serve') 'Wrong fallback executable or arguments'
            Assert-True ($WorkingDirectory -eq $InstallDir -and $WindowStyle -eq 'Hidden') 'Fallback lost install path or hidden window setting'
            Assert-True ($RedirectStandardOutput -eq (Join-Path $LogDir 'service-fallback.out.log')) 'Fallback stdout is not logged'
            Assert-True ($RedirectStandardError -eq (Join-Path $LogDir 'service-fallback.err.log')) 'Fallback stderr is not logged'
            if ($script:ServiceScenario -eq 'start-failed') { throw 'mock process creation failure' }
            $script:BackgroundStarted = $true
        }
        foreach ($script:ServiceScenario in @('registered', 'registered-launch-failed', 'denied', 'native-throw', 'already-running', 'initializing', 'start-failed', 'status-failed', 'invalid-status', 'status-problem', 'timeout', 'http-failed', 'http-unhealthy', 'dependency-failed')) {
            $ServiceRegistered = $false
            $ServiceReady = $false
            $ServiceLaunchAttempted = $false
            $DepsReady = $script:ServiceScenario -ne 'dependency-failed'
            $script:BackgroundStarted = $false
            $script:LegacyRemoved = $false
            $script:StatusCalls = 0
            $script:ServiceMessages = New-Object 'System.Collections.Generic.List[string]'
            . $RegisterService
            $Failed = $false
            try { . $FinishInstall } catch { $Failed = $true }
            $ExpectedFailure = $script:ServiceScenario -in @('start-failed', 'status-failed', 'invalid-status', 'status-problem', 'timeout', 'http-failed', 'http-unhealthy', 'dependency-failed')
            Assert-True ($Failed -eq $ExpectedFailure) "Incorrect final install result: $script:ServiceScenario"
            Assert-True (-not $script:LegacyRemoved) 'Installer removed the CLI-managed per-user autostart entry'
            Assert-True ($script:ServiceMessages.Contains('   Visit: http://127.0.0.1:29999') -eq $ServiceReady) 'Installer advertised a URL without runtime readiness'
            if ($script:ServiceScenario -in @('registered', 'already-running', 'initializing', 'status-failed', 'invalid-status', 'status-problem')) {
                Assert-True (-not $script:BackgroundStarted) "Duplicate or blind launch: $script:ServiceScenario"
            }
            if ($script:ServiceScenario -in @('denied', 'native-throw')) {
                Assert-True ($ServiceReady -and -not $ServiceRegistered) 'Runtime readiness and autostart registration were conflated'
                Assert-True ($script:ServiceMessages.Contains('✅ Core installed and background process ready; login autostart is not configured.')) 'Fallback did not disclose missing autostart'
            }
            if ($script:ServiceScenario -eq 'registered-launch-failed') {
                Assert-True ($ServiceRegistered -and $ServiceReady) 'Registered autostart was lost when direct startup recovered'
            }
        }
    }
    & {
        $InstallDir = 'C:\Users\Owner\[test]\.polaris\bin'
        $script:StoppedPaths = New-Object 'System.Collections.Generic.List[string]'
        function Get-Process {
            param($Name, $ErrorAction)
            foreach ($Path in @("$InstallDir\polaris.exe", "$InstallDir\desktop\polaris-desktop-windows-amd64.exe", "$InstallDir-old\polaris.exe", 'C:\Other\polaris.exe')) {
                [pscustomobject]@{Path=$Path}
            }
        }
        function Stop-Process {
            param([Parameter(ValueFromPipeline=$true)]$InputObject, [switch]$Force)
            process { $script:StoppedPaths.Add($InputObject.Path) }
        }
        function Wait-Process { param([Parameter(ValueFromPipeline=$true)]$InputObject, $Timeout) process { } }
        $Start = $Text.IndexOf('$InstallPrefix =')
        $End = $Text.IndexOf('Get-ChildItem -LiteralPath $SrcDir', $Start)
        . ([scriptblock]::Create($Text.Substring($Start, $End - $Start)))
        Assert-True ($script:StoppedPaths.Count -eq 2) 'Update did not stop installed core and desktop exclusively'
    }
    & {
        $WorkDir = Join-Path $TestDir 'cleanup [中文]'
        New-Item -ItemType Directory -Path $WorkDir | Out-Null
        $PreviousErrorActionPreference = 'Continue'
        $PreviousProgressPreference = 'Continue'
        $ProgressPreference = 'SilentlyContinue'
        $MainTry = $Ast.EndBlock.Statements | Where-Object { $_ -is [System.Management.Automation.Language.TryStatementAst] } | Select-Object -Last 1
        $Cleanup = $MainTry.Finally.Extent.Text
        . ([scriptblock]::Create($Cleanup.Substring(1, $Cleanup.Length - 2)))
        Assert-True (-not (Test-Path -LiteralPath $WorkDir)) 'Work directory was not removed'
        Assert-True ($ErrorActionPreference -eq 'Continue' -and $ProgressPreference -eq 'Continue') 'Caller preferences were not restored'
    }
    & {
        # 使用真实 Node 验证旧版参数传递；函数模拟无法发现双引号被吞掉的问题。
        $NodePath = (Get-Command node -ErrorAction Stop).Source
        $NodeMajor = [int]((& $NodePath --version).TrimStart('v').Split('.')[0])
        $PSNativeCommandArgumentPassing = 'Legacy'
        $NodeFunction = $Ast.Find({ param($Node) $Node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $Node.Name -eq 'Test-WorkingNode' }, $true)
        $NodeCode = $NodeFunction.Find({ param($Node) $Node -is [System.Management.Automation.Language.StringConstantExpressionAst] -and $Node.Value -like 'process.exit*' }, $true).Value
        & $NodePath -e $NodeCode
        Assert-True ($LASTEXITCODE -eq $(if ($NodeMajor -ge 22) { 0 } else { 1 })) 'Node version check failed with legacy argument passing'
    }
    & {
        $OriginalPythonMirror = $env:UV_PYTHON_INSTALL_MIRROR
        try {
            $IsMainland = $true
            $ActiveProxy = 'https://mirror.invalid'
            function Write-Msg { param($zh, $en, $Color) }
            function uv {
                $script:PythonAttempts.Add([string]$env:UV_PYTHON_INSTALL_MIRROR)
                if ($script:PythonAttempts.Count -eq 1) {
                    if ($script:PythonFailure -eq 'throw') { throw 'mock native stderr failure' }
                    $global:LASTEXITCODE = 1
                } else { $global:LASTEXITCODE = 0 }
            }
            foreach ($script:PythonFailure in @('throw', 'exit')) {
                $env:UV_PYTHON_INSTALL_MIRROR = 'https://previous.invalid'
                $script:PythonAttempts = New-Object 'System.Collections.Generic.List[string]'
                Install-ManagedPython
                Assert-True ($script:PythonAttempts.Count -eq 2) 'Python failure did not retry'
                Assert-True ($script:PythonAttempts[0] -like 'https://mirror.invalid/*') 'Python mirror was not used'
                Assert-True ([string]::IsNullOrEmpty($script:PythonAttempts[1])) 'Python retry did not use official source'
                Assert-True ($env:UV_PYTHON_INSTALL_MIRROR -eq 'https://previous.invalid') 'Python mirror was not restored'
            }
        } finally { $env:UV_PYTHON_INSTALL_MIRROR = $OriginalPythonMirror }
    }
    & {
        # 用真实 ZIP 覆盖损坏包和缺文件包，验证替换前不会删除旧依赖。
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        function Write-Msg { param($zh, $en, $Color) }
        function Add-UserPathFront { param($DirToAdd) }
        function Update-SessionPath { }
        function Test-WorkingNode { $false }
        function Get-Command {
            param($Name)
            if ($Name -ne 'winget') { Microsoft.PowerShell.Core\Get-Command $Name }
        }
        function Invoke-WebRequest { param($Uri, $OutFile, [switch]$UseBasicParsing, $TimeoutSec, $ErrorAction) Copy-Item -LiteralPath $Fixture -Destination $OutFile -Force }
        function Invoke-RestMethod { param($Uri, $TimeoutSec, $ErrorAction) "$FixtureHash  node-v24.1.0-win-x64.zip`r`n" }
        $NodeStart = $Text.IndexOf('# 4. Node.js')
        $NodeEnd = $Text.IndexOf('# 5. FFmpeg', $NodeStart)
        $NodeInstall = [scriptblock]::Create($Text.Substring($NodeStart, $NodeEnd - $NodeStart))
        function Expand-PortableGit { param($Archive, $Destination) Expand-Archive -LiteralPath $Archive -DestinationPath $Destination -Force }
        $GitLoop = $Ast.Find({ param($Node) $Node -is [System.Management.Automation.Language.ForEachStatementAst] -and $Node.Variable.VariablePath.UserPath -eq 'u' -and $Node.Condition.Extent.Text -eq '$GitUrls' }, $true)
        $GitInstall = [scriptblock]::Create('$PortableGitZip = Join-Path $WorkDir "portable-git.exe"' + "`n" + $GitLoop.Extent.Text)
        foreach ($Dependency in @('Node', 'Git')) {
            foreach ($Scenario in @('corrupt', 'missing', 'valid')) {
                $WorkDir = Join-Path $TestDir "$Dependency $Scenario [中文]"
                $DepsDir = Join-Path $WorkDir 'deps'
                $Dest = Join-Path $DepsDir $Dependency.ToLowerInvariant()
                New-Item -ItemType Directory -Path $Dest -Force | Out-Null
                $Sentinel = Join-Path $Dest 'old-install'
                Set-Content -LiteralPath $Sentinel -Value 'keep'
                $Fixture = Join-Path $WorkDir 'fixture.zip'
                $Package = Join-Path $WorkDir 'package'
                New-Item -ItemType Directory -Path $Package | Out-Null
                if ($Scenario -eq 'corrupt') {
                    Set-Content -LiteralPath $Fixture -Value 'not a ZIP'
                } elseif ($Dependency -eq 'Node') {
                    foreach ($Name in @('node.exe', 'npm.cmd')) { Set-Content -LiteralPath (Join-Path $Package $Name) -Value 'fixture' }
                    if ($Scenario -eq 'valid') { Set-Content -LiteralPath (Join-Path $Package 'npx.cmd') -Value 'fixture' }
                    [System.IO.Compression.ZipFile]::CreateFromDirectory($Package, $Fixture, [System.IO.Compression.CompressionLevel]::Optimal, $true)
                } else {
                    $CmdDir = Join-Path $Package 'cmd'
                    New-Item -ItemType Directory -Path $CmdDir | Out-Null
                    $Name = if ($Scenario -eq 'valid') { 'git.exe' } else { 'missing.exe' }
                    Set-Content -LiteralPath (Join-Path $CmdDir $Name) -Value 'fixture'
                    $BinDir = Join-Path $Package 'bin'
                    New-Item -ItemType Directory -Path $BinDir | Out-Null
                    if ($Scenario -eq 'valid') { Set-Content -LiteralPath (Join-Path $BinDir 'bash.exe') -Value 'fixture' }
                    [System.IO.Compression.ZipFile]::CreateFromDirectory($Package, $Fixture, [System.IO.Compression.CompressionLevel]::Optimal, $false)
                }
                $FixtureHash = (Get-FileHash -LiteralPath $Fixture -Algorithm SHA256).Hash.ToLowerInvariant()
                $GitHash = $FixtureHash
                $GitUrls = @('https://download.invalid/git.zip')
                $IsMainland = $false
                $NodeVersion = '24'
                $Arch = 'amd64'
                if ($Dependency -eq 'Node') { . $NodeInstall } else { . $GitInstall }
                Assert-True ((Test-Path -LiteralPath $Sentinel) -eq ($Scenario -ne 'valid')) "$Dependency $Scenario did not preserve/replace old install correctly"
                if ($Scenario -eq 'valid') {
                    $Required = if ($Dependency -eq 'Node') { 'npx.cmd' } else { 'cmd/git.exe' }
                    Assert-True (Test-Path -LiteralPath (Join-Path $Dest $Required)) "$Dependency valid archive was not installed"
                }
            }
        }
    }
    # 真实子进程覆盖 exit 和异常；失败必须回到父进程且能继续重试。
    & {
        $WorkDir = Join-Path $TestDir 'uv installer with spaces'
        New-Item -ItemType Directory -Path $WorkDir | Out-Null
        function Invoke-RestMethod { $script:MockUvInstaller }
        foreach ($Scenario in @('exit 23', 'throw "mock uv download failure"', 'exit 0')) {
            $script:MockUvInstaller = $Scenario
            $Failed = $false
            try { Invoke-UvInstaller } catch { $Failed = $true }
            Assert-True ($Failed -eq ($Scenario -ne 'exit 0')) "Unexpected uv subprocess result: $Scenario"
        }
    }
    & {
        $OriginalUvDir = $env:UV_INSTALL_DIR
        $OriginalUvBase = $env:UV_INSTALLER_GITHUB_BASE_URL
        try {
            $env:UV_INSTALL_DIR = 'previous-uv-dir'
            $env:UV_INSTALLER_GITHUB_BASE_URL = 'https://previous.invalid'
            $LocalBinDir = $TestDir
            $CargoBinDir = $TestDir
            $IsMainland = $true
            $ActiveProxy = 'https://mirror.invalid'
            function Test-WorkingUv { $false }
            function Write-Msg { param($zh, $en, $Color) }
            function Add-UserPathFront { param($DirToAdd) }
            function Update-SessionPath { }
            function Invoke-UvInstaller {
                $script:UvAttempts.Add([string]$env:UV_INSTALLER_GITHUB_BASE_URL)
                Assert-True ($env:UV_INSTALL_DIR -eq $TestDir) 'uv install directory was not passed to child'
                if ($script:UvAttempts.Count -eq 1 -or $script:FailUvRetry) { throw 'mock installer failure' }
            }
            $Start = $Text.IndexOf('# 2. uv 检查与自动安装')
            $End = $Text.IndexOf('# 3. Python 3', $Start)
            $InstallUv = [scriptblock]::Create($Text.Substring($Start, $End - $Start))
            foreach ($script:FailUvRetry in @($false, $true)) {
                $script:UvAttempts = New-Object 'System.Collections.Generic.List[string]'
                . $InstallUv
                Assert-True ($script:UvAttempts.Count -eq 2) 'uv failure did not trigger retry'
                Assert-True ($script:UvAttempts[0] -eq 'https://mirror.invalid/https://github.com') 'Mirror was not used first'
                Assert-True ([string]::IsNullOrEmpty($script:UvAttempts[1])) 'Retry did not clear mirror override'
                Assert-True ($env:UV_INSTALL_DIR -eq 'previous-uv-dir') 'uv install directory was not restored'
                Assert-True ($env:UV_INSTALLER_GITHUB_BASE_URL -eq 'https://previous.invalid') 'uv mirror override was not restored'
            }
        } finally {
            $env:UV_INSTALL_DIR = $OriginalUvDir
            $env:UV_INSTALLER_GITHUB_BASE_URL = $OriginalUvBase
        }
    }
    $InstallDir = $TestDir
    $LocalBinDir = $TestDir
    $CargoBinDir = $TestDir
    $DepsDir = $TestDir
    $env:ProgramFiles = $TestDir
    $env:LOCALAPPDATA = $TestDir
    $env:USERPROFILE = $TestDir
    $SessionOnly = Join-Path $TestDir 'session-only'
    New-Item -ItemType Directory -Path $SessionOnly | Out-Null
    $env:Path = $SessionOnly
    Update-SessionPath
    Assert-True (($env:Path -split ';') -contains $SessionOnly) 'Session-only PATH was discarded'

    function Update-SessionPath { }
    $WorkingScript = Join-Path $TestDir 'working.ps1'
    Set-Content -Path $WorkingScript -Value '$global:LASTEXITCODE = 0'
    $StoreDir = Join-Path $TestDir 'Microsoft/WindowsApps'
    New-Item -ItemType Directory -Path $StoreDir -Force | Out-Null
    $StoreScript = Join-Path $StoreDir 'python3.ps1'
    Set-Content -Path $StoreScript -Value 'throw "Store alias must never be executed"'
    $script:Commands = @{}
    function Get-Command {
        param($Name, [switch]$All, $ErrorAction)
        $script:Commands[$Name]
    }
    $script:Commands['python3'] = @([pscustomobject]@{Source=$StoreScript}, [pscustomobject]@{Source=$WorkingScript})
    Assert-True (Test-WorkingPython) 'Real Python behind WindowsApps was not found'
    Assert-True ($script:WorkingPythonExe -eq $WorkingScript) 'Wrong Python path was recorded'
    $script:Commands['python3'] = @([pscustomobject]@{Source=$StoreScript})
    Assert-True (-not (Test-WorkingPython)) 'Store-only Python was accepted'

    foreach ($Name in @('node', 'npx.cmd', 'npm.cmd', 'uv')) {
        $script:Commands[$Name] = [pscustomobject]@{Source=$WorkingScript}
    }
    function npm.cmd { $global:LASTEXITCODE = 0 }
    Assert-True (Test-WorkingNode) 'Node must use npx.cmd and npm.cmd without PowerShell script execution policy'
    Assert-True (-not (Test-WorkingUv)) 'Missing uvx was accepted'
    $script:Commands['uvx'] = [pscustomobject]@{Source=$WorkingScript}
    Assert-True (Test-WorkingUv) 'Working uv/uvx were rejected'

    # x86 PowerShell 在 ARM64 Windows 上必须选择本机 arm64 发行包。
    $Start = $Text.IndexOf('$NativeArch =')
    $End = $Text.IndexOf('$ArchiveName =', $Start)
    $DetectArch = [scriptblock]::Create($Text.Substring($Start, $End - $Start))
    $env:PROCESSOR_ARCHITECTURE = 'x86'
    $env:PROCESSOR_ARCHITEW6432 = 'ARM64'
    . $DetectArch
    Assert-True ($Arch -eq 'arm64') 'WOW64 native architecture was not respected'
    $env:PROCESSOR_ARCHITEW6432 = ''
    $Rejected = $false
    try { . $DetectArch } catch { $Rejected = $true }
    Assert-True $Rejected 'Unsupported x86 OS was accepted'
    Write-Host 'PASS: service registration/fallback/readiness (12 scenarios), UTF-8 entry, scoped process stop, cleanup/preferences, legacy Node arguments, Python retry, Git/Node staging, uv subprocess/retry, parser, session PATH, Python aliases, Node shims, uvx, WOW64'
} finally {
    $env:Path = $SavedPath
    $env:USERPROFILE = $SavedProfile
    $env:ProgramFiles = $SavedProgramFiles
    $env:LOCALAPPDATA = $SavedLocalAppData
    $env:PROCESSOR_ARCHITECTURE = $SavedArch
    $env:PROCESSOR_ARCHITEW6432 = $SavedNativeArch
    Remove-Item $TestDir -Recurse -Force -ErrorAction SilentlyContinue
}
