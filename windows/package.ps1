[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Binary,

    [string]$GuiBinary,

    [string]$Version,

    [string]$Destination = (Join-Path $PSScriptRoot '..\dist')
)

$ErrorActionPreference = 'Stop'

$resolvedBinary = (Resolve-Path -LiteralPath $Binary -ErrorAction Stop).Path
if (-not (Test-Path -LiteralPath $resolvedBinary -PathType Leaf)) {
    throw "TunScope binary does not exist: $Binary"
}

$resolvedGuiBinary = $null
if (-not [string]::IsNullOrWhiteSpace($GuiBinary)) {
    $resolvedGuiBinary = (Resolve-Path -LiteralPath $GuiBinary -ErrorAction Stop).Path
    if (-not (Test-Path -LiteralPath $resolvedGuiBinary -PathType Leaf)) {
        throw "TunScope GUI binary does not exist: $GuiBinary"
    }
}

if ([string]::IsNullOrWhiteSpace($Version)) {
    $versionOutput = (& $resolvedBinary version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw "Unable to read the TunScope version from $resolvedBinary"
    }
    $versionMatch = [regex]::Match($versionOutput, '^tunscope\s+(.+)$')
    if (-not $versionMatch.Success) {
        throw "Unexpected TunScope version output: $versionOutput"
    }
    $Version = $versionMatch.Groups[1].Value
}

if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$') {
    throw "Version must be a semantic version without a leading v: $Version"
}

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$readme = Join-Path $PSScriptRoot 'README.md'
$exampleConfig = Join-Path $PSScriptRoot 'tunscope.example.json'
$prepareWintun = Join-Path $PSScriptRoot 'prepare-wintun.ps1'
$installer = Join-Path $PSScriptRoot 'install.ps1'
$license = Join-Path $repositoryRoot 'LICENSE'
$requiredInputs = @($readme, $exampleConfig, $prepareWintun, $installer, $license)
foreach ($requiredInput in $requiredInputs) {
    if (-not (Test-Path -LiteralPath $requiredInput -PathType Leaf)) {
        throw "Required package input does not exist: $requiredInput"
    }
}

$destinationPath = [System.IO.Path]::GetFullPath($Destination)
$temporaryRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("tunscope-package-" + [guid]::NewGuid().ToString('N'))
$packageName = "tunscope-$Version-windows-amd64"
$stagingDirectory = Join-Path $temporaryRoot $packageName
$archivePath = Join-Path $destinationPath "$packageName.zip"
$checksumPath = "$archivePath.sha256"

try {
    New-Item -ItemType Directory -Path $stagingDirectory -Force | Out-Null
    Copy-Item -LiteralPath $resolvedBinary -Destination (Join-Path $stagingDirectory 'tunscope.exe')
    if ($null -ne $resolvedGuiBinary) {
        Copy-Item -LiteralPath $resolvedGuiBinary -Destination (Join-Path $stagingDirectory 'TunScope.GUI.exe')
    }
    Copy-Item -LiteralPath $readme -Destination (Join-Path $stagingDirectory 'README.md')
    Copy-Item -LiteralPath $exampleConfig -Destination (Join-Path $stagingDirectory 'tunscope.example.json')
    Copy-Item -LiteralPath $prepareWintun -Destination (Join-Path $stagingDirectory 'prepare-wintun.ps1')
    Copy-Item -LiteralPath $installer -Destination (Join-Path $stagingDirectory 'install.ps1')
    Copy-Item -LiteralPath $license -Destination (Join-Path $stagingDirectory 'LICENSE.txt')

    & $prepareWintun -Destination $stagingDirectory

    New-Item -ItemType Directory -Path $destinationPath -Force | Out-Null
    if (Test-Path -LiteralPath $archivePath) {
        Remove-Item -LiteralPath $archivePath -Force
    }
    if (Test-Path -LiteralPath $checksumPath) {
        Remove-Item -LiteralPath $checksumPath -Force
    }
    Compress-Archive -LiteralPath $stagingDirectory -DestinationPath $archivePath -CompressionLevel Optimal

    $archiveHash = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    $checksumLine = "$archiveHash  $([System.IO.Path]::GetFileName($archivePath))`n"
    $utf8WithoutBom = New-Object -TypeName System.Text.UTF8Encoding -ArgumentList $false
    [System.IO.File]::WriteAllText($checksumPath, $checksumLine, $utf8WithoutBom)

    Write-Host "Created $archivePath"
    Write-Host "Created $checksumPath"
}
finally {
    if (Test-Path -LiteralPath $temporaryRoot) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
