// Package config loads and validates the controller's YAML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultWindowsImageURL is Microsoft's evaluation-center link for the
	// Windows Server 2025 evaluation VHDX (English, x64). It redirects to
	// a versioned file on software-static.download.prss.microsoft.com.
	DefaultWindowsImageURL = "https://go.microsoft.com/fwlink/?linkid=2345826"
	// DefaultVirtioWinURL pins a versioned https URL: the unversioned
	// stable-virtio alias redirects through plain http.
	DefaultVirtioWinURL = "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.302-1/virtio-win-0.1.302.iso"
)

// Duration wraps time.Duration to accept "5m"-style YAML strings.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	GitHub   GitHub  `yaml:"github"`
	StateDir string  `yaml:"state_dir"`
	Paths    Paths   `yaml:"paths"`
	Images   Images  `yaml:"images"`
	Docker   Docker  `yaml:"docker"`
	Windows  Windows `yaml:"windows"`
	Pools    []Pool  `yaml:"pools"`
}

// Paths overrides the default per-concern subdirectories under StateDir.
// Both fields default to <StateDir>/{images,run} when empty.
type Paths struct {
	// Images holds the baked base image (base.qcow2 + base.json), the
	// cloud image download, the bake working directory, and the docker
	// runner image provenance sidecar.
	Images string `yaml:"images"`
	// Run holds per-VM workdirs (overlay.qcow2, seed ISO, console log,
	// QMP socket, PID file) for QEMU pools, and the jit-config mount
	// staging directory for docker pools.
	Run string `yaml:"run"`
}

// Images configures base/runner image lifecycle.
type Images struct {
	// AutoRefresh bakes missing images on controller start instead of
	// failing. Pointer so an absent key defaults to true (a plain bool's
	// zero value is false, which would invert the intended default).
	AutoRefresh *bool `yaml:"auto_refresh"`
}

type GitHub struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	APIBaseURL     string `yaml:"api_base_url"`
}

// Docker configures the docker backend (pools with backend: docker).
type Docker struct {
	// Runtime is the container runtime for job containers: "runsc"
	// (gVisor, the default) or "runc" (no sandbox — see README warnings).
	Runtime string `yaml:"runtime"`
}

// Windows configures the base image for os: windows pools.
type Windows struct {
	ImageURL        string `yaml:"image_url"`
	ImageSHA256     string `yaml:"image_sha256"`
	VirtioWinURL    string `yaml:"virtio_win_url"`
	VirtioWinSHA256 string `yaml:"virtio_win_sha256"`
	// OVMFDir holds OVMF_CODE*.fd and OVMF_VARS*.fd. Empty means
	// auto-detect (see ResolveOVMF).
	OVMFDir string `yaml:"ovmf_dir"`
}

type Pool struct {
	Name    string `yaml:"name"`
	Backend string `yaml:"backend"`
	// OS selects the guest for qemu pools: "linux" (default) or
	// "windows". Docker pools are always linux.
	OS string `yaml:"os"`
	// Isolation selects the sandbox for docker pools: "gvisor" (default;
	// runsc + --privileged, full Docker-in-job) or "seccomp" (native runc,
	// no --privileged, Docker's default seccomp profile; no Docker inside
	// the job). Only valid on backend: docker pools.
	Isolation string `yaml:"isolation"`
	// SeccompProfile optionally replaces Docker's built-in default seccomp
	// profile with a custom one (absolute path on the host). Only valid
	// with Isolation "seccomp"; empty means the built-in default.
	SeccompProfile  string   `yaml:"seccomp_profile"`
	Scope           string   `yaml:"scope"`
	Org             string   `yaml:"org"`
	Repo            string   `yaml:"repo"`
	Count           int      `yaml:"count"`
	CPUs            int      `yaml:"cpus"`
	MemoryMB        int      `yaml:"memory_mb"`
	DiskGB          int      `yaml:"disk_gb"`
	Labels          []string `yaml:"labels"`
	RunnerGroup     string   `yaml:"runner_group"`
	LivenessTimeout Duration `yaml:"liveness_timeout"`
	DrainTimeout    Duration `yaml:"drain_timeout"`
}

