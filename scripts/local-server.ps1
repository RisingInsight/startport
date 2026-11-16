[CmdletBinding()]
param(
    [ValidateSet('Start', 'Stop')]
    [string]$Action = 'Start',
    [switch]$OpenPanel
)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$exePath = Join-Path $projectRoot 'starport.exe'
$configPath = Join-Path $projectRoot 'config.json'
$dataPath = Join-Path $projectRoot 'data'
$metadataPath = Join-Path $dataPath 'local-server.json'
$logPath = Join-Path $dataPath 'logs'
$lifecycleMutex = $null
$ownsLifecycleMutex = $false

function Get-ManagedProcess {
    if (-not (Test-Path -LiteralPath $metadataPath -PathType Leaf)) { return $null }
    $metadata = Get-Content -LiteralPath $metadataPath -Raw | ConvertFrom-Json
    $serverProcess = Get-Process -Id $metadata.process_id -ErrorAction SilentlyContinue
    if ($null -eq $serverProcess) { return $null }
    if ($metadata.exe -ne $exePath -or $serverProcess.Path -ne $exePath -or
        [string]$serverProcess.StartTime.ToUniversalTime().Ticks -ne $metadata.started_ticks) {
        if ($Action -eq 'Start') { return $null }
        throw 'Saved process identity does not match. No process was stopped. Check data\local-server.json.'
    }
    return $serverProcess
}

function Save-ProcessIdentity($ServerProcess) {
    @{
        process_id = $ServerProcess.Id
        exe = $exePath
        started_ticks = [string]$ServerProcess.StartTime.ToUniversalTime().Ticks
        started_utc = $ServerProcess.StartTime.ToUniversalTime().ToString('o')
    } | ConvertTo-Json | Set-Content -LiteralPath $metadataPath -Encoding UTF8
}

function Test-Alive {
    try {
        $response = Invoke-RestMethod -Uri ($baseUrl + '/livez') -TimeoutSec 2
        return ($response.alive -eq $true -and $response.service -eq 'starport')
    } catch {
        return $false
    }
}

try {
    # Serialize process discovery, launch, and metadata writes for this install.
    # Case normalization makes Windows path spellings share the same lock.
    $hasher = [Security.Cryptography.SHA256]::Create()
    try {
        $identity = [IO.Path]::GetFullPath($exePath).ToUpperInvariant()
        $hash = $hasher.ComputeHash([Text.Encoding]::UTF8.GetBytes($identity))
        $lockName = 'Local\StarPort-' + [BitConverter]::ToString($hash).Replace('-', '')
    } finally {
        $hasher.Dispose()
    }
    $lifecycleMutex = New-Object Threading.Mutex($false, $lockName)
    try {
        $ownsLifecycleMutex = $lifecycleMutex.WaitOne(45000)
    } catch [Threading.AbandonedMutexException] {
        # The previous launcher exited unexpectedly; this thread owns the mutex.
        $ownsLifecycleMutex = $true
    }
    if (-not $ownsLifecycleMutex) {
        throw 'Another StarPort start/stop operation is still running. Wait for it to finish, then retry.'
    }

    $serverProcess = Get-ManagedProcess
    if ($Action -eq 'Stop') {
        if ($null -eq $serverProcess) {
            Write-Host 'StarPort is not running as a managed local process.'
            exit 0
        }
        Write-Host 'Stopping StarPort. Any requests currently in progress will be terminated.'
        try {
            Stop-Process -InputObject $serverProcess -ErrorAction Stop
        } catch {
            $serverProcess.Refresh()
            if (-not $serverProcess.HasExited) { throw }
        }
        if (-not $serverProcess.WaitForExit(10000)) {
            throw 'StarPort did not exit within 10 seconds. The process record was preserved; check Task Manager and retry.'
        }
        Remove-Item -LiteralPath $metadataPath
        Write-Host 'StarPort stopped.'
        exit 0
    }

    if (-not (Test-Path -LiteralPath $exePath -PathType Leaf)) {
        throw 'starport.exe is missing. Build the project before starting it.'
    }
    $listen = '127.0.0.1:7863'
    if (Test-Path -LiteralPath $configPath -PathType Leaf) {
        $config = Get-Content -LiteralPath $configPath -Raw -Encoding UTF8 | ConvertFrom-Json
        if ($config.listen) { $listen = $config.listen }
    }
    if ($env:STARPORT_LISTEN) { $listen = $env:STARPORT_LISTEN }
    if ($listen.StartsWith(':')) { $listen = '127.0.0.1' + $listen }
    $listen = $listen -replace '^0\.0\.0\.0:', '127.0.0.1:' -replace '^\[::\]:', '[::1]:'
    $baseUrl = 'http://' + $listen
    New-Item -ItemType Directory -Path $logPath -Force | Out-Null

    if ($null -eq $serverProcess) {
        $existing = @(Get-Process -Name 'starport' -ErrorAction SilentlyContinue |
            Where-Object { $_.Path -eq $exePath })
        if ($existing.Count -gt 1) { throw 'Multiple copies of this StarPort executable are already running.' }
        if ($existing.Count -eq 1) {
            $serverProcess = $existing[0]
        } else {
            if (Test-Alive) {
                throw 'Another StarPort instance is using the configured address. No new instance was started.'
            }
            $stdoutPath = Join-Path $logPath 'starport.stdout.log'
            $stderrPath = Join-Path $logPath 'starport.stderr.log'
            $serverProcess = Start-Process -FilePath $exePath -WorkingDirectory $projectRoot `
                -ArgumentList @('-config', ('"{0}"' -f $configPath), '-open=false') `
                -WindowStyle Hidden -RedirectStandardOutput $stdoutPath `
                -RedirectStandardError $stderrPath -PassThru
        }
        Save-ProcessIdentity $serverProcess
    }

    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $serverProcess.Refresh()
        if ($serverProcess.HasExited) {
            throw ('StarPort exited before becoming ready. Check logs in ' + $logPath)
        }
        if (Test-Alive) {
            Write-Host ('StarPort is running (PID ' + $serverProcess.Id + ').')
            Write-Host ('Panel: ' + $baseUrl + '/startport/')
            Write-Host ('Admin key: api_key in ' + $configPath)
            Write-Host ('Logs: ' + $logPath)
            if ($OpenPanel) { Start-Process ($baseUrl + '/startport/') }
            exit 0
        }
        Start-Sleep -Milliseconds 250
    } while ([DateTime]::UtcNow -lt $deadline)
    throw ('No successful /livez response after 30 seconds. Check logs in ' + $logPath + '. Use stop-local.cmd to stop the process.')
} catch {
    Write-Host ('Error: ' + $_.Exception.Message) -ForegroundColor Red
    exit 1
} finally {
    if ($null -ne $lifecycleMutex) {
        try {
            if ($ownsLifecycleMutex) { $lifecycleMutex.ReleaseMutex() }
        } finally {
            $lifecycleMutex.Dispose()
        }
    }
}
