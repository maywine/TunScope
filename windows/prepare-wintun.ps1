[CmdletBinding()]
param(
    [string]$Destination = (Join-Path $PSScriptRoot '..\bin')
)

$ErrorActionPreference = 'Stop'
$version = '0.14.1'
$archiveUrl = "https://www.wintun.net/builds/wintun-$version.zip"
$expectedSha256 = '07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51'
$temporaryRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("mactun-wintun-" + [guid]::NewGuid().ToString('N'))
$archivePath = Join-Path $temporaryRoot 'wintun.zip'
$extractPath = Join-Path $temporaryRoot 'extract'

try {
    New-Item -ItemType Directory -Path $temporaryRoot | Out-Null
    Invoke-WebRequest -Uri $archiveUrl -OutFile $archivePath
    $actualSha256 = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualSha256 -ne $expectedSha256) {
        throw "Wintun archive checksum mismatch: got $actualSha256"
    }
    Expand-Archive -Path $archivePath -DestinationPath $extractPath
    $source = Join-Path $extractPath 'wintun\bin\amd64\wintun.dll'
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "The official archive does not contain bin\amd64\wintun.dll"
    }
    New-Item -ItemType Directory -Path $Destination -Force | Out-Null
    $target = Join-Path $Destination 'wintun.dll'
    Copy-Item -LiteralPath $source -Destination $target -Force
    Write-Host "Installed official Wintun $version AMD64 DLL at $target"
}
finally {
    if (Test-Path -LiteralPath $temporaryRoot) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
