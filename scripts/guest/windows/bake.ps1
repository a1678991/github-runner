# Runs ONCE as Administrator (unattend FirstLogonCommands) during the
# image bake boot. Installs virtio drivers, the runner user, Git, the build
# toolchain (via WinGet), and the actions runner, then powers off. The host watches the serial console for
# BAKE-OK; any failure prints BAKE-FAILED and powers off so the bake is
# rejected quickly instead of hanging until the host timeout.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# Log degrades to the console alone while the serial port is missing or
# closed, so logging from the failure paths below can never itself throw.
$serial = $null
function Log([string]$msg) {
    $line = "[bake $(Get-Date -Format HH:mm:ss)] $msg"
    if ($serial -and $serial.IsOpen) { $serial.WriteLine($line) }
    Write-Host $line
}

function Find-VolumeByLabel([string]$label) {
    $v = Get-Volume | Where-Object { $_.FileSystemLabel -eq $label } | Select-Object -First 1
    if (-not $v) { throw "volume with label $label not found" }
    return "$($v.DriveLetter):"
}

function Get-Verified([string]$url, [string]$dest, [string]$sha256) {
    Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $dest
    if ($sha256) {
        $got = (Get-FileHash $dest -Algorithm SHA256).Hash.ToLower()
        if ($got -ne $sha256.ToLower()) { throw "checksum mismatch for $url : got $got want $sha256" }
        Log "verified sha256 of $(Split-Path $dest -Leaf)"
    } else {
        Log "no checksum for $(Split-Path $dest -Leaf); relying on TLS only"
    }
}

