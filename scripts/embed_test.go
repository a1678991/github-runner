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
