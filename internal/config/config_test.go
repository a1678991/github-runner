package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const validYAML = `
github:
  app_id: 1
  installation_id: 2
  private_key_path: /tmp/key.pem
pools:
  - name: fmt
    scope: org
    org: my-org
    count: 2
    cpus: 2
    memory_mb: 2048
    disk_gb: 20
    labels: [self-hosted, linux, x64, fmt]
`

func TestLoadValidAppliesDefaults(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHub.APIBaseURL != "https://api.github.com" {
		t.Errorf("APIBaseURL = %q", c.GitHub.APIBaseURL)
	}
	if c.StateDir != "/var/lib/github-qemu-runner" {
		t.Errorf("StateDir = %q", c.StateDir)
	}
	p := c.Pools[0]
	if p.RunnerGroup != "Default" {
		t.Errorf("RunnerGroup = %q", p.RunnerGroup)
	}
	if time.Duration(p.LivenessTimeout) != 5*time.Minute {
		t.Errorf("LivenessTimeout = %v", p.LivenessTimeout)
	}
	if time.Duration(p.DrainTimeout) != 30*time.Minute {
		t.Errorf("DrainTimeout = %v", p.DrainTimeout)
	}
	if got := p.APIPrefix(); got != "orgs/my-org" {
		t.Errorf("APIPrefix = %q", got)
	}
}

func TestRepoScopeAPIPrefix(t *testing.T) {
	y := strings.NewReplacer(
		"scope: org", "scope: repo",
		"org: my-org", `repo: owner/name`,
	).Replace(validYAML)
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Pools[0].APIPrefix(); got != "repos/owner/name" {
		t.Errorf("APIPrefix = %q", got)
	}
}

func TestDurationOverride(t *testing.T) {
	y := validYAML + "    liveness_timeout: 90s\n    drain_timeout: 1h\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(c.Pools[0].LivenessTimeout) != 90*time.Second {
		t.Errorf("LivenessTimeout = %v", c.Pools[0].LivenessTimeout)
	}
	if time.Duration(c.Pools[0].DrainTimeout) != time.Hour {
		t.Errorf("DrainTimeout = %v", c.Pools[0].DrainTimeout)
	}
}

