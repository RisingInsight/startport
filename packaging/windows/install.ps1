[CmdletBinding()]
param(
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'Programs\StarPort'),
    [switch]$SkipStart,
    [switch]$NoShortcuts,
    [string]$ShortcutDir
)

$ErrorActionPreference = 'Stop'

function New-StarPortShortcut([string]$ShortcutPath, [string]$TargetPath) {
    [StarPort.WindowsShortcut]::Create($ShortcutPath, $TargetPath, $InstallDir, (Join-Path $InstallDir 'starport.exe'))
}

try {
    $nativeArchitecture = $env:PROCESSOR_ARCHITEW6432
    if (-not $nativeArchitecture) { $nativeArchitecture = $env:PROCESSOR_ARCHITECTURE }
    if ($nativeArchitecture -ne 'AMD64') {
        throw 'This package requires Windows x64 (Intel/AMD). It is not an ARM64 or 32-bit package.'
    }
    if ($PSVersionTable.PSVersion -lt [version]'5.1') {
        throw 'Windows PowerShell 5.1 or newer is required.'
    }

    $payloadRoot = Join-Path $PSScriptRoot 'payload'
    $manifestPath = Join-Path $PSScriptRoot 'manifest.json'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
        throw 'Extract the entire ZIP first, then double-click INSTALL.cmd inside the extracted folder.'
    }
    # This allowlist never includes config.json, auths, data, or local build caches.
    $allowedFiles = @('starport.exe', 'start-local.cmd', 'stop-local.cmd', 'scripts/local-server.ps1', 'scripts/windows-shortcut.ps1', 'LICENSE', 'README-install.txt')
    $manifest = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $entries = @($manifest.files)
    if ($entries.Count -ne $allowedFiles.Count) { throw 'Unexpected installation manifest.' }
    $verifiedFiles = @()
    foreach ($relativePath in $allowedFiles) {
        $matchingEntries = @($entries | Where-Object { $_.path -ceq $relativePath })
        if ($matchingEntries.Count -ne 1) { throw ('Missing or duplicate manifest entry: ' + $relativePath) }
        $entry = $matchingEntries[0]
        if ($entry.sha256 -notmatch '^[0-9a-fA-F]{64}$') { throw ('Invalid hash: ' + $relativePath) }
        $sourcePath = Join-Path $payloadRoot $relativePath
        if (-not (Test-Path -LiteralPath $sourcePath -PathType Leaf)) {
            throw ('Missing package file: ' + $relativePath + '. Extract the entire ZIP first.')
        }
        if ((Get-FileHash -LiteralPath $sourcePath -Algorithm SHA256).Hash -ne $entry.sha256) {
            throw ('Package integrity check failed: ' + $relativePath + '. Copy or extract the ZIP again.')
        }
        $verifiedFiles += [pscustomobject]@{ Path = $relativePath; Source = $sourcePath; Hash = $entry.sha256 }
    }

    $InstallDir = [IO.Path]::GetFullPath($InstallDir).TrimEnd('\', '/')
    if (-not [IO.Path]::IsPathRooted($InstallDir) -or $InstallDir -eq [IO.Path]::GetPathRoot($InstallDir).TrimEnd('\', '/')) {
        throw 'Choose a dedicated installation folder, not a drive root.'
    }
    $fullPayloadRoot = [IO.Path]::GetFullPath($payloadRoot).TrimEnd('\', '/')
    if ($InstallDir -eq $fullPayloadRoot -or $InstallDir.StartsWith($fullPayloadRoot + '\', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Choose an installation folder outside the package payload folder.'
    }

    $installedExe = Join-Path $InstallDir 'starport.exe'
    $binary = $verifiedFiles | Where-Object { $_.Path -eq 'starport.exe' }
    if (Test-Path -LiteralPath $installedExe -PathType Leaf) {
        if ((Get-FileHash -LiteralPath $installedExe -Algorithm SHA256).Hash -ne $binary.Hash) {
            $running = @(Get-Process -Name starport -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $installedExe })
            if ($running.Count -gt 0) {
                throw 'An older StarPort is running in the installation folder. Stop it with stop-local.cmd, then run INSTALL.cmd again.'
            }
        }
    }

    Write-Host 'Package checks passed. Installing StarPort...'
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    foreach ($file in $verifiedFiles) {
        $destination = Join-Path $InstallDir $file.Path
        if (Test-Path -LiteralPath $destination -PathType Leaf) {
            if ((Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash -eq $file.Hash) { continue }
        }
        New-Item -ItemType Directory -Path (Split-Path -Parent $destination) -Force | Out-Null
        Copy-Item -LiteralPath $file.Source -Destination $destination -Force
        if ((Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash -ne $file.Hash) {
            throw ('Installed file verification failed: ' + $file.Path)
        }
    }
    foreach ($directoryName in @('auths', 'data')) {
        New-Item -ItemType Directory -Path (Join-Path $InstallDir $directoryName) -Force | Out-Null
    }

    if (-not $NoShortcuts) {
        try {
            # Load only the verified, installed helper. IShellLinkW preserves
            # paths that cannot be represented in the system ANSI code page.
            . (Join-Path $InstallDir 'scripts\windows-shortcut.ps1')
            $desktopDir = $ShortcutDir
            if (-not $desktopDir) {
                $desktopShell = New-Object -ComObject WScript.Shell
                try { $desktopDir = $desktopShell.SpecialFolders.Item('Desktop') }
                finally { [void][Runtime.InteropServices.Marshal]::ReleaseComObject($desktopShell) }
            }
            if (-not $desktopDir -or -not (Test-Path -LiteralPath $desktopDir -PathType Container)) {
                throw 'Desktop directory is unavailable.'
            }
            New-StarPortShortcut (Join-Path $desktopDir 'StarPort - Start.lnk') (Join-Path $InstallDir 'start-local.cmd')
            New-StarPortShortcut (Join-Path $desktopDir 'StarPort - Stop.lnk') (Join-Path $InstallDir 'stop-local.cmd')
            Write-Host 'Desktop shortcuts created: StarPort - Start / StarPort - Stop.'
        } catch {
            Write-Warning ('Files are installed, but desktop shortcuts could not be created: ' + $_.Exception.Message)
            Write-Host 'Use start-local.cmd and stop-local.cmd in the installation folder.'
        }
    }

    Write-Host ('Installed in: ' + $InstallDir)
    Write-Host 'Existing config.json, auths and data have been preserved.'
    Write-Host 'No Windows service or startup entry has been registered.'
    if (-not $SkipStart) {
        $windowsPowerShell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
        & $windowsPowerShell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $InstallDir 'scripts\local-server.ps1') -Action Start -OpenPanel
        if ($LASTEXITCODE -ne 0) {
            throw 'Files are installed, but StarPort could not start. Read the startup error above, then retry start-local.cmd.'
        }
        Write-Host ('Login key: the api_key value in ' + (Join-Path $InstallDir 'config.json'))
    }
    Write-Host 'StarPort installation complete.'
    exit 0
} catch {
    Write-Host ('Setup error: ' + $_.Exception.Message) -ForegroundColor Red
    exit 1
}
