package config

import (
	"os"
	"path/filepath"
	"slices"
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

func TestPathsDefaultToStateDirSubdirs(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Paths.Images, "/var/lib/github-qemu-runner/images"; got != want {
		t.Errorf("Paths.Images = %q, want %q", got, want)
	}
	if got, want := c.Paths.Run, "/var/lib/github-qemu-runner/run"; got != want {
		t.Errorf("Paths.Run = %q, want %q", got, want)
	}
}

func TestPathsOverride(t *testing.T) {
	y := validYAML + "paths:\n  images: /mnt/fast/images\n  run: /mnt/fast/run\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Paths.Images != "/mnt/fast/images" {
		t.Errorf("Paths.Images = %q", c.Paths.Images)
	}
	if c.Paths.Run != "/mnt/fast/run" {
		t.Errorf("Paths.Run = %q", c.Paths.Run)
	}
}

func TestPathsRespectStateDirOverride(t *testing.T) {
	y := validYAML + "state_dir: /srv/ghq\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Paths.Images, "/srv/ghq/images"; got != want {
		t.Errorf("Paths.Images = %q, want %q", got, want)
	}
	if got, want := c.Paths.Run, "/srv/ghq/run"; got != want {
		t.Errorf("Paths.Run = %q, want %q", got, want)
	}
}

func TestPathsEnvExpansion(t *testing.T) {
	t.Setenv("FAST_DISK", "/mnt/fast")
	y := validYAML + "paths:\n  images: ${FAST_DISK}/images\n  run: ${FAST_DISK}/run\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Paths.Images != "/mnt/fast/images" {
		t.Errorf("Paths.Images = %q", c.Paths.Images)
	}
	if c.Paths.Run != "/mnt/fast/run" {
		t.Errorf("Paths.Run = %q", c.Paths.Run)
	}
}

