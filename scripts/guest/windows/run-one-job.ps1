# Runs at `runner` logon (scheduled task ghq-run-one-job, interactive,
# highest privileges) inside the ephemeral job VM. Runs exactly one job,
# then powers the guest off NO MATTER WHAT -- host-side teardown depends
# on the qemu process exiting.
$ErrorActionPreference = 'Continue'
# Log degrades to the console alone while the serial port is missing or
# closed, so logging from the failure paths below can never itself throw.
$serial = $null
function Log([string]$msg) {
    $line = "[run-one-job $(Get-Date -Format HH:mm:ss)] $msg"
    if ($serial -and $serial.IsOpen) { $serial.WriteLine($line) } else { Write-Host $line }
}

# The outermost try exists for its finally: host-side teardown waits on
# the qemu process, so serial construction and Open() must sit INSIDE it.
# Outside, a COM1 failure would terminate the script before any finally
# ran and strand the VM.
try {
    $serial = New-Object System.IO.Ports.SerialPort 'COM1', 115200, 'None', 8, 'One'
    $serial.Open()

    try {
        Log "start; user=$env:USERNAME session=$((Get-Process -Id $PID).SessionId)"

        # Windows does not grow the system volume into new disk space; the
        # overlay may be larger than the base (pool disk_gb).
        $part = Get-Partition -DriveLetter C
        $max = (Get-PartitionSupportedSize -DriveLetter C).SizeMax
        if ($max - $part.Size -gt 100MB) {
            Resize-Partition -DriveLetter C -Size $max
            Log "C: extended to $([math]::Round($max / 1GB)) GB"
        }

        $vol = Get-Volume | Where-Object { $_.FileSystemLabel -eq 'GHQSEED' } | Select-Object -First 1
        if (-not $vol) { throw 'seed volume GHQSEED not found' }
        $jitFile = "$($vol.DriveLetter):\runner-jit.conf"
        if (-not (Test-Path $jitFile)) { throw "run-one-job: $jitFile missing" }
        $jit = (Get-Content $jitFile -Raw).Trim()
        if (-not $jit) { throw "run-one-job: $jitFile empty" }

        # The JIT config registers a pre-created ephemeral runner; run.cmd
        # executes one job, deregisters, and exits. The blob is single-use.
        Set-Location 'C:\actions-runner'
        & 'C:\actions-runner\run.cmd' --jitconfig $jit
        Log "runner exited rc=$LASTEXITCODE"
    } catch {
        Log "JOB-FAILED: $($_.Exception.Message)"
    } finally {
        if ($serial -and $serial.IsOpen) { $serial.Close() }
    }
} finally {
    Stop-Computer -Force
}
