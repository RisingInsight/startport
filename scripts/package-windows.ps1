#Requires -Version 5.1
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$distPath = Join-Path $projectRoot 'dist'
$zipPath = Join-Path $distPath 'StarPort-Windows-x64-Setup.zip'

# This allowlist is the complete package input. Never copy the project tree.
$installerFiles = @('INSTALL.cmd', 'install.ps1', 'README.txt')
$payloadFiles = @(
    'starport.exe'
    'start-local.cmd'
    'stop-local.cmd'
    'LICENSE'
    'scripts/local-server.ps1'
    'README-install.txt'
)

function Get-PayloadSourcePath([string]$RelativePath) {
    if ($RelativePath -eq 'README-install.txt') {
        return (Join-Path $projectRoot 'packaging\windows\README.txt')
    }
    return (Join-Path $projectRoot $RelativePath)
}

foreach ($name in $installerFiles) {
    $sourcePath = Join-Path $projectRoot ('packaging\windows\' + $name)
    if (-not (Test-Path -LiteralPath $sourcePath -PathType Leaf)) {
        throw ('Required installer file is missing: ' + $sourcePath)
    }
}
foreach ($relativePath in $payloadFiles) {
    $sourcePath = Get-PayloadSourcePath $relativePath
    if (-not (Test-Path -LiteralPath $sourcePath -PathType Leaf)) {
        throw ('Required payload file is missing: ' + $sourcePath)
    }
}

New-Item -ItemType Directory -Path $distPath -Force | Out-Null
$stagingPath = Join-Path $distPath ('package-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $stagingPath | Out-Null
$payloadPath = Join-Path $stagingPath 'payload'
New-Item -ItemType Directory -Path $payloadPath | Out-Null

foreach ($name in $installerFiles) {
    Copy-Item -LiteralPath (Join-Path $projectRoot ('packaging\windows\' + $name)) `
        -Destination (Join-Path $stagingPath $name)
}

$manifestEntries = @()
foreach ($relativePath in $payloadFiles) {
    $destinationPath = Join-Path $payloadPath $relativePath
    $destinationParent = Split-Path -Parent $destinationPath
    New-Item -ItemType Directory -Path $destinationParent -Force | Out-Null
    Copy-Item -LiteralPath (Get-PayloadSourcePath $relativePath) -Destination $destinationPath
    $manifestEntries += [ordered]@{
        path = $relativePath
        sha256 = (Get-FileHash -LiteralPath $destinationPath -Algorithm SHA256).Hash.ToLowerInvariant()
    }
}
$manifest = [ordered]@{ files = $manifestEntries } | ConvertTo-Json -Depth 4
$utf8 = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllText((Join-Path $stagingPath 'manifest.json'), $manifest, $utf8)

$packageItems = @(
    (Join-Path $stagingPath 'INSTALL.cmd')
    (Join-Path $stagingPath 'install.ps1')
    (Join-Path $stagingPath 'README.txt')
    (Join-Path $stagingPath 'manifest.json')
    $payloadPath
)
Compress-Archive -LiteralPath $packageItems -DestinationPath $zipPath -CompressionLevel Optimal -Force

# Keep the unique staging directory for review; no recursive deletion is used.
$package = Get-Item -LiteralPath $zipPath
Write-Output ('Created: ' + $package.FullName)
Write-Output ('Size: ' + $package.Length + ' bytes')
Write-Output ('SHA256: ' + (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant())
Write-Output ('Review files: ' + $stagingPath)