# The outermost try exists for its finally: powering the guest off is the
# only way the host's bake ever finishes, so serial construction and
# Open() must sit INSIDE it. Outside, a COM1 failure would terminate the
# script before any finally ran and leave qemu up until the host timeout.
try {
    $serial = New-Object System.IO.Ports.SerialPort 'COM1', 115200, 'None', 8, 'One'
    $serial.Open()

    try {
        $os = Get-CimInstance Win32_OperatingSystem
        Log "start; $($os.Caption) build $($os.BuildNumber) firmware=$env:firmware_type"
        $seed = Find-VolumeByLabel 'GHQSEED'
        $env_ = Get-Content "$seed\bake-env.json" -Raw | ConvertFrom-Json

        # --- virtio drivers ---------------------------------------------------
        # The dummy virtio-blk disk the host attaches makes viostor bind to real
        # hardware here, which is what registers it as a boot-start driver so
        # clones can boot from virtio-blk. NetKVM binding brings the network up.
        $vwin = (Get-Volume | Where-Object { $_.DriveType -eq 'CD-ROM' -and (Test-Path "$($_.DriveLetter):\virtio-win-guest-tools.exe") } | Select-Object -First 1)
        if (-not $vwin) { throw 'virtio-win ISO not found' }
        $vwin = "$($vwin.DriveLetter):"
        $osDir = if (Test-Path "$vwin\viostor\2k25") { '2k25' } else { '2k22' }
        Log "virtio-win at $vwin, driver folder $osDir"
        foreach ($drv in 'viostor', 'NetKVM', 'vioscsi', 'Balloon', 'viorng', 'vioserial', 'pvpanic', 'qemufwcfg') {
            $inf = Get-ChildItem "$vwin\$drv\$osDir\amd64\*.inf" -ErrorAction SilentlyContinue | Select-Object -First 1
            if (-not $inf) { throw "driver $drv not found under $vwin\$drv\$osDir\amd64" }
            # Under $ErrorActionPreference = 'Stop', the stderr that 2>&1
            # merges into the success stream arrives as ErrorRecords and is
            # escalated to a terminating NativeCommandError before the rc
            # check below can run. Relax the preference for the call only.
            $saved = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            try {
                $out = & pnputil.exe /add-driver $inf.FullName /install 2>&1 | Out-String
            } finally {
                $ErrorActionPreference = $saved
            }
            if ($LASTEXITCODE -ne 0) { throw "pnputil $drv failed rc=$LASTEXITCODE : $out" }
            Log "installed $drv"
        }
        Start-Sleep -Seconds 5
        $viostor = Get-CimInstance Win32_SystemDriver -Filter "Name='viostor'"
        Log "viostor state=$($viostor.State) startmode=$($viostor.StartMode)"
        if ($viostor.StartMode -ne 'Boot') { throw 'viostor is not a boot-start driver; clones would not boot from virtio-blk' }

        $deadline = (Get-Date).AddMinutes(3)
        $ip = $null
        while ((Get-Date) -lt $deadline) {
            $ip = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -like '10.0.2.*' }
            if ($ip) { break }
            Start-Sleep -Seconds 3
        }
        if (-not $ip) { throw 'no 10.0.2.x address within 3 minutes (virtio-net not up)' }
        Log "network up: $($ip.IPAddress)"

        # --- runner user + autologon -----------------------------------------
        # Interactive session for GitHub-hosted parity (GUI-touching tests
        # work). The password never leaves this VM; the image is disposable.
        $pw = 'Aa1' + (-join ((48..57) + (65..90) + (97..122) | Get-Random -Count 29 | ForEach-Object { [char]$_ }))
        New-LocalUser -Name runner -Password (ConvertTo-SecureString $pw -AsPlainText -Force) -PasswordNeverExpires -AccountNeverExpires | Out-Null
        Add-LocalGroupMember -Group Administrators -Member runner
        $wl = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
        Set-ItemProperty $wl AutoAdminLogon -Value '1' -Type String
        Set-ItemProperty $wl DefaultUserName -Value 'runner' -Type String
        Set-ItemProperty $wl DefaultPassword -Value $pw -Type String
        Set-ItemProperty $wl DefaultDomainName -Value $env:COMPUTERNAME -Type String
        Remove-ItemProperty $wl AutoLogonCount -ErrorAction SilentlyContinue
        Log 'runner user + autologon configured'

        # --- policies and services -------------------------------------------
        Set-Service wuauserv -StartupType Disabled
        Stop-Service wuauserv -Force -ErrorAction SilentlyContinue
        Get-ScheduledTask -TaskName ServerManager -ErrorAction SilentlyContinue | Disable-ScheduledTask | Out-Null
        New-Item -Path 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\OOBE' -Force | Out-Null
        Set-ItemProperty 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\OOBE' DisablePrivacyExperience -Value 1 -Type DWord
        Set-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\TimeZoneInformation' RealTimeIsUniversal -Value 1 -Type DWord
        Set-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' LongPathsEnabled -Value 1 -Type DWord
        powercfg /change monitor-timeout-ac 0 | Out-Null
        powercfg /change standby-timeout-ac 0 | Out-Null
        Log 'policies applied'

        # --- Git for Windows ---------------------------------------------------
        Get-Verified $env_.git_url 'C:\git-installer.exe' $env_.git_sha256
        $p = Start-Process 'C:\git-installer.exe' -ArgumentList '/VERYSILENT', '/NORESTART', '/NOCANCEL', '/SP-', '/COMPONENTS=""', '/o:PathOption=CmdTools' -Wait -PassThru
        if ($p.ExitCode -ne 0) { throw "git installer exit code $($p.ExitCode)" }
        Remove-Item 'C:\git-installer.exe'
        # Same NativeCommandError trap as pnputil above; the explicit
        # Test-Path and rc check keep a broken install fatal even though the
        # preference is relaxed around the call.
        $gitExe = 'C:\Program Files\Git\cmd\git.exe'
        if (-not (Test-Path $gitExe)) { throw "git not found at $gitExe after install" }
        $saved = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try {
            $gitVer = (& $gitExe --version 2>&1 | Out-String).Trim()
        } finally {
            $ErrorActionPreference = $saved
        }
        if ($LASTEXITCODE -ne 0) { throw "git --version failed rc=$LASTEXITCODE : $gitVer" }
        Log "git: $gitVer"

        # --- build toolchain via WinGet ----------------------------------------
        # WinGet ships in App Installer on Server 2025, but is registered for a
        # user only asynchronously after the first logon — and this script IS
        # the first logon. Register it, then bootstrap the current client via
        # Repair-WinGetPackageManager (Windows Update is off, so the image's
        # App Installer may predate the manifests' schema).
        # https://learn.microsoft.com/windows/package-manager/winget/
        Add-AppxPackage -RegisterByFamilyName -MainPackage Microsoft.DesktopAppInstaller_8wekyb3d8bbwe -ErrorAction SilentlyContinue
        Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Force | Out-Null
        Install-Module -Name Microsoft.WinGet.Client -Repository PSGallery -Scope AllUsers -Force | Out-Null
        Repair-WinGetPackageManager -AllUsers -Latest
        $winget = (Get-Command winget.exe -ErrorAction SilentlyContinue).Source
        if (-not $winget) { $winget = "$env:LOCALAPPDATA\Microsoft\WindowsApps\winget.exe" }
        if (-not (Test-Path $winget)) { throw "winget not found after Repair-WinGetPackageManager" }
        $saved = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try {
            $wgVer = (& $winget --version 2>&1 | Out-String).Trim()
        } finally {
            $ErrorActionPreference = $saved
        }
        if ($LASTEXITCODE -ne 0) { throw "winget --version failed rc=$LASTEXITCODE : $wgVer" }
        Log "winget: $wgVer"

        # Success, or an outcome that leaves the package installed:
        # 0x8A15002B no applicable update, 0x8A150061 / 0x8A15010D already
        # installed, 0x8A15010E newer already installed, 0x8A150109 reboot
        # required (the bake powers off at the end anyway).
        # https://github.com/microsoft/winget-cli/blob/master/doc/windows/package-manager/winget/returnCodes.md
        $wingetOK = @(0, -1978335189, -1978335135, -1978334963, -1978334962, -1978334967)
        function Install-WinGetPackage([string]$id, [string[]]$extra = @()) {
            $wgArgs = @('install', '--id', $id, '--exact', '--source', 'winget', '--silent',
                '--accept-package-agreements', '--accept-source-agreements', '--disable-interactivity') + $extra
            $saved = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            try {
                $out = & $winget @wgArgs 2>&1 | Out-String
            } finally {
                $ErrorActionPreference = $saved
            }
            $rc = $LASTEXITCODE
            if ($wingetOK -notcontains $rc) { throw "winget install $id failed rc=$rc : $out" }
            Log "installed $id (rc=$rc)"
        }

        # Every Visual C++ redistributable, both architectures: prebuilt
        # tools (pnpm, node addons, ...) die with 0xC0000135 without them.
        foreach ($year in '2005', '2008', '2010', '2012', '2013', '2015+') {
            foreach ($arch in 'x86', 'x64') { Install-WinGetPackage "Microsoft.VCRedist.$year.$arch" }
        }
        # MSBuild + MSVC v143 x64/x86 + Windows 11 SDK (signtool): the VCTools
        # workload requires MSBuild and recommends the compiler and SDK.
        # --override replaces winget's default installer switches, so the
        # silent/wait flags are repeated here.
        # https://learn.microsoft.com/visualstudio/install/workload-component-id-vs-build-tools
        Install-WinGetPackage 'Microsoft.VisualStudio.2022.BuildTools' @('--override',
            '--wait --quiet --norestart --nocache --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended')
        $vswhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
        $vsPath = if (Test-Path $vswhere) { & $vswhere -latest -products * -requires Microsoft.Component.MSBuild Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath }
        if (-not $vsPath) { throw 'VS Build Tools installed but MSBuild/MSVC not found by vswhere' }
        Log "VS Build Tools at $vsPath"
        # CLI tools jobs use. jq is a portable package: machine scope puts it
        # under Program Files with a machine-PATH link, visible to `runner`
        # (default user scope would land in this Administrator's profile).
        Install-WinGetPackage 'GitHub.cli'
        Install-WinGetPackage 'jqlang.jq' @('--scope', 'machine')

        # --- actions-runner ----------------------------------------------------
        Get-Verified $env_.runner_url 'C:\runner.zip' $env_.runner_sha256
        New-Item -ItemType Directory -Force 'C:\actions-runner' | Out-Null
        Expand-Archive 'C:\runner.zip' -DestinationPath 'C:\actions-runner' -Force
        Remove-Item 'C:\runner.zip'
        if (-not (Test-Path 'C:\actions-runner\run.cmd')) { throw 'run.cmd missing after extract' }
        Log "actions-runner $($env_.runner_version) extracted"

        # --- run-one-job startup task ----------------------------------------
        New-Item -ItemType Directory -Force 'C:\ghq' | Out-Null
        Copy-Item "$seed\run-one-job.ps1" 'C:\ghq\run-one-job.ps1'
        $action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument '-NoProfile -ExecutionPolicy Bypass -File C:\ghq\run-one-job.ps1'
        $trigger = New-ScheduledTaskTrigger -AtLogOn -User 'runner'
        $principal = New-ScheduledTaskPrincipal -UserId 'runner' -LogonType Interactive -RunLevel Highest
        $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Days 3) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
        Register-ScheduledTask -TaskName 'ghq-run-one-job' -Action $action -Trigger $trigger -Principal $principal -Settings $settings | Out-Null
        Log 'scheduled task registered'

        # --- licence -----------------------------------------------------------
        try {
            & cscript.exe //nologo C:\Windows\System32\slmgr.vbs /ato | Out-Null
            $lic = Get-CimInstance SoftwareLicensingProduct -Filter "PartialProductKey IS NOT NULL AND ApplicationID='55c92734-d682-4d71-983e-d6ec3f16059f'" | Select-Object -First 1  # well-known Windows SLID, not a secret
            Log "licence status=$($lic.LicenseStatus) grace=$($lic.GracePeriodRemaining)min"
        } catch { Log "licence activation skipped: $($_.Exception.Message)" }

        Log 'BAKE-OK'
    } catch {
        Log "BAKE-FAILED: $($_.Exception.Message) at $($_.InvocationInfo.PositionMessage)"
    } finally {
        if ($serial -and $serial.IsOpen) { $serial.Close() }
    }
} finally {
    Stop-Computer -Force
}
