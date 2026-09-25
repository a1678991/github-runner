package scripts

import (
	"strings"
	"testing"
)

func TestEmbeddedScripts(t *testing.T) {
	if !strings.Contains(RunOneJob, "--jitconfig") || !strings.Contains(RunOneJob, "trap 'poweroff' EXIT") {
		t.Error("RunOneJob missing expected content")
	}
	if !strings.Contains(Bake, "BAKE-OK") || !strings.Contains(Bake, "cloud-init clean") {
		t.Error("Bake missing expected content")
	}
	if !strings.Contains(Bake, "/etc/sudoers.d/runner") {
		t.Error("Bake must grant runner passwordless sudo (parity with GitHub-hosted images; jobs run `sudo apt-get ...`)")
	}
	if !strings.Contains(Bake, "apt-get purge -y unattended-upgrades") ||
		!strings.Contains(Bake, "systemctl mask --now apt-daily.timer apt-daily-upgrade.timer") {
		t.Error("Bake must purge unattended-upgrades and mask the apt-daily timers (they grab the apt/dpkg lock at boot in cloned job VMs and break jobs running `apt-get`)")
	}
	if !strings.Contains(Bake, "apt-get dist-upgrade") {
		t.Error("Bake must dist-upgrade during bake (guest OS updates land via image refresh, never in job VMs)")
	}
}

func TestDockerAssetsEmbedded(t *testing.T) {
	if !strings.Contains(Dockerfile, "FROM ubuntu:24.04") {
		t.Error("Dockerfile missing or wrong base image")
	}
	if !strings.Contains(Dockerfile, "--uid 1001") {
		t.Error("Dockerfile must create runner with uid 1001 (avoids collision with ubuntu:24.04's uid-1000 user)")
	}
	if !strings.Contains(Dockerfile, "/etc/sudoers.d/runner") {
		t.Error("Dockerfile must grant runner passwordless sudo (ubuntu:24.04 ships no sudo; jobs run `sudo apt-get ...` as on GitHub-hosted runners)")
	}
	if !strings.Contains(Dockerfile, "VOLUME /var/lib/docker") {
		t.Error("Dockerfile must declare anonymous VOLUME /var/lib/docker so inner-docker state never lands on the container's overlay")
	}
	if !strings.Contains(DockerEntrypoint, "--iptables=false") {
		t.Error("entrypoint must disable inner dockerd iptables (gVisor has no netfilter)")
	}
	if !strings.Contains(DockerEntrypoint, "runuser -u runner") {
		t.Error("entrypoint must drop privileges via runuser -u runner before exec'ing run.sh (PID 1 is root for dockerd; the job must not be)")
	}
	if !strings.Contains(Dockerfile, "FROM ubuntu:24.04 AS base") ||
		!strings.Contains(Dockerfile, "FROM base AS dind") ||
		!strings.Contains(Dockerfile, "FROM base AS slim") {
		t.Error("Dockerfile must define dind and slim build stages sharing a common base stage")
	}
	if !strings.Contains(DockerEntrypointSlim, "runuser -u runner") {
		t.Error("slim entrypoint must drop privileges via runuser -u runner before exec'ing run.sh")
	}
	if !strings.Contains(DockerEntrypointSlim, "--jitconfig") {
		t.Error("slim entrypoint must pass the JIT config to run.sh")
	}
	if strings.Contains(DockerEntrypointSlim, "dockerd") {
		t.Error("slim entrypoint must not start dockerd (seccomp pools run without --privileged; DinD is gvisor-pool-only)")
	}
}