func TestEnvExpansionInKeyPath(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "/cred")
	y := strings.Replace(validYAML,
		"private_key_path: /tmp/key.pem",
		"private_key_path: ${CREDENTIALS_DIRECTORY}/app-key.pem", 1)
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHub.PrivateKeyPath != "/cred/app-key.pem" {
		t.Errorf("PrivateKeyPath = %q", c.GitHub.PrivateKeyPath)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"no pools", `
github: {app_id: 1, installation_id: 2, private_key_path: /k}
pools: []
`, "at least one pool"},
		{
			"bad scope", strings.Replace(validYAML, "scope: org", "scope: enterprise", 1),
			`scope must be "org" or "repo"`,
		},
		{"repo without owner/name", strings.NewReplacer(
			"scope: org", "scope: repo", "org: my-org", "repo: justname",
		).Replace(validYAML), "owner/name"},
		{
			"bad pool name", strings.Replace(validYAML, "name: fmt", "name: FMT_pool", 1),
			"pool name",
		},
		{
			"duplicate pool name", validYAML + strings.TrimPrefix(strings.ReplaceAll(validYAML, "github:", "ignore:"), "\n"),
			"",
		}, // replaced below — see note
		{"zero count", strings.Replace(validYAML, "count: 2", "count: 0", 1), "count"},
		{"no labels", strings.Replace(validYAML, "labels: [self-hosted, linux, x64, fmt]", "labels: []", 1), "label"},
		{"unknown field", strings.Replace(validYAML, "pools:", "poolz:", 1), "poolz"},
		{"missing app_id", strings.Replace(validYAML, "app_id: 1", "app_id: 0", 1), "app_id"},
	}
	for _, tc := range cases {
		if tc.name == "duplicate pool name" {
			continue // covered by TestDuplicatePoolName
		}
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDuplicatePoolName(t *testing.T) {
	y := validYAML + `
  - name: fmt
    scope: org
    org: my-org
    count: 1
    cpus: 1
    memory_mb: 512
    disk_gb: 10
    labels: [x]
`
	_, err := Load(writeConfig(t, y))
	if err == nil || !strings.Contains(err.Error(), "duplicate pool name") {
		t.Fatalf("want duplicate pool name error, got %v", err)
	}
}

func TestCapacityWarnings(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if w := c.CapacityWarnings(32, 64000); len(w) != 0 {
		t.Errorf("unexpected warnings: %v", w)
	}
	w := c.CapacityWarnings(2, 1024) // pools want 2*2=4 vCPU, 4096 MiB
	if len(w) != 2 {
		t.Errorf("want 2 warnings, got %v", w)
	}
	// <= 0 means "unknown host resources": never warn
	if w := c.CapacityWarnings(0, 0); len(w) != 0 {
		t.Errorf("unknown host resources must not warn: %v", w)
	}
}

func TestLabelValidation(t *testing.T) {
	cases := []struct{ name, label string }{
		{"empty label", `""`},
		{"whitespace label", `" "`},
		{"comma in label", `"a,b"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := strings.Replace(validYAML,
				"labels: [self-hosted, linux, x64, fmt]",
				"labels: [self-hosted, "+tc.label+"]", 1)
			_, err := Load(writeConfig(t, y))
			if err == nil || !strings.Contains(err.Error(), "invalid label") {
				t.Fatalf("want invalid label error, got %v", err)
			}
		})
	}
	// long label
	y := strings.Replace(validYAML,
		"labels: [self-hosted, linux, x64, fmt]",
		`labels: ["`+strings.Repeat("x", 257)+`"]`, 1)
	if _, err := Load(writeConfig(t, y)); err == nil || !strings.Contains(err.Error(), "invalid label") {
		t.Fatalf("want invalid label error for long label, got %v", err)
	}
}

func TestRepoScopeRejectsCustomGroup(t *testing.T) {
	y := strings.NewReplacer(
		"scope: org", "scope: repo",
		"org: my-org", "repo: owner/name",
	).Replace(validYAML) + "    runner_group: custom\n"
	_, err := Load(writeConfig(t, y))
	if err == nil || !strings.Contains(err.Error(), "Default runner group") {
		t.Fatalf("want Default-group error, got %v", err)
	}
}

const dockerPoolYAML = `
github:
  app_id: 1
  installation_id: 2
  private_key_path: /tmp/key.pem
pools:
  - name: oci
    backend: docker
    scope: org
    org: my-org
    count: 1
    cpus: 2
    memory_mb: 2048
    disk_gb: 20
    labels: [self-hosted, linux, arm64]
`

func TestBackendDefaultsToQEMU(t *testing.T) {
	c, err := Load(writeConfig(t, strings.Replace(dockerPoolYAML, "    backend: docker\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Pools[0].Backend; got != "qemu" {
		t.Errorf("default backend = %q, want qemu", got)
	}
	if c.HasBackend("docker") {
		t.Error("HasBackend(docker) = true for qemu-only config")
	}
	if !c.HasBackend("qemu") {
		t.Error("HasBackend(qemu) = false for qemu-only config")
	}
}

func TestDockerBackendAndRuntime(t *testing.T) {
	c, err := Load(writeConfig(t, dockerPoolYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Pools[0].Backend; got != "docker" {
		t.Errorf("backend = %q, want docker", got)
	}
	if got := c.Docker.Runtime; got != "runsc" {
		t.Errorf("default docker.runtime = %q, want runsc", got)
	}
	if !c.HasBackend("docker") || c.HasBackend("qemu") {
		t.Error("HasBackend wrong for docker-only config")
	}
}

func TestDockerRuntimeRuncAccepted(t *testing.T) {
	c, err := Load(writeConfig(t, "docker:\n  runtime: runc\n"+dockerPoolYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Docker.Runtime; got != "runc" {
		t.Errorf("docker.runtime = %q, want runc", got)
	}
}

func TestInvalidBackendRejected(t *testing.T) {
	_, err := Load(writeConfig(t, strings.Replace(dockerPoolYAML, "backend: docker", "backend: podman", 1)))
	if err == nil || !strings.Contains(err.Error(), "backend") {
		t.Errorf("want backend validation error, got %v", err)
	}
}

func TestInvalidDockerRuntimeRejected(t *testing.T) {
	_, err := Load(writeConfig(t, "docker:\n  runtime: kata\n"+dockerPoolYAML))
	if err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Errorf("want runtime validation error, got %v", err)
	}
}

const seccompPoolYAML = dockerPoolYAML + `  - name: fast
    backend: docker
    isolation: seccomp
    scope: org
    org: my-org
    count: 1
    cpus: 4
    memory_mb: 4096
    disk_gb: 20
    labels: [self-hosted, linux, fast]
`

func TestIsolationDefaultsToGvisor(t *testing.T) {
	c, err := Load(writeConfig(t, dockerPoolYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Pools[0].Isolation; got != "gvisor" {
		t.Errorf("default isolation = %q, want gvisor", got)
	}
	if !c.HasDockerIsolation("gvisor") || c.HasDockerIsolation("seccomp") {
		t.Error("HasDockerIsolation wrong for gvisor-only config")
	}
}

func TestQEMUPoolHasNoIsolation(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Pools[0].Isolation; got != "" {
		t.Errorf("qemu pool isolation = %q, want empty", got)
	}
	if c.HasDockerIsolation("gvisor") || c.HasDockerIsolation("seccomp") {
		t.Error("HasDockerIsolation must be false for qemu-only config")
	}
}

func TestSeccompIsolationAccepted(t *testing.T) {
	c, err := Load(writeConfig(t, seccompPoolYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Pools[1].Isolation; got != "seccomp" {
		t.Errorf("isolation = %q, want seccomp", got)
	}
	if !c.HasDockerIsolation("gvisor") || !c.HasDockerIsolation("seccomp") {
		t.Error("HasDockerIsolation must report both modes for mixed config")
	}
}

func TestSeccompProfileAccepted(t *testing.T) {
	y := strings.Replace(seccompPoolYAML, "    isolation: seccomp\n",
		"    isolation: seccomp\n    seccomp_profile: /etc/ghq/strict.json\n", 1)
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Pools[1].SeccompProfile; got != "/etc/ghq/strict.json" {
		t.Errorf("seccomp_profile = %q", got)
	}
}

func TestIsolationValidationErrors(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{
			"bad isolation value",
			strings.Replace(seccompPoolYAML, "isolation: seccomp", "isolation: firecracker", 1),
			`isolation must be "gvisor" or "seccomp"`,
		},
		{
			"isolation on qemu pool",
			validYAML + "    isolation: seccomp\n",
			"only valid on docker pools",
		},
		{
			"seccomp_profile without seccomp isolation",
			strings.Replace(dockerPoolYAML, "    backend: docker\n",
				"    backend: docker\n    seccomp_profile: /etc/ghq/p.json\n", 1),
			"requires isolation: seccomp",
		},
		{
			"relative seccomp_profile",
			strings.Replace(seccompPoolYAML, "    isolation: seccomp\n",
				"    isolation: seccomp\n    seccomp_profile: rel/p.json\n", 1),
			"absolute path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