// APIPrefix returns the REST path prefix for the pool's registration scope,
// e.g. "orgs/my-org" or "repos/owner/name".
func (p Pool) APIPrefix() string {
	if p.Scope == "repo" {
		return "repos/" + p.Repo
	}
	return "orgs/" + p.Org
}

// Pool names feed VM and runner names (ghq-<pool>-<id>); keep them short
// and DNS-ish.
var poolNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,19}$`)

var sha256Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.GitHub.APIBaseURL == "" {
		c.GitHub.APIBaseURL = "https://api.github.com"
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/github-qemu-runner"
	}
	c.Paths.Images = os.ExpandEnv(c.Paths.Images)
	c.Paths.Run = os.ExpandEnv(c.Paths.Run)
	if c.Paths.Images == "" {
		c.Paths.Images = filepath.Join(c.StateDir, "images")
	}
	if c.Paths.Run == "" {
		c.Paths.Run = filepath.Join(c.StateDir, "run")
	}
	if c.Images.AutoRefresh == nil {
		on := true
		c.Images.AutoRefresh = &on
	}
	// Lets the systemd unit pass the App key via LoadCredential:
	// private_key_path: ${CREDENTIALS_DIRECTORY}/app-key.pem
	c.GitHub.PrivateKeyPath = os.ExpandEnv(c.GitHub.PrivateKeyPath)
	if c.Docker.Runtime == "" {
		c.Docker.Runtime = "runsc"
	}
	if c.Windows.ImageURL == "" {
		c.Windows.ImageURL = DefaultWindowsImageURL
	}
	if c.Windows.VirtioWinURL == "" {
		c.Windows.VirtioWinURL = DefaultVirtioWinURL
	}
	c.Windows.OVMFDir = os.ExpandEnv(c.Windows.OVMFDir)
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Backend == "" {
			p.Backend = "qemu"
		}
		if p.OS == "" {
			p.OS = "linux"
		}
		if p.Backend == "docker" && p.Isolation == "" {
			p.Isolation = "gvisor"
		}
		if p.RunnerGroup == "" {
			p.RunnerGroup = "Default"
		}
		if p.LivenessTimeout == 0 {
			p.LivenessTimeout = Duration(5 * time.Minute)
		}
		if p.DrainTimeout == 0 {
			p.DrainTimeout = Duration(30 * time.Minute)
		}
	}
}

func (c *Config) validate() error {
	if c.GitHub.AppID <= 0 {
		return fmt.Errorf("github.app_id is required")
	}
	if c.GitHub.InstallationID <= 0 {
		return fmt.Errorf("github.installation_id is required")
	}
	if c.GitHub.PrivateKeyPath == "" {
		return fmt.Errorf("github.private_key_path is required")
	}
	if len(c.Pools) == 0 {
		return fmt.Errorf("at least one pool is required")
	}
	if c.Docker.Runtime != "runsc" && c.Docker.Runtime != "runc" {
		return fmt.Errorf(`docker.runtime must be "runsc" or "runc"`)
	}
	if !filepath.IsAbs(c.Paths.Images) {
		return fmt.Errorf("paths.images must be an absolute path")
	}
	if !filepath.IsAbs(c.Paths.Run) {
		return fmt.Errorf("paths.run must be an absolute path")
	}
	if c.Windows.OVMFDir != "" && !filepath.IsAbs(c.Windows.OVMFDir) {
		return fmt.Errorf("windows.ovmf_dir must be an absolute path")
	}
	for _, s := range []struct{ key, val string }{
		{"windows.image_sha256", c.Windows.ImageSHA256},
		{"windows.virtio_win_sha256", c.Windows.VirtioWinSHA256},
	} {
		if s.val != "" && !sha256Re.MatchString(s.val) {
			return fmt.Errorf("%s must be 64 hex characters", s.key)
		}
	}
	seen := map[string]bool{}
	for _, p := range c.Pools {
		if !poolNameRe.MatchString(p.Name) {
			return fmt.Errorf("pool name %q must match %s", p.Name, poolNameRe)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		seen[p.Name] = true
		if p.Backend != "qemu" && p.Backend != "docker" {
			return fmt.Errorf(`pool %s: backend must be "qemu" or "docker"`, p.Name)
		}
		if p.OS != "linux" && p.OS != "windows" {
			return fmt.Errorf(`pool %s: os must be "linux" or "windows"`, p.Name)
		}
		if p.OS == "windows" && p.Backend != "qemu" {
			return fmt.Errorf("pool %s: os: windows requires backend: qemu", p.Name)
		}
		if p.Backend == "docker" {
			if p.Isolation != "gvisor" && p.Isolation != "seccomp" {
				return fmt.Errorf(`pool %s: isolation must be "gvisor" or "seccomp"`, p.Name)
			}
		} else if p.Isolation != "" {
			return fmt.Errorf("pool %s: isolation is only valid on docker pools", p.Name)
		}
		if p.SeccompProfile != "" {
			if p.Isolation != "seccomp" {
				return fmt.Errorf("pool %s: seccomp_profile requires isolation: seccomp", p.Name)
			}
			if !filepath.IsAbs(p.SeccompProfile) {
				return fmt.Errorf("pool %s: seccomp_profile must be an absolute path", p.Name)
			}
		}
		switch p.Scope {
		case "org":
			if p.Org == "" {
				return fmt.Errorf("pool %s: org is required when scope is org", p.Name)
			}
		case "repo":
			parts := strings.Split(p.Repo, "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return fmt.Errorf("pool %s: repo must be owner/name", p.Name)
			}
			if p.RunnerGroup != "Default" {
				return fmt.Errorf("pool %s: repo-level runners only support the Default runner group", p.Name)
			}
		default:
			return fmt.Errorf(`pool %s: scope must be "org" or "repo"`, p.Name)
		}
		if p.Count < 1 {
			return fmt.Errorf("pool %s: count must be >= 1", p.Name)
		}
		if p.CPUs < 1 {
			return fmt.Errorf("pool %s: cpus must be >= 1", p.Name)
		}
		minMem := 256
		if p.OS == "windows" {
			minMem = 2048
		}
		if p.MemoryMB < minMem {
			return fmt.Errorf("pool %s: memory_mb must be >= %d", p.Name, minMem)
		}
		if p.DiskGB < 10 {
			return fmt.Errorf("pool %s: disk_gb must be >= 10", p.Name)
		}
		if len(p.Labels) == 0 {
			return fmt.Errorf("pool %s: at least one label is required", p.Name)
		}
		for _, l := range p.Labels {
			if strings.TrimSpace(l) == "" || strings.Contains(l, ",") || len(l) > 256 {
				return fmt.Errorf("pool %s: invalid label %q (must be non-empty, no commas, <= 256 chars)", p.Name, l)
			}
		}
	}
	return nil
}

// CapacityWarnings reports oversubscription against host resources. The
// caller decides how loudly to surface them; oversubscription is allowed.
func (c *Config) CapacityWarnings(hostCPUs, hostMemMB int) []string {
	var cpus, mem int
	for _, p := range c.Pools {
		cpus += p.Count * p.CPUs
		mem += p.Count * p.MemoryMB
	}
	var w []string
	// <= 0 means "host resources unknown" — never warn on unknowns.
	if hostCPUs > 0 && cpus > hostCPUs {
		w = append(w, fmt.Sprintf("pools may use %d vCPUs but host has %d", cpus, hostCPUs))
	}
	if hostMemMB > 0 && mem > hostMemMB {
		w = append(w, fmt.Sprintf("pools may use %d MiB RAM but host has %d MiB", mem, hostMemMB))
	}
	return w
}

// HasBackend reports whether any pool uses the given backend.
func (c *Config) HasBackend(b string) bool {
	for _, p := range c.Pools {
		if p.Backend == b {
			return true
		}
	}
	return false
}

// HasDockerIsolation reports whether any docker pool uses the given
// isolation mode ("gvisor" or "seccomp").
func (c *Config) HasDockerIsolation(mode string) bool {
	for _, p := range c.Pools {
		if p.Backend == "docker" && p.Isolation == mode {
			return true
		}
	}
	return false
}

// HasQEMUOS reports whether any qemu pool runs the given guest OS
// ("linux" or "windows").
func (c *Config) HasQEMUOS(os string) bool {
	for _, p := range c.Pools {
		if p.Backend == "qemu" && p.OS == os {
			return true
		}
	}
	return false
}