func TestWindowsAssetsEmbedded(t *testing.T) {
	for name, s := range map[string]string{"Unattend": WindowsUnattend, "Bake": WindowsBake, "RunOneJob": WindowsRunOneJob} {
		if strings.TrimSpace(s) == "" {
			t.Errorf("%s is empty", name)
		}
		if strings.Contains(s, "\r\n") {
			t.Errorf("%s has CRLF line endings; keep LF (PowerShell 5.1 and Setup accept LF)", name)
		}
	}
	for _, want := range []string{"{{.AdminPassword}}", `pass="specialize"`, `pass="oobeSystem"`, "<HideEULAPage>true</HideEULAPage>", "GHQSEED", "bake.ps1"} {
		if !strings.Contains(WindowsUnattend, want) {
			t.Errorf("Unattend.xml missing %q", want)
		}
	}
	for _, want := range []string{"BAKE-OK", "BAKE-FAILED", "pnputil", "viostor", "DisablePrivacyExperience", "wuauserv", "ghq-run-one-job", "bake-env.json", "Stop-Computer -Force", "RealTimeIsUniversal"} {
		if !strings.Contains(WindowsBake, want) {
			t.Errorf("bake.ps1 missing %q", want)
		}
	}
	for _, want := range []string{"runner-jit.conf", "--jitconfig", "Resize-Partition", "Stop-Computer -Force", "GHQSEED"} {
		if !strings.Contains(WindowsRunOneJob, want) {
			t.Errorf("run-one-job.ps1 missing %q", want)
		}
	}
	// Host-side teardown waits on the qemu process, so the power-off must be
	// unconditional: the serial port is constructed inside the try whose
	// finally runs Stop-Computer -Force, never before it.
	for name, s := range map[string]string{"bake.ps1": WindowsBake, "run-one-job.ps1": WindowsRunOneJob} {
		serial := strings.Index(s, "New-Object System.IO.Ports.SerialPort")
		outerTry := strings.Index(s, "\ntry {\n")
		stop := strings.Index(s, "Stop-Computer -Force")
		if serial < 0 || outerTry < 0 || stop < 0 {
			t.Errorf("%s: missing serial setup, top-level try, or Stop-Computer -Force", name)
			continue
		}
		if outerTry > serial || stop < serial {
			t.Errorf("%s: serial setup (offset %d) must sit inside the top-level try (offset %d) whose finally powers off (offset %d); outside it a COM1 failure skips the power-off and strands the VM", name, serial, outerTry, stop)
		}
	}
	// A native command's stderr merged by 2>&1 arrives as ErrorRecords, which
	// $ErrorActionPreference = 'Stop' escalates to a terminating
	// NativeCommandError before any $LASTEXITCODE check can run.
	for _, call := range []string{"pnputil.exe /add-driver", "& $gitExe --version", "& $exe @argv", "& taskkill.exe"} {
		i := strings.Index(WindowsBake, call)
		if i < 0 {
			t.Errorf("bake.ps1 missing %q", call)
			continue
		}
		if j := strings.LastIndex(WindowsBake[:i], "$ErrorActionPreference = 'Continue'"); j < 0 || i-j > 300 {
			t.Errorf("bake.ps1: %q must run with $ErrorActionPreference relaxed to 'Continue' so its exit code stays reachable", call)
		}
	}
	// Hosted-image parity (docs/superpowers/specs/2026-09-26-windows-hosted-parity-design.md).
	for _, want := range []string{
		// config reaches the guest
		"$env_.packages", "$env_.build_tools", "$env_.disable_defender",
		// Git configured like the hosted image
		"/COMPONENTS=gitlfs", "/o:EnableSymlinks=Enabled", "/o:BashTerminalOption=ConHost",
		// Git\bin prepended, as runner-images' Add-MachinePathItem does, so
		// `shell: bash` finds Git's MINGW64 launcher before Git\usr\bin's
		// bare bash.exe that PathOption=CmdTools appends.
		"Add-MachinePath 'C:\\Program Files\\Git\\bin' -Prepend", "safe.directory", "GCM_INTERACTIVE",
		// system tuning
		"NoAutoUpdate", "DisableWindowsUpdateAccess", "WaaSMedicSvc", "AllowTelemetry",
		"MaintenanceDisabled", "ConsentPromptBehaviorAdmin", "SysMain", "ServicesPipeTimeout",
		// Defender
		"Set-MpPreference", "DisableRealtimeMonitoring", "DisableBehaviorMonitoring",
		"DisableIOAVProtection", "ExclusionPath", "RealTimeProtectionEnabled",
		// packages: machine scope with a retry on "no applicable installer"
		"'--scope', 'machine'", "-1978335216", "Assert-WinGetResult",
		// Build Tools + SDK
		"Microsoft.VisualStudio.Component.Windows11SDK.26100",
		// toolchain assertion and summary
		"signtool.exe", "Log \"toolchain: ",
		// the check fails unless bash resolves to Git's launcher
		"'C:\\Program Files\\Git\\bin\\bash.exe'",
		// the SDK the Build Tools override adds is the one reported
		"\\bin\\10.0.26100.*\\x64\\signtool.exe",
		// empty or null package entries never reach winget
		"$env_.packages | Where-Object { $_ }",
		// a function that returns a value must not leak WaitForExit's bool
		"$null = $p.WaitForExit()",
		// WinGet prefers MSI/EXE installers over MSIX, as the hosted image
		// installs them (an MSIX pwsh is a per-user alias, off the machine PATH)
		"installerTypes", "Microsoft.DesktopAppInstaller_8wekyb3d8bbwe\\LocalState\\settings.json",
	} {
		if !strings.Contains(WindowsBake, want) {
			t.Errorf("bake.ps1 missing %q", want)
		}
	}
	// The toolchain check sees what the `runner` user's logon will: the
	// machine PATH only, never this Administrator's user PATH.
	if strings.Contains(WindowsBake, "GetEnvironmentVariable('Path', 'User')") {
		t.Error("bake.ps1: the toolchain check must not read the Administrator's user PATH")
	}
	// gh and jq come from the configurable package list, not hardcoded installs.
	for _, gone := range []string{"Install-WinGetPackage 'GitHub.cli'", "Install-WinGetPackage 'jqlang.jq'"} {
		if strings.Contains(WindowsBake, gone) {
			t.Errorf("bake.ps1 still hardcodes %q; it belongs to windows.packages", gone)
		}
	}
	// Tool checks for packaged commands are keyed off the configured list,
	// so `packages: []` does not demand pwsh/gh/jq/7z.
	if !strings.Contains(WindowsBake, "foreach ($id in $packages) { if ($pkgCommands.ContainsKey($id))") {
		t.Error("bake.ps1: packaged-command checks must be derived from $packages")
	}
	// Both Defender blocks (apply, then assert) are gated on the option.
	if n := strings.Count(WindowsBake, "if ($env_.disable_defender)"); n < 2 {
		t.Errorf("bake.ps1: want the Defender apply and check both gated on $env_.disable_defender, found %d", n)
	}
	// Build Tools install and its checks are gated on the option.
	if n := strings.Count(WindowsBake, "if ($env_.build_tools)"); n < 2 {
		t.Errorf("bake.ps1: want the Build Tools install and check both gated on $env_.build_tools, found %d", n)
	}
	// Order: Defender off before the first WinGet install (it speeds the
	// installs up); the toolchain summary right before the sentinel.
	defender := strings.Index(WindowsBake, "Set-MpPreference @pref")
	firstInstall := strings.Index(WindowsBake, `Install-WinGetPackage "Microsoft.VCRedist.2015+.$arch"`)
	summary := strings.Index(WindowsBake, "Log \"toolchain: ")
	ok := strings.Index(WindowsBake, "Log 'BAKE-OK'")
	if defender < 0 || firstInstall < 0 || defender > firstInstall {
		t.Errorf("bake.ps1: Defender preferences (offset %d) must be applied before the first WinGet install (offset %d)", defender, firstInstall)
	}
	// The installer-type preference is in place before the first install.
	wgSettings := strings.Index(WindowsBake, "LocalState\\settings.json")
	if wgSettings < 0 || firstInstall < 0 || wgSettings > firstInstall {
		t.Errorf("bake.ps1: WinGet settings (offset %d) must be written before the first WinGet install (offset %d)", wgSettings, firstInstall)
	}
	if summary < 0 || ok < 0 || summary > ok {
		t.Errorf("bake.ps1: the toolchain summary (offset %d) must be logged before BAKE-OK (offset %d)", summary, ok)
	}
}
