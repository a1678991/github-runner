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
	for _, call := range []string{"pnputil.exe /add-driver", "& $gitExe --version"} {
		i := strings.Index(WindowsBake, call)
		if i < 0 {
			t.Errorf("bake.ps1 missing %q", call)
			continue
		}
		if j := strings.LastIndex(WindowsBake[:i], "$ErrorActionPreference = 'Continue'"); j < 0 || i-j > 300 {
			t.Errorf("bake.ps1: %q must run with $ErrorActionPreference relaxed to 'Continue' so its exit code stays reachable", call)
		}
	}
}
