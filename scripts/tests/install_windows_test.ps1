$ErrorActionPreference = 'Stop'
$Root = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
$Source = Join-Path $Root 'scripts/install.ps1'
$Tokens = $null
$ParseErrors = $null
$Ast = [System.Management.Automation.Language.Parser]::ParseFile($Source, [ref]$Tokens, [ref]$ParseErrors)
if ($ParseErrors.Count) { throw ($ParseErrors | Out-String) }
$null = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $Root 'scripts/uninstall.ps1'), [ref]$Tokens, [ref]$ParseErrors)
if ($ParseErrors.Count) { throw ($ParseErrors | Out-String) }

# 只加载待测函数，不运行安装主体或写入用户注册表。
foreach ($Name in @('Update-SessionPath', 'Test-WorkingPython', 'Test-WorkingNode', 'Test-WorkingUv')) {
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
    $Text = Get-Content $Source -Raw
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
    Write-Host 'PASS: parser, session PATH, Python store aliases, Node command shims, uvx, WOW64 architecture'
} finally {
    $env:Path = $SavedPath
    $env:USERPROFILE = $SavedProfile
    $env:ProgramFiles = $SavedProgramFiles
    $env:LOCALAPPDATA = $SavedLocalAppData
    $env:PROCESSOR_ARCHITECTURE = $SavedArch
    $env:PROCESSOR_ARCHITEW6432 = $SavedNativeArch
    Remove-Item $TestDir -Recurse -Force -ErrorAction SilentlyContinue
}
