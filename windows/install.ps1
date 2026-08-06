[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string]$SourceDirectory = $PSScriptRoot,

    [string]$InstallDirectory = (Join-Path $env:ProgramFiles 'MacTun'),

    [switch]$AddToMachinePath
)

$ErrorActionPreference = 'Stop'

function Test-IsAdministrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

if (-not $WhatIfPreference -and -not (Test-IsAdministrator)) {
    throw 'Run install.ps1 from an elevated PowerShell window.'
}
if ([string]::IsNullOrWhiteSpace($InstallDirectory)) {
    throw 'InstallDirectory cannot be empty.'
}
if ($InstallDirectory.Contains(';') -or $InstallDirectory.Contains("`r") -or $InstallDirectory.Contains("`n")) {
    throw 'InstallDirectory contains an unsupported character.'
}

$sourcePath = [System.IO.Path]::GetFullPath($SourceDirectory)
$installPath = [System.IO.Path]::GetFullPath($InstallDirectory).TrimEnd('\')
$installRoot = [System.IO.Path]::GetPathRoot($installPath).TrimEnd('\')
if ($installPath -eq $installRoot) {
    throw "Refusing to install directly into a drive root: $installPath"
}
if ([string]::Equals($sourcePath.TrimEnd('\'), $installPath, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "SourceDirectory and InstallDirectory must be different: $installPath"
}

$requiredFiles = @('mactun.exe', 'wintun.dll')
foreach ($requiredFile in $requiredFiles) {
    $sourceFile = Join-Path $sourcePath $requiredFile
    if (-not (Test-Path -LiteralPath $sourceFile -PathType Leaf)) {
        throw "The package is missing $requiredFile in $sourcePath"
    }
}

$installedExecutable = Join-Path $installPath 'mactun.exe'
if (Test-Path -LiteralPath $installedExecutable -PathType Leaf) {
    $runningInstall = Get-Process -Name 'mactun' -ErrorAction SilentlyContinue | Where-Object {
        try {
            [string]::Equals($_.Path, $installedExecutable, [System.StringComparison]::OrdinalIgnoreCase)
        }
        catch {
            $false
        }
    }
    if ($runningInstall) {
        throw "The installed MacTun is running. Stop it manually with '$installedExecutable down', then run the installer again."
    }
}

$packageFiles = @(
    'mactun.exe',
    'wintun.dll',
    'README.md',
    'LICENSE.txt',
    'WINTUN-LICENSE.txt',
    'mactun.example.json',
    'prepare-wintun.ps1',
    'install.ps1'
)

if ($PSCmdlet.ShouldProcess($installPath, 'Install MacTun')) {
    New-Item -ItemType Directory -Path $installPath -Force | Out-Null
    foreach ($packageFile in $packageFiles) {
        $sourceFile = Join-Path $sourcePath $packageFile
        if (Test-Path -LiteralPath $sourceFile -PathType Leaf) {
            Copy-Item -LiteralPath $sourceFile -Destination (Join-Path $installPath $packageFile) -Force
        }
    }

    if ($AddToMachinePath) {
        $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
        $pathEntries = @($machinePath -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
        $alreadyPresent = $pathEntries | Where-Object {
            [string]::Equals($_.TrimEnd('\'), $installPath, [System.StringComparison]::OrdinalIgnoreCase)
        }
        if (-not $alreadyPresent) {
            $newMachinePath = (($pathEntries + $installPath) -join ';')
            [Environment]::SetEnvironmentVariable('Path', $newMachinePath, 'Machine')
            Write-Host 'Added MacTun to the machine PATH. Open a new terminal to use it.'
        }
    }

    Write-Host "Installed MacTun at $installPath"
    Write-Host "Run '$installedExecutable doctor --proxy socks5://127.0.0.1:7890' before starting TUN."
    Write-Host 'The installer did not start or stop MacTun.'
}
