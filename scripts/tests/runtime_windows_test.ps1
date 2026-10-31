$ErrorActionPreference = 'Stop'
$Root = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
$Text = [IO.File]::ReadAllText((Join-Path $Root 'scripts/install.ps1'))
$Errors = $null
$Ast = [Management.Automation.Language.Parser]::ParseInput($Text, [ref]$null, [ref]$Errors)
if ($Errors.Count) { throw ($Errors | Out-String) }
foreach ($Name in @('Get-NativePowerShell', 'Invoke-PolarisLibraryProbe', 'Test-WorkingVisualC', 'Install-MicrosoftComponent', 'Install-VisualCRuntime', 'Assert-PolarisNativeRuntime', 'Test-WebView2Runtime', 'Install-WebView2Runtime', 'Test-WorkingBash', 'Test-WorkingFFmpeg', 'Install-FFmpeg', 'Test-PolarisHTTPHealth')) {
    $Function = $Ast.FindAll({param($n) $n -is [Management.Automation.Language.FunctionDefinitionAst]}, $true) | Where-Object { $_.Name -eq $Name } | Select-Object -First 1
    if (-not $Function) { throw "Missing installer function: $Name" }
    . ([scriptblock]::Create($Function.Extent.Text))
}
function Assert($Value, $Message) { if (-not $Value) { throw $Message } }
function Write-Msg { param($zh, $en, $Color) }
$Temp = Join-Path ([IO.Path]::GetTempPath()) ('polaris-runtime-test-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $Temp | Out-Null
try {
    & {
        $SavedArch = $env:PROCESSOR_ARCHITECTURE
        $SavedNative = $env:PROCESSOR_ARCHITEW6432
        function Test-WorkingVisualC { return $script:RuntimeReady }
        function Install-MicrosoftComponent { param($Name, $Uri, $Arguments, $ReadyFunction) $script:Installs.Add($Uri); $script:RuntimeReady = $true }
        try {
            foreach ($Arch in @('AMD64', 'ARM64')) {
                $env:PROCESSOR_ARCHITECTURE = 'x86'
                $env:PROCESSOR_ARCHITEW6432 = $Arch
                $script:RuntimeReady = $true
                $script:Installs = New-Object 'Collections.Generic.List[string]'
                Install-VisualCRuntime
                Assert ($script:Installs.Count -eq 0) 'Usable VC runtime was downloaded again'
                $script:RuntimeReady = $false
                Install-VisualCRuntime
                $Suffix = if ($Arch -eq 'AMD64') { 'x64' } else { 'arm64' }
                Assert ($script:Installs.Count -eq 1 -and $script:Installs[0] -eq "https://aka.ms/vc14/vc_redist.$Suffix.exe") 'Wrong runtime architecture or source'
                Install-VisualCRuntime
                Assert ($script:Installs.Count -eq 1) 'Reinstalled working runtime'
            }
        } finally { $env:PROCESSOR_ARCHITECTURE = $SavedArch; $env:PROCESSOR_ARCHITEW6432 = $SavedNative }
    }
    & {
        $WorkDir = $Temp
        function Invoke-WebRequest { param($Uri, $OutFile, [switch]$UseBasicParsing, $TimeoutSec, $ErrorAction) $script:Downloaded = $Uri }
        function Get-AuthenticodeSignature { param($LiteralPath) [pscustomobject]@{Status=$script:SignatureStatus; SignerCertificate=[pscustomobject]@{Subject=$script:Signer}} }
        function Test-ComponentReady { return $script:PostReady }
        function Start-Process {
            param($FilePath, $ArgumentList, [switch]$PassThru, $ErrorAction)
            $script:Launched = $true
            $Process = [pscustomobject]@{ExitCode=$script:ExitCode; Handle=1}
            $Process | Add-Member ScriptMethod WaitForExit { param($Timeout) return $script:WaitResult }
            $Process | Add-Member ScriptMethod Kill { $script:Killed = $true }
            return $Process
        }
        foreach ($Case in @('success', 'untrusted', 'wrong-signer', 'exit-failed', 'not-ready', 'timeout', 'reboot-ready', 'reboot-not-ready', 'newer-ready')) {
            $script:SignatureStatus = if ($Case -eq 'untrusted') { 'NotSigned' } else { 'Valid' }
            $script:Signer = if ($Case -eq 'wrong-signer') { 'O=Other Publisher, C=US' } else { 'CN=Microsoft Corporation, O=Microsoft Corporation, C=US' }
            $script:ExitCode = switch ($Case) { 'exit-failed' { 5 } 'reboot-ready' { 3010 } 'reboot-not-ready' { 3010 } 'newer-ready' { 1638 } default { 0 } }
            $script:PostReady = $Case -notin @('not-ready', 'reboot-not-ready')
            $script:WaitResult = $Case -ne 'timeout'
            $script:Launched = $false
            $script:Killed = $false
            $Failed = $false
            try { Install-MicrosoftComponent -Name test -Uri 'https://download.invalid' -Arguments '/test' -ReadyFunction 'Test-ComponentReady' } catch { $Failed = $true }
            Assert ($Failed -eq ($Case -notin @('success', 'reboot-ready', 'newer-ready'))) "Incorrect runtime install result: $Case"
            if ($Case -in @('untrusted', 'wrong-signer')) { Assert (-not $script:Launched) 'Executed untrusted installer' }
            if ($Case -eq 'timeout') { Assert $script:Killed 'Timed out installer left running' }
        }
    }
    & {
        $Package = Join-Path $Temp 'package [中文] O''Brien'
        $Lib = Join-Path $Package 'lib'
        New-Item -ItemType Directory -Path $Lib -Force | Out-Null
        Set-Content -LiteralPath (Join-Path $Lib 'substrate.dll') -Value invalid
        function Invoke-PolarisLibraryProbe { param($Libraries) return [pscustomobject]@{ready=$script:NativeReady; code=$script:NativeCode; detail='test native failure'} }
        function Install-VisualCRuntime { param([switch]$Repair) $script:Repairs++; $script:NativeReady=$script:RepairWorks }
        foreach ($Code in @(0, 126, 127, 193)) {
            foreach ($RepairWorks in @($true, $false)) {
                $script:NativeReady = $Code -eq 0
                $script:NativeCode = $Code
                $script:RepairWorks = $RepairWorks
                $script:Repairs = 0
                $script:VisualCInstallAttempted = $false
                $Failed = $false
                try { Assert-PolarisNativeRuntime -Root $Package } catch { $Failed = $true }
                Assert ($script:Repairs -eq $(if ($Code -in @(126,127)) {1} else {0})) 'Repaired runtime for an unrelated native error'
                Assert ($Failed -eq ($Code -eq 193 -or ($Code -ne 0 -and -not $RepairWorks))) 'Native failure was silently accepted'
            }
        }
    }
    & {
        function Test-WebView2Runtime { return $script:WebViewReady }
        function Install-MicrosoftComponent { param($Name, $Uri, $Arguments, $ReadyFunction) $script:WebViewInstalls++; Assert ($Uri -eq 'https://go.microsoft.com/fwlink/p/?LinkId=2124703') 'Wrong WebView2 source' }
        $script:WebViewInstalls = 0
        $script:WebViewReady = $true
        Install-WebView2Runtime
        Assert ($script:WebViewInstalls -eq 0) 'Installed existing WebView2'
        $script:WebViewReady = $false
        Install-WebView2Runtime
        Assert ($script:WebViewInstalls -eq 1) 'Did not install missing WebView2'
    }
    & {
        function Test-WorkingFFmpeg { return $script:FFmpegReady }
        function Invoke-RestMethod { param($Uri, $TimeoutSec, $ErrorAction) $script:MetadataCalls++; return [pscustomobject]@{assets=@()} }
        $script:FFmpegReady = $true
        $script:MetadataCalls = 0
        Install-FFmpeg
        Assert ($script:MetadataCalls -eq 0) 'Queried/downloaded existing FFmpeg'
        $script:FFmpegReady = $false
        $Arch = 'amd64'
        $Failed = $false
        try { Install-FFmpeg } catch { $Failed = $true }
        Assert ($Failed -and $script:MetadataCalls -eq 1) 'Accepted FFmpeg asset without trusted metadata'
    }
    if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        $Valid = Join-Path $env:WINDIR 'System32\kernel32.dll'
        Assert (Invoke-PolarisLibraryProbe @($Valid)).ready 'Actual native child probe failed for system DLL'
        $Invalid = Join-Path $Temp 'invalid [中文] O''Brien.dll'
        Set-Content -LiteralPath $Invalid -Value 'invalid DLL'
        Assert (-not (Invoke-PolarisLibraryProbe @($Invalid)).ready) 'Actual child probe accepted corrupt DLL'
    }
    # 原生负向用例的退出码不能被 GitHub Actions 当成整个测试脚本失败。
    $global:LASTEXITCODE = 0
    Write-Host 'PASS: dependency reuse, native architecture, Microsoft signature gate, install failure/timeout/reboot, native DLL repair boundaries, WebView2, FFmpeg metadata'
} finally { Remove-Item -LiteralPath $Temp -Recurse -Force }
