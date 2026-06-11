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
}

func TestDockerAssetsEmbedded(t *testing.T) {
	if !strings.Contains(Dockerfile, "FROM ubuntu:24.04") {
		t.Error("Dockerfile missing or wrong base image")
	}
	if !strings.Contains(DockerEntrypoint, "--iptables=false") {
		t.Error("entrypoint must disable inner dockerd iptables (gVisor has no netfilter)")
	}
}
