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
foreach ($Name in @('Update-SessionPath', 'Test-WorkingPython', 'Test-WorkingNode', 'Test-WorkingUv', 'Install-ManagedPython', 'Get-PolarisRuntimeStatus', 'Test-ReleaseSignature', 'Get-PortableArtifactUrls', 'Get-FixedArtifact', 'Receive-VerifiedArtifact', 'Replace-DependencyDirectory', 'Expand-PortableGit', 'Install-PortableGit', 'Install-PortableUv', 'Install-PortableNode', 'Install-FFmpeg', 'Test-PortableGitRuntime', 'Test-PortableUvRuntime', 'Test-PortableNodeRuntime', 'Test-PortableFFmpegRuntime', 'Install-DependencyPackage')) {
    $FunctionAst = $Ast.FindAll({ param($Node) $Node -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true) |
        Where-Object { $_.Name -eq $Name } | Select-Object -First 1
    . ([scriptblock]::Create($FunctionAst.Extent.Text))
}
$PackageFunction = $Ast.FindAll({ param($Node) $Node -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true) |
    Where-Object { $_.Name -eq 'Install-DependencyPackage' } | Select-Object -First 1
. ([scriptblock]::Create(($PackageFunction.Extent.Text -replace '^function Install-DependencyPackage', 'function Install-DependencyPackageActual')))
$KeyBlock = [regex]::Match($Text, '(?ms)# RELEASE_PUBLIC_KEYS_BEGIN\n(.*?)\n# RELEASE_PUBLIC_KEYS_END').Groups[1].Value
$script:ReleasePublicKeys = @([regex]::Matches($KeyBlock, "'([A-Za-z0-9+/=]+)'") | ForEach-Object { $_.Groups[1].Value })
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
        # 显式指定的 npmrc 属于用户，即使它仍使用 npm 默认源也不能改写。
        $SavedMirrorEnv = @{}
        foreach ($Name in @('POLARIS_CONFIGURE_MIRRORS','NPM_CONFIG_REGISTRY','NPM_CONFIG_USERCONFIG','PIP_CONFIG_FILE','UV_CONFIG_FILE','APPDATA')) {
            $SavedMirrorEnv[$Name] = [Environment]::GetEnvironmentVariable($Name, 'Process')
        }
        try {
            $MirrorBlockStart = $Text.IndexOf("if (`$env:POLARIS_CONFIGURE_MIRRORS -eq '1') {")
            $MirrorBlockEnd = $Text.IndexOf('# 5. FFmpeg', $MirrorBlockStart)
            Assert-True ($MirrorBlockStart -ge 0 -and $MirrorBlockEnd -gt $MirrorBlockStart) 'Mirror configuration block was not found'
            $Npmrc = Join-Path $TestDir 'custom npmrc'
            $AppDataFixture = Join-Path $TestDir 'appdata'
            New-Item -ItemType Directory -Path $AppDataFixture -Force | Out-Null
            Set-Content -LiteralPath $Npmrc -Value 'registry=https://registry.npmjs.org/'
            $env:POLARIS_CONFIGURE_MIRRORS = '1'
            $env:NPM_CONFIG_REGISTRY = $null
            $env:NPM_CONFIG_USERCONFIG = $Npmrc
            $env:PIP_CONFIG_FILE = Join-Path $TestDir 'pip.ini'
            $env:UV_CONFIG_FILE = Join-Path $TestDir 'uv.toml'
            $env:APPDATA = $AppDataFixture
            $script:WorkingNpmCmd = 'MockNpmForMirrorTest'
            $script:MirrorNpmCalls = 0
            function Test-WorkingNode { return $true }
            function Test-WorkingPython { return $false }
            function Write-Msg { param($zh, $en, $Color) }
            function MockNpmForMirrorTest { $script:MirrorNpmCalls++ }
            . ([scriptblock]::Create($Text.Substring($MirrorBlockStart, $MirrorBlockEnd - $MirrorBlockStart)))
            Assert-True ($script:MirrorNpmCalls -eq 0) 'Explicit custom npmrc was modified despite its default registry'
            Assert-True ((Get-Content -LiteralPath $Npmrc -Raw).Trim() -eq 'registry=https://registry.npmjs.org/') 'Explicit npmrc bytes changed'
        } finally {
            foreach ($Name in $SavedMirrorEnv.Keys) { [Environment]::SetEnvironmentVariable($Name, $SavedMirrorEnv[$Name], 'Process') }
        }
    }
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
            $Region = 'cn'
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
                Assert-True ($script:PythonAttempts.Count -eq 1) 'Python install did not use a single selected mirror'
                Assert-True ($script:PythonAttempts[0] -eq 'https://previous.invalid') 'Explicit Python mirror was not used first'
                Assert-True ($env:UV_PYTHON_INSTALL_MIRROR -eq 'https://previous.invalid') 'Python mirror was not restored'
            }
            $env:UV_PYTHON_INSTALL_MIRROR = ''
            $script:PythonAttempts = New-Object 'System.Collections.Generic.List[string]'
            Install-ManagedPython
            Assert-True ($script:PythonAttempts.Count -eq 2) 'Default Python install did not try a fallback after mirror failure'
            Assert-True ($script:PythonAttempts[0] -eq 'https://registry.npmmirror.com/-/binary/python-build-standalone') 'Mainland Python install did not prefer npmmirror'
            Assert-True ($script:PythonAttempts[1] -match '^https://ghproxy\.net/') 'Python fallback did not use the fixed proxy mirror'
        } finally { $env:UV_PYTHON_INSTALL_MIRROR = $OriginalPythonMirror }
    }
    $PinnedNode = Get-FixedArtifact 'node' 'amd64'
    $PinnedGyan = Get-FixedArtifact 'ffmpeg' 'amd64'
    $PinnedArmFfmpeg = Get-FixedArtifact 'ffmpeg' 'arm64'
    $PinnedUvWheel = Get-FixedArtifact 'uv-wheel' 'amd64'
    Assert-True ($PinnedNode.Name -eq 'node-v24.22.3-win-x64.zip' -and $PinnedNode.Hash -eq 'e612af99f626ab415ea225af6bce5c971feb448cb5cae250bb1c3fa50c5f7d9f') 'Node fixed artifact pin drifted'
    Assert-True ($PinnedGyan.Url -match 'gyan\.dev' -and $PinnedGyan.Hash -eq '60f467265b1e312373dbcd92200c2618a74850f98d3d078e94296bb3fa2047ba') 'Gyan fixed artifact pin drifted'
    Assert-True ($PinnedArmFfmpeg.Name -match 'winarm64') 'ARM64 FFmpeg pin is not architecture-specific'
    Assert-True ($PinnedUvWheel.Name -eq 'uv-0.13.7-py3-none-win_amd64.whl' -and $PinnedUvWheel.Hash -eq '2835e2ab397d0db86323b9975dce75ecd76aa9cfa0d6a2aea84734aae7f8c9b3') 'Aliyun uv wheel pin drifted'
    & {
        # 临时归档模拟断网和镜像下载；不调用宿主包管理器或真实安装器。
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        function Write-Msg { param($zh, $en, $Color) }
        function Add-UserPathFront { param($DirToAdd) $script:AddedPaths.Add($DirToAdd) }
        function Update-SessionPath { }
        function Test-WorkingGit { $true }
        function Test-WorkingBash { $true }
        function Test-WorkingUv { $true }
        function Test-WorkingNode { $true }
        function Test-WorkingFFmpeg { Test-Path (Join-Path $DepsDir 'ffmpeg/bin/ffmpeg.exe') }
        function Test-PortableGitRuntime { param($Root) $true }
        function Test-PortableUvRuntime { param($Root) $true }
        function Test-PortableNodeRuntime { param($Root) $true }
        function Test-PortableFFmpegRuntime { param($BinDirectory) (Test-Path (Join-Path $BinDirectory 'ffmpeg.exe')) -and (Test-Path (Join-Path $BinDirectory 'ffprobe.exe')) }
        function Test-WorkingPython { $false }
        function Install-DependencyPackage { param($WingetId, $ScoopId, $ChocoId) $false }
        function Get-Command { param($Name, [switch]$All, $ErrorAction) if ($Name -eq 'winget') { return [pscustomobject]@{Name='winget'} }; return $null }
        function Expand-PortableGit { param($Archive, $Destination) Expand-Archive -LiteralPath $Archive -DestinationPath $Destination -Force }
        function Get-FixedArtifact {
            param($Kind, $NativeArch)
            if ($Kind -eq 'ffmpeg' -and $NativeArch -eq 'amd64') { return [pscustomobject]@{Name=$PinnedGyan.Name; Url=$PinnedGyan.Url; Hash=$script:FixtureArtifact.Hash} }
            if ($Kind -eq 'ffmpeg-fallback' -and $NativeArch -eq 'amd64') { return [pscustomobject]@{Name='ffmpeg-N-128227-g4811625b5f-win64-lgpl.zip'; Url='https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/autobuild-2026-10-31-12-59/ffmpeg-N-128227-g4811625b5f-win64-lgpl.zip'; Hash=$script:FixtureArtifact.Hash} }
            if ($Kind -eq 'ffmpeg' -and $NativeArch -eq 'arm64') { return [pscustomobject]@{Name=$PinnedArmFfmpeg.Name; Url=$PinnedArmFfmpeg.Url; Hash=$script:FixtureArtifact.Hash} }
            if ($Kind -eq 'uv-wheel') { return [pscustomobject]@{Name='uv-fixture.whl'; Url='https://mirrors.aliyun.com/pypi/uv-fixture.whl'; Hash=$script:FixtureArtifact.Hash} }
            if ($Kind -eq 'uv') { return [pscustomobject]@{Name='uv-fixture.zip'; Url='https://github.com/example/release/uv-fixture.zip'; Hash=$script:FixtureArtifact.Hash} }
            return $script:FixtureArtifact
        }
        function Invoke-RestMethod { param($Uri, $TimeoutSec, $ErrorAction) $script:MetadataCalls++; throw 'metadata endpoint is unreachable' }
        function Invoke-WebRequest {
            param($Uri, $OutFile, [switch]$UseBasicParsing, $TimeoutSec, $ErrorAction, $Method)
            $script:DownloadCalls.Add($Uri)
            if ($script:FailUris -contains $Uri) { throw 'mock network failure' }
            Copy-Item -LiteralPath $script:Fixture -Destination $OutFile -Force
        }
        $DownloadBase = 'https://cdn.invalid/releases'
        $IsMainland = $true
        $Region = 'cn'
        $ActiveProxy = 'https://proxy-one.invalid'
        $ProxyHosts = @('https://proxy-one.invalid', 'https://proxy-two.invalid')
        $Arch = 'amd64'
        $script:AddedPaths = New-Object 'System.Collections.Generic.List[string]'
        $WorkDir = Join-Path $TestDir 'portable downloads'
        $DepsDir = Join-Path $WorkDir 'deps'
        $LocalBinDir = Join-Path $WorkDir 'local-bin'
        New-Item -ItemType Directory -Path $DepsDir,$LocalBinDir -Force | Out-Null
        $Package = Join-Path $WorkDir 'package'
        $Fixture = Join-Path $WorkDir 'fixture.zip'
        $script:Fixture = $Fixture

        foreach ($Dependency in @('git', 'uv', 'node')) {
            Remove-Item -LiteralPath $Package,$Fixture -Recurse -Force -ErrorAction SilentlyContinue
            New-Item -ItemType Directory -Path $Package -Force | Out-Null
            if ($Dependency -eq 'git') {
                New-Item -ItemType Directory -Path (Join-Path $Package 'cmd'),(Join-Path $Package 'bin') -Force | Out-Null
                Set-Content -LiteralPath (Join-Path $Package 'cmd/git.exe') -Value 'fixture'
                Set-Content -LiteralPath (Join-Path $Package 'bin/bash.exe') -Value 'fixture'
            } elseif ($Dependency -eq 'uv') {
                $UvScripts = Join-Path $Package 'uv-0.13.7.data/scripts'
                $UvLicense = Join-Path $Package 'uv-0.13.7.dist-info/licenses'
                New-Item -ItemType Directory -Path $UvScripts,$UvLicense -Force | Out-Null
                foreach ($Name in @('uv.exe','uvw.exe','uvx.exe')) { Set-Content -LiteralPath (Join-Path $UvScripts $Name) -Value 'fixture' }
                Set-Content -LiteralPath (Join-Path $UvLicense 'LICENSE-MIT') -Value 'fixture license'
            } else {
                $NodeRoot = Join-Path $Package 'node-v24.22.3-win-x64'
                New-Item -ItemType Directory -Path $NodeRoot | Out-Null
                foreach ($Name in @('node.exe','npm.cmd','npx.cmd')) { Set-Content -LiteralPath (Join-Path $NodeRoot $Name) -Value 'fixture' }
            }
            [System.IO.Compression.ZipFile]::CreateFromDirectory($Package, $Fixture, [System.IO.Compression.CompressionLevel]::Optimal, $false)
            $FixtureHash = (Get-FileHash -LiteralPath $Fixture -Algorithm SHA256).Hash.ToLowerInvariant()
            $script:FixtureArtifact = [pscustomobject]@{Name='portable-fixture.zip'; Url='https://github.com/example/release/portable-fixture.zip'; Mainland='https://mirror.invalid/portable-fixture.zip'; Hash=$FixtureHash}
            $Dest = Join-Path $DepsDir $Dependency
            New-Item -ItemType Directory -Path $Dest -Force | Out-Null
            $Sentinel = Join-Path $Dest 'old-install'
            Set-Content -LiteralPath $Sentinel -Value 'keep'
            $script:DownloadCalls = New-Object 'System.Collections.Generic.List[string]'
            $script:FailUris = if ($Dependency -eq 'uv') { @('https://cdn.invalid/releases/uv-fixture.whl') } else { @('https://cdn.invalid/releases/portable-fixture.zip','https://mirror.invalid/portable-fixture.zip','https://proxy-one.invalid/https://github.com/example/release/portable-fixture.zip') }
            $Install = switch ($Dependency) { 'git' { 'Install-PortableGit' } 'uv' { 'Install-PortableUv' } 'node' { 'Install-PortableNode' } }
            $Result = & $Install
            Assert-True $Result "$Dependency fixed fixture did not install after mirror failures"
            Assert-True ($script:DownloadCalls.Count -ge $(if ($Dependency -eq 'uv') {2} else {3})) "$Dependency did not continue through candidate sources: $($script:DownloadCalls -join ', ')"
            Assert-True (-not (Test-Path -LiteralPath $Sentinel)) "$Dependency valid archive did not replace the old fixture"
            Assert-True ($script:AddedPaths.Count -gt 0) "$Dependency did not add the installed location to session/user PATH"
            if ($Dependency -eq 'uv') { Assert-True (Test-Path (Join-Path $DepsDir 'uv/uv-0.13.7.dist-info/licenses/LICENSE-MIT')) 'Aliyun uv wheel license metadata was discarded' }
        }

        # 摘要错时不得解压或覆盖旧依赖；元数据端点全程保持不可用且未被请求。
        $script:FixtureArtifact = [pscustomobject]@{Name='portable-fixture.zip'; Url='https://github.com/example/release/portable-fixture.zip'; Mainland=''; Hash=('0' * 64)}
        $Dest = Join-Path $DepsDir 'node'
        New-Item -ItemType Directory -Path $Dest -Force | Out-Null
        $Sentinel = Join-Path $Dest 'old-install'
        Set-Content -LiteralPath $Sentinel -Value 'keep'
        $script:FailUris = @()
        $script:DownloadCalls = New-Object 'System.Collections.Generic.List[string]'
        $script:MetadataCalls = 0
        Assert-True (-not (Install-PortableNode)) 'Bad SHA-256 was accepted'
        Assert-True (Test-Path -LiteralPath $Sentinel) 'Bad SHA-256 removed the prior Node installation'
        Assert-True ($script:MetadataCalls -eq 0) 'Portable fallback queried GitHub API, astral installer, or Node SHASUMS metadata'

        # 区域源排序和架构 pin 固定，Gyan 主站不经 GitHub 代理，ARM64 不取 Gyan x64。
        $script:FixtureArtifact = $null
        $NodeUrls = @(Get-PortableArtifactUrls $PinnedNode.Name $PinnedNode.Url $PinnedNode.Mainland)
        Assert-True ($NodeUrls[0] -eq "$DownloadBase/$($PinnedNode.Name)" -and $NodeUrls[1] -eq $PinnedNode.Mainland) 'Node mirror ordering lost self-hosted/npmmirror priority'
        $GyanUrls = @(Get-PortableArtifactUrls $PinnedGyan.Name $PinnedGyan.Url)
        Assert-True ($GyanUrls[0] -eq "$DownloadBase/$($PinnedGyan.Name)" -and $GyanUrls[1] -eq $PinnedGyan.Url) 'Gyan main site was sent through a GitHub proxy'
        Assert-True ($PinnedArmFfmpeg.Url -notmatch 'gyan\.dev') 'ARM64 selected the Gyan x64 archive'

        # FFmpeg x64 使用 Gyan 主站；ARM64 使用固定原生归档代理候选。
        $FFmpegRoot = Join-Path $Package 'ffmpeg-fixture'
        $Bin = Join-Path $FFmpegRoot 'bin'
        New-Item -ItemType Directory -Path $Bin -Force | Out-Null
        Set-Content -LiteralPath (Join-Path $Bin 'ffmpeg.exe') -Value 'fixture'
        Set-Content -LiteralPath (Join-Path $Bin 'ffprobe.exe') -Value 'fixture'
        Set-Content -LiteralPath (Join-Path $FFmpegRoot 'LICENSE.txt') -Value 'fixture license'
        Remove-Item -LiteralPath $Fixture -Force -ErrorAction SilentlyContinue
        [System.IO.Compression.ZipFile]::CreateFromDirectory($FFmpegRoot, $Fixture)
        $script:FailUris = @()
        $script:DownloadCalls = New-Object 'System.Collections.Generic.List[string]'
        function Receive-VerifiedArtifact {
            param([string[]]$Urls, [string]$Destination, [string]$Sha256)
            $script:FFmpegUrls.Add(($Urls -join '|'))
            if ($Arch -eq 'amd64' -and $script:FFmpegUrls.Count -eq 1) { return $false }
            Copy-Item -LiteralPath $script:Fixture -Destination $Destination -Force
            return $true
        }
        $Arch = 'amd64'
        $script:FFmpegUrls = New-Object 'System.Collections.Generic.List[string]'
        $null = Install-FFmpeg
        Assert-True ($script:FFmpegUrls.Count -eq 2 -and $script:FFmpegUrls[0] -match 'gyan\.dev' -and $script:FFmpegUrls[1] -match 'FFmpeg-Builds') 'FFmpeg x64 did not fall back from Gyan to its pinned native archive'
        Assert-True (Test-Path (Join-Path $DepsDir 'ffmpeg/LICENSE.txt')) 'FFmpeg install discarded archive license files'
        Remove-Item -LiteralPath (Join-Path $DepsDir 'ffmpeg') -Recurse -Force
        $Arch = 'arm64'
        $script:FFmpegUrls = New-Object 'System.Collections.Generic.List[string]'
        $null = Install-FFmpeg
        Assert-True ($script:FFmpegUrls.Count -eq 1 -and $script:FFmpegUrls[0] -match 'winarm64' -and $script:FFmpegUrls[0] -match 'github\.com') "FFmpeg ARM64 did not use its architecture-specific candidate: $($script:FFmpegUrls[0])"

        # 包管理器进度输出必须留在 host stream，调用方只接收 bool。
        function winget { param([string]$Operation,[Alias('id')][string]$PackageId,[Alias('e')][switch]$Exact,[Alias('source')][string]$SourceName,[Alias('accept-package-agreements')][switch]$Agree,[Alias('accept-source-agreements')][switch]$SourceAgree,[switch]$silent,[Alias('disable-interactivity')][switch]$Disable) Write-Output 'mock package progress'; $global:LASTEXITCODE = 0 }
        $PackageResult = Install-DependencyPackageActual 'Test.Package' 'test' 'test'
        Assert-True ($PackageResult -is [bool] -and $PackageResult) 'Package manager output polluted the boolean result'
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

    # 私有目录中的旧 Node 不可用时，应复用 PATH 后方的可用版本，并确保刷新 PATH 后它优先。
    $NodeOldDir = Join-Path $TestDir 'node-old'
    $NodePreferredDir = Join-Path $TestDir 'node-valid'
    New-Item -ItemType Directory -Path $NodeOldDir,$NodePreferredDir -Force | Out-Null
    Set-Content -LiteralPath (Join-Path $NodeOldDir 'node.ps1') -Value '$global:LASTEXITCODE = 1'
    Set-Content -LiteralPath (Join-Path $NodePreferredDir 'node.ps1') -Value '$global:LASTEXITCODE = 0'
    foreach ($Name in @('npm','npx')) { Set-Content -LiteralPath (Join-Path $NodePreferredDir "$Name.ps1") -Value '$global:LASTEXITCODE = 0' }
    $script:PreferredNodeDir = ''
    $InstallDir = Join-Path $TestDir 'unrelated-install'
    $LocalBinDir = Join-Path $TestDir 'unrelated-local-bin'
    $CargoBinDir = Join-Path $TestDir 'unrelated-cargo'
    $DepsDir = Join-Path $TestDir 'unrelated-deps'
    $env:Path = "$NodeOldDir;$NodePreferredDir"
    $script:NodeFixtureDirs = @($NodeOldDir,$NodePreferredDir)
    function Get-Command {
        param($Name, [switch]$All, $ErrorAction)
        $FileName = switch -Regex ($Name) { '^node$' { 'node.ps1'; break } '^npm\.cmd$' { 'npm.ps1'; break } '^npx\.cmd$' { 'npx.ps1'; break } default { return $null } }
        foreach ($Directory in ($env:Path -split ';')) {
            $Candidate = Join-Path $Directory $FileName
            if (Test-Path -LiteralPath $Candidate) { [pscustomobject]@{Source=$Candidate} }
        }
    }
    Assert-True (Test-WorkingNode) 'A valid Node candidate behind an incompatible private Node was not reused'
    Assert-True ($script:PreferredNodeDir -eq $NodePreferredDir) 'The valid later Node directory was not preferred'
    $SelectedNode = Get-Command 'node' -All | Select-Object -First 1
    Assert-True ($SelectedNode.Source -eq (Join-Path $NodePreferredDir 'node.ps1')) 'Session PATH still selects the incompatible Node candidate'

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

    if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        function ConvertTo-DerSignature([byte[]]$Raw) {
            $Body = New-Object 'System.Collections.Generic.List[byte]'
            foreach ($Start in @(0, 32)) {
                $Integer = New-Object 'System.Collections.Generic.List[byte]'
                for ($i = $Start; $i -lt $Start + 32; $i++) { $Integer.Add($Raw[$i]) }
                while ($Integer.Count -gt 1 -and $Integer[0] -eq 0) { $Integer.RemoveAt(0) }
                if (($Integer[0] -band 0x80) -ne 0) { $Integer.Insert(0, [byte]0) }
                $Body.Add([byte]2); $Body.Add([byte]$Integer.Count)
                foreach ($Value in $Integer) { $Body.Add($Value) }
            }
            $Der = New-Object 'System.Collections.Generic.List[byte]'
            $Der.Add([byte]48); $Der.Add([byte]$Body.Count)
            foreach ($Value in $Body) { $Der.Add($Value) }
            return ,$Der.ToArray()
        }
        $SignatureKey = [Security.Cryptography.ECDsaCng]::new(256)
        try {
            $KeyBlob = $SignatureKey.Key.Export([Security.Cryptography.CngKeyBlobFormat]::EccPublicBlob)
            [byte[]]$SpkiPrefix = @(0x30,0x59,0x30,0x13,0x06,0x07,0x2A,0x86,0x48,0xCE,0x3D,0x02,0x01,0x06,0x08,0x2A,0x86,0x48,0xCE,0x3D,0x03,0x01,0x07,0x03,0x42,0x00,0x04)
            $TestSpki = [Convert]::ToBase64String([byte[]]($SpkiPrefix + $KeyBlob[8..71]))
            $ChecksumPath = Join-Path $TestDir 'signature-checksum'
            $SignaturePath = Join-Path $TestDir 'signature-checksum.sig'
            [byte[]]$Payload = [Text.Encoding]::UTF8.GetBytes('release checksum fixture')
            [IO.File]::WriteAllBytes($ChecksumPath, $Payload)
            [IO.File]::WriteAllText($SignaturePath, [Convert]::ToBase64String((ConvertTo-DerSignature ($SignatureKey.SignData($Payload, [Security.Cryptography.HashAlgorithmName]::SHA256)))))
            $script:ReleasePublicKeys = @('not-a-key', $TestSpki)
            Assert-True (Test-ReleaseSignature $ChecksumPath $SignaturePath) 'CNG release signature failed with second rotation key'
            [IO.File]::AppendAllText($ChecksumPath, 'tampered')
            Assert-True (-not (Test-ReleaseSignature $ChecksumPath $SignaturePath)) 'CNG accepted a tampered checksum'
            [IO.File]::WriteAllText($SignaturePath, 'not-base64')
            $Rejected = $false
            try { Test-ReleaseSignature $ChecksumPath $SignaturePath | Out-Null } catch { $Rejected = $true }
            Assert-True $Rejected 'CNG accepted a malformed or stripped signature'
        } finally { $SignatureKey.Dispose() }
    }

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
    Write-Host 'PASS: service readiness, fixed portable dependency fallbacks, mirror/hash rejection, PATH, Python retry, Node shims, uvx, CNG signature, WOW64'
} finally {
    $env:Path = $SavedPath
    $env:USERPROFILE = $SavedProfile
    $env:ProgramFiles = $SavedProgramFiles
    $env:LOCALAPPDATA = $SavedLocalAppData
    $env:PROCESSOR_ARCHITECTURE = $SavedArch
    $env:PROCESSOR_ARCHITEW6432 = $SavedNativeArch
    Remove-Item $TestDir -Recurse -Force -ErrorAction SilentlyContinue
}
