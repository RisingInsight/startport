#Requires -Version 5.1
[CmdletBinding()]
param([switch]$Live)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$zipPath = Join-Path $projectRoot 'dist\StarPort-Windows-x64-Setup.zip'
$testRoot = Join-Path $projectRoot ('data\installer-test-' + [Guid]::NewGuid().ToString('N'))
$extracted = Join-Path $testRoot 'extracted package'
# Mix scripts so neither Western nor Chinese ANSI code pages can represent
# the complete path. Shortcut creation and readback must use Unicode APIs.
$unicodeSuffix = ([string][char]0x5B89) + [char]0x88C5 + [char]0x1200
$destination = Join-Path $testRoot ($unicodeSuffix + ' installed app')
$shortcutDir = Join-Path $testRoot ($unicodeSuffix + ' test desktop')
$powershellExe = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'

function Assert-True($Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

Add-Type -AssemblyName System.IO.Compression.FileSystem
$archive = [IO.Compression.ZipFile]::OpenRead($zipPath)
try {
    $actual = @($archive.Entries | Where-Object { $_.Name } | ForEach-Object { $_.FullName.Replace('\', '/') })
    $expected = @('INSTALL.cmd', 'install.ps1', 'README.txt', 'manifest.json', 'payload/starport.exe', 'payload/start-local.cmd', 'payload/stop-local.cmd', 'payload/scripts/local-server.ps1', 'payload/scripts/windows-shortcut.ps1', 'payload/LICENSE', 'payload/README-install.txt')
    Assert-True (-not (Compare-Object $expected $actual)) 'Unexpected ZIP contents; refusing to test a package that may include private files.'
} finally { $archive.Dispose() }
Write-Output 'PASS: ZIP contains only the approved files'

New-Item -ItemType Directory -Path $shortcutDir -Force | Out-Null
Expand-Archive -LiteralPath $zipPath -DestinationPath $extracted
$installer = Join-Path $extracted 'install.ps1'
& $powershellExe -NoProfile -ExecutionPolicy Bypass -File $installer -InstallDir $destination -SkipStart -ShortcutDir $shortcutDir
Assert-True ($LASTEXITCODE -eq 0) 'Fresh installation failed.'
Assert-True (-not (Test-Path -LiteralPath (Join-Path $destination 'config.json'))) 'Installer copied a pre-existing configuration.'
Assert-True ((Get-FileHash -LiteralPath (Join-Path $destination 'starport.exe')).Hash -eq (Get-FileHash -LiteralPath (Join-Path $projectRoot 'starport.exe')).Hash) 'Installed executable does not match the built executable.'
. (Join-Path $projectRoot 'scripts\windows-shortcut.ps1')
foreach ($action in @('Start', 'Stop')) {
    $shortcutPath = Join-Path $shortcutDir ('StarPort - ' + $action + '.lnk')
    Assert-True (Test-Path -LiteralPath $shortcutPath -PathType Leaf) 'Installer did not create the shortcut file.'
    $shortcut = [StarPort.WindowsShortcut]::Read($shortcutPath)
    Assert-True ($shortcut.TargetPath -eq (Join-Path $destination ($action.ToLowerInvariant() + '-local.cmd'))) 'Incorrect shortcut target.'
    Assert-True ($shortcut.WorkingDirectory -eq $destination) 'Incorrect shortcut working directory.'
    Assert-True ($shortcut.IconPath -eq (Join-Path $destination 'starport.exe') -and $shortcut.IconIndex -eq 0) 'Incorrect shortcut icon.'
}
Write-Output 'PASS: fresh install, executable identity, and real Windows shortcuts'

$configPath = Join-Path $destination 'config.json'
$helperPath = Join-Path $destination 'scripts\local-server.ps1'
$savedEnvironment = @{}
$started = $false
$launchers = @()
try {
    if ($Live) {
        foreach ($entry in @(Get-ChildItem Env:STARPORT_*)) {
            $savedEnvironment[$entry.Name] = $entry.Value
            [Environment]::SetEnvironmentVariable($entry.Name, $null, 'Process')
        }
        $listener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, 0)
        $listener.Start()
        $port = $listener.LocalEndpoint.Port
        $listener.Stop()
        $env:STARPORT_LISTEN = '127.0.0.1:' + $port
        $started = $true
        # Two overlapping launches must result in one managed, stoppable server.
        foreach ($index in 1..2) {
            $launcher = Start-Process -FilePath $powershellExe -WindowStyle Hidden -PassThru `
                -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', ('"{0}"' -f $helperPath), '-Action', 'Start') `
                -RedirectStandardOutput (Join-Path $testRoot ('start-' + $index + '.out.log')) `
                -RedirectStandardError (Join-Path $testRoot ('start-' + $index + '.err.log'))
            # WinPS 5.1 needs the handle held before exit to retain ExitCode.
            $null = $launcher.Handle
            $launchers += $launcher
        }
        foreach ($launcher in $launchers) {
            Assert-True ($launcher.WaitForExit(60000)) 'Concurrent launcher timed out.'
            Assert-True ($launcher.ExitCode -eq 0) ('Concurrent launcher exit code: ' + $launcher.ExitCode + '; inspect start logs in the test directory.')
        }
        $installedExe = Join-Path $destination 'starport.exe'
        $servers = @(Get-Process -Name starport -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $installedExe })
        $metadata = Get-Content -LiteralPath (Join-Path $destination 'data\local-server.json') -Raw -Encoding UTF8 | ConvertFrom-Json
        Assert-True ($servers.Count -eq 1 -and $servers[0].Id -eq $metadata.process_id) 'Concurrent launches did not preserve exactly one managed process.'
        Write-Output 'PASS: concurrent starts preserve one managed process'
        $config = Get-Content -LiteralPath $configPath -Raw -Encoding UTF8 | ConvertFrom-Json
        Assert-True ($config.api_key.Length -ge 24) 'First start did not create a strong key.'
        $overview = Invoke-RestMethod -Uri ('http://127.0.0.1:' + $port + '/startport/api/overview') -Headers @{ Authorization = 'Bearer ' + $config.api_key } -TimeoutSec 10
        Assert-True ($overview.auth_required -and $overview.total -eq 0) 'New installation did not start with independent empty account state.'
        $originalConfigPath = Join-Path $projectRoot 'config.json'
        if (Test-Path -LiteralPath $originalConfigPath) {
            $original = Get-Content -LiteralPath $originalConfigPath -Raw -Encoding UTF8 | ConvertFrom-Json
            Assert-True ($config.api_key -ne $original.api_key) 'Installed application reused the original admin key.'
        }
        Write-Output 'PASS: installed app starts on an isolated port with a new key and authenticated panel'
    } else {
        '{"api_key":"test-only-preservation-marker"}' | Set-Content -LiteralPath $configPath -Encoding UTF8
    }

    $authMarker = Join-Path $destination 'auths\preserve-test.txt'
    $dataMarker = Join-Path $destination 'data\preserve-test.txt'
    'auth preservation marker' | Set-Content -LiteralPath $authMarker
    'data preservation marker' | Set-Content -LiteralPath $dataMarker
    $preservedPaths = @($configPath, $authMarker, $dataMarker)
    $previousHashes = @($preservedPaths | ForEach-Object { (Get-FileHash -LiteralPath $_).Hash })
    & $powershellExe -NoProfile -ExecutionPolicy Bypass -File $installer -InstallDir $destination -SkipStart -NoShortcuts
    Assert-True ($LASTEXITCODE -eq 0) 'Repeated installation failed.'
    $currentHashes = @($preservedPaths | ForEach-Object { (Get-FileHash -LiteralPath $_).Hash })
    Assert-True (-not (Compare-Object $previousHashes $currentHashes)) 'Reinstallation changed configuration or account data.'
    Write-Output 'PASS: reinstallation preserves config, auths, and data'
} finally {
    try {
        foreach ($launcher in $launchers) {
            if (-not $launcher.HasExited) { Stop-Process -InputObject $launcher -ErrorAction Stop }
        }
        if ($started) {
            & $powershellExe -NoProfile -ExecutionPolicy Bypass -File $helperPath -Action Stop
            Assert-True ($LASTEXITCODE -eq 0) ('Could not stop isolated test application: ' + $destination)
            $remaining = @(Get-Process -Name starport -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq (Join-Path $destination 'starport.exe') })
            Assert-True ($remaining.Count -eq 0) 'Stop left the installed test application running.'
            Write-Output 'PASS: stop leaves no test server running'
        }
    } finally {
        if ($Live) {
            [Environment]::SetEnvironmentVariable('STARPORT_LISTEN', $null, 'Process')
            foreach ($name in $savedEnvironment.Keys) { [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process') }
        }
    }
}

'corrupted test payload' | Add-Content -LiteralPath (Join-Path $extracted 'payload\LICENSE')
$rejectedDestination = Join-Path $testRoot 'must not be installed'
& $powershellExe -NoProfile -ExecutionPolicy Bypass -File $installer -InstallDir $rejectedDestination -SkipStart -NoShortcuts
Assert-True ($LASTEXITCODE -ne 0) 'A corrupted package was accepted.'
Assert-True (-not (Test-Path -LiteralPath $rejectedDestination)) 'Corrupted package changed the target before verification.'
Write-Output 'PASS: corrupted package is rejected before installation'
Write-Output ('Test files retained in: ' + $testRoot)
# The expected corrupt-package failure above must not leak into CI's exit code.
exit 0