func TestPathsRelativeRejected(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{
			"relative images",
			validYAML + "paths:\n  images: rel/images\n",
			"paths.images must be an absolute path",
		},
		{
			"relative run",
			validYAML + "paths:\n  run: rel/run\n",
			"paths.run must be an absolute path",
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

func TestAutoRefreshDefaultsTrue(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Images.AutoRefresh == nil || !*c.Images.AutoRefresh {
		t.Errorf("Images.AutoRefresh = %v, want non-nil true", c.Images.AutoRefresh)
	}
}

func TestAutoRefreshExplicitFalse(t *testing.T) {
	y := validYAML + "images:\n  auto_refresh: false\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Images.AutoRefresh == nil || *c.Images.AutoRefresh {
		t.Errorf("Images.AutoRefresh = %v, want non-nil false", c.Images.AutoRefresh)
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

const windowsPoolYAML = `
github:
  app_id: 1
  installation_id: 2
  private_key_path: /tmp/key.pem
pools:
  - name: win
    os: windows
    scope: org
    org: my-org
    count: 1
    cpus: 4
    memory_mb: 8192
    disk_gb: 80
    labels: [self-hosted, windows, x64]
`

func TestWindowsPoolDefaults(t *testing.T) {
	c, err := Load(writeConfig(t, windowsPoolYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pools[0].OS != "windows" || c.Pools[0].Backend != "qemu" {
		t.Errorf("pool = %+v", c.Pools[0])
	}
	if c.Windows.Image != DefaultWindowsImage {
		t.Errorf("Image = %q", c.Windows.Image)
	}
	if c.Windows.VirtioWin != DefaultVirtioWin {
		t.Errorf("VirtioWin = %q", c.Windows.VirtioWin)
	}
	if !c.HasQEMUOS("windows") || c.HasQEMUOS("linux") {
		t.Error("HasQEMUOS wrong")
	}
}

func TestLinuxPoolOSDefault(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pools[0].OS != "linux" || !c.HasQEMUOS("linux") || c.HasQEMUOS("windows") {
		t.Errorf("OS = %q", c.Pools[0].OS)
	}
}

func TestWindowsPoolValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"bad os", func(y string) string { return strings.Replace(y, "os: windows", "os: bsd", 1) }, `os must be "linux" or "windows"`},
		{"docker backend", func(y string) string { return strings.Replace(y, "os: windows", "os: windows\n    backend: docker", 1) }, "os: windows requires backend: qemu"},
		{"low memory", func(y string) string { return strings.Replace(y, "memory_mb: 8192", "memory_mb: 1024", 1) }, "memory_mb must be >= 2048"},
		{"relative ovmf", func(y string) string { return y + "windows:\n  ovmf_dir: share/ovmf\n" }, "windows.ovmf_dir must be an absolute path"},
		{"bad sha", func(y string) string { return y + "windows:\n  image_sha256: abc\n" }, "windows.image_sha256 must be 64 hex characters"},
		{"bad virtio sha", func(y string) string { return y + "windows:\n  virtio_win_sha256: xyz\n" }, "windows.virtio_win_sha256 must be 64 hex characters"},
		{"relative image", func(y string) string { return y + "windows:\n  image: srv/win.vhdx\n" }, "windows.image must be an http(s) URL or an absolute path"},
		{"file:// image", func(y string) string { return y + "windows:\n  image: file:///srv/win.vhdx\n" }, "windows.image: use a plain absolute path, not a file:// URL"},
		{"relative virtio", func(y string) string { return y + "windows:\n  virtio_win: virtio-win.iso\n" }, "windows.virtio_win must be an http(s) URL or an absolute path"},
		{"file:// virtio", func(y string) string { return y + "windows:\n  virtio_win: file:///srv/virtio-win.iso\n" }, "windows.virtio_win: use a plain absolute path, not a file:// URL"},
		{"bad package id", func(y string) string { return y + "windows:\n  packages: [\"jq; rm -rf\"]\n" }, `windows.packages: "jq; rm -rf" is not a WinGet package ID`},
		{"leading dot package", func(y string) string { return y + "windows:\n  packages: [.foo]\n" }, `windows.packages: ".foo" is not a WinGet package ID`},
		{"duplicate package", func(y string) string { return y + "windows:\n  packages: [GitHub.cli, github.cli]\n" }, `windows.packages: "github.cli" is listed twice`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.mutate(windowsPoolYAML)))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestWindowsBlockOverrides(t *testing.T) {
	y := windowsPoolYAML + "windows:\n  image: https://example.com/w.vhdx\n  image_sha256: " +
		strings.Repeat("a", 64) + "\n  ovmf_dir: /usr/share/edk2/x64\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Windows.Image != "https://example.com/w.vhdx" || c.Windows.OVMFDir != "/usr/share/edk2/x64" {
		t.Errorf("Windows = %+v", c.Windows)
	}
}

// A local file is a first-class source for both keys: absolute paths are
// accepted and kept verbatim (the bake uses them in place).
func TestWindowsLocalSources(t *testing.T) {
	y := windowsPoolYAML + "windows:\n  image: /srv/win.vhdx\n  virtio_win: /srv/virtio-win.iso\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Windows.Image != "/srv/win.vhdx" || c.Windows.VirtioWin != "/srv/virtio-win.iso" {
		t.Errorf("Windows = %+v", c.Windows)
	}
	if !IsLocalSource(c.Windows.Image) || !IsLocalSource(c.Windows.VirtioWin) {
		t.Error("absolute paths must be local sources")
	}
}

func TestWindowsSourcesExpandEnv(t *testing.T) {
	t.Setenv("GHQ_TEST_IMAGES", "/srv/images")
	y := windowsPoolYAML + "windows:\n  image: ${GHQ_TEST_IMAGES}/win.vhdx\n  virtio_win: ${GHQ_TEST_IMAGES}/virtio-win.iso\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Windows.Image != "/srv/images/win.vhdx" || c.Windows.VirtioWin != "/srv/images/virtio-win.iso" {
		t.Errorf("Windows = %+v", c.Windows)
	}
}

func TestIsLocalSource(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"/srv/win.vhdx", true},
		{"https://example.com/w.vhdx", false},
		{"http://example.com/w.vhdx", false},
		{"srv/win.vhdx", false},
		{"", false},
	} {
		if got := IsLocalSource(tc.in); got != tc.want {
			t.Errorf("IsLocalSource(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCheckLocalSource(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "win.vhdx")
	if err := os.WriteFile(file, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	// t.TempDir() may itself sit in a namespace the units replace
	// (TMPDIR=/tmp), so compare against the lexical rule, not "".
	warning, err := CheckLocalSource(file)
	if err != nil || warning != hiddenNamespaceWarning(file) {
		t.Errorf("CheckLocalSource(file) = %q, %v", warning, err)
	}
	if _, err := CheckLocalSource(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: err = %v, want not a regular file", err)
	}
	if _, err := CheckLocalSource(filepath.Join(dir, "missing.vhdx")); err == nil {
		t.Error("missing file must be an error")
	}
	// ProtectHome=yes hides /home from the service; warn, don't fail:
	// `setup` and the controller may well be able to read the file.
	warning, err = CheckLocalSource("/home/nonexistent-user/win.vhdx")
	if err == nil {
		t.Skip("path under /home unexpectedly exists")
	}
	if warning != "" {
		t.Errorf("warning = %q, want none when the stat fails", warning)
	}
}

// The warning half of CheckLocalSource is lexical, so it can be checked
// against paths that need not exist.
func TestHiddenNamespaceWarning(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/srv/images/win.vhdx", ""},
		{"/var/lib/github-qemu-runner/images/win.vhdx", ""},
		{"/homer/win.vhdx", ""}, // not /home/
		{"/home/op/win.vhdx", "ProtectHome=yes"},
		{"/root/win.vhdx", "ProtectHome=yes"},
		{"/run/user/1000/win.vhdx", "ProtectHome=yes"},
		{"/tmp/win.vhdx", "PrivateTmp=yes"},
		{"/var/tmp/win.vhdx", "PrivateTmp=yes"},
		// Cleaned before matching, both ways round.
		{"/tmp//sub/../win.vhdx", "PrivateTmp=yes"},
		{"/home/../srv/win.vhdx", ""},
	} {
		got := hiddenNamespaceWarning(tc.path)
		if tc.want == "" {
			if got != "" {
				t.Errorf("hiddenNamespaceWarning(%q) = %q, want none", tc.path, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) || !strings.Contains(got, tc.path) {
			t.Errorf("hiddenNamespaceWarning(%q) = %q, want one naming the path and %s", tc.path, got, tc.want)
		}
	}
}

func TestCheckLocalSourceWarnsUnderHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || !strings.HasPrefix(home, "/home/") {
		t.Skipf("home directory %q is not under /home: %v", home, err)
	}
	dir, err := os.MkdirTemp(home, "ghq-test-")
	if err != nil {
		t.Skipf("cannot create a directory under %s: %v", home, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "win.vhdx")
	if err := os.WriteFile(file, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	warning, err := CheckLocalSource(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning, "ProtectHome=yes") {
		t.Errorf("warning = %q, want a ProtectHome warning", warning)
	}
}

func TestWindowsSHA256Lowercased(t *testing.T) {
	// Microsoft publishes evaluation-media digests in uppercase; the
	// verifier compares against lowercase hex, so Load must fold case.
	upper := strings.Repeat("DEADBEEF", 8)
	mixed := strings.Repeat("cAfE", 16)
	y := windowsPoolYAML + "windows:\n  image_sha256: " + upper +
		"\n  virtio_win_sha256: " + mixed + "\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Windows.ImageSHA256 != strings.ToLower(upper) {
		t.Errorf("ImageSHA256 = %q, want %q", c.Windows.ImageSHA256, strings.ToLower(upper))
	}
	if c.Windows.VirtioWinSHA256 != strings.ToLower(mixed) {
		t.Errorf("VirtioWinSHA256 = %q, want %q", c.Windows.VirtioWinSHA256, strings.ToLower(mixed))
	}
}

func TestWindowsParityDefaults(t *testing.T) {
	c, err := Load(writeConfig(t, windowsPoolYAML))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Microsoft.PowerShell", "GitHub.cli", "jqlang.jq", "7zip.7zip"}
	if !slices.Equal(c.Windows.Packages, want) {
		t.Errorf("Packages = %v, want %v", c.Windows.Packages, want)
	}
	if c.Windows.BuildTools == nil || !*c.Windows.BuildTools {
		t.Errorf("BuildTools = %v, want true", c.Windows.BuildTools)
	}
	if c.Windows.DisableDefender == nil || !*c.Windows.DisableDefender {
		t.Errorf("DisableDefender = %v, want true", c.Windows.DisableDefender)
	}
	// The default list must be a copy: mutating one config's list must not
	// leak into the package-level default.
	c.Windows.Packages[0] = "Changed"
	if DefaultWindowsPackages[0] != "Microsoft.PowerShell" {
		t.Error("DefaultWindowsPackages aliased into the loaded config")
	}
}

func TestWindowsParityOverrides(t *testing.T) {
	y := windowsPoolYAML + "windows:\n  packages: [Microsoft.PowerShell, Kitware.CMake]\n  build_tools: false\n  disable_defender: false\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Windows.Packages, []string{"Microsoft.PowerShell", "Kitware.CMake"}) {
		t.Errorf("Packages = %v", c.Windows.Packages)
	}
	if *c.Windows.BuildTools || *c.Windows.DisableDefender {
		t.Errorf("BuildTools=%v DisableDefender=%v, want both false", *c.Windows.BuildTools, *c.Windows.DisableDefender)
	}
}

func TestWindowsPackagesEmptyAndNull(t *testing.T) {
	c, err := Load(writeConfig(t, windowsPoolYAML+"windows:\n  packages: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Windows.Packages) != 0 {
		t.Errorf("packages: [] must install nothing, got %v", c.Windows.Packages)
	}
	c, err = Load(writeConfig(t, windowsPoolYAML+"windows:\n  packages:\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Windows.Packages, DefaultWindowsPackages) {
		t.Errorf("packages: (null) must mean the default list, got %v", c.Windows.Packages)
	}
}
