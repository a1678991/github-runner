# Windows Job VMs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `backend: qemu` pools gain `os: windows`, backed by a Windows Server evaluation image baked under OVMF with virtio drivers, running one job per disposable VM exactly like Linux pools.

**Architecture:** A second baked image `base-windows.qcow2` is produced by booting the Server 2025 evaluation VHDX once with an `Unattend.xml` seed CD that completes OOBE and runs a built-in `bake.ps1`. Job VMs clone that image (virtio-blk + virtio-net, OVMF, fresh NVRAM copy), read the JIT config from a seed CD, run one job from a logon-triggered scheduled task, and power off. `qemu.Spec`, `seed`, `imagebake`, `imageprep`, `controller`, and `config` each gain a small Windows branch; the pool loop is untouched.

**Tech Stack:** Go 1.26 (stdlib + `gopkg.in/yaml.v3` only), QEMU/KVM with OVMF (edk2), `qemu-img`, `genisoimage`, PowerShell 5.1 guest scripts, virtio-win drivers.

**Spec:** `docs/superpowers/specs/2026-09-23-windows-runner-design.md`

## Global Constraints

- Go module `github.com/a1678991/github-qemu-runner`, Go 1.26, no new dependencies. `golangci-lint run` and `golangci-lint fmt --diff` must stay clean (`mise exec -- golangci-lint run`).
- Linux QEMU argv must stay byte-for-byte identical (`TestArgs` and a new `TestArgsLinuxUnchanged` guard it).
- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`); commitlint runs in the commit-msg hook. Every commit message ends with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- Work on branch `feat/windows-runner` (already exists, spec committed as `0d2e465`).
- Guest scripts are embedded via `scripts/embed.go`; `.editorconfig` already pins LF line endings for all files. PowerShell must be 5.1-compatible (no `??`, no ternary, no `-Parallel`).
- Windows pools require `memory_mb >= 2048`; `os: windows` is rejected on `backend: docker`.
- Default image URL `https://go.microsoft.com/fwlink/?linkid=2345826` (Server 2025 eval VHDX). Default virtio ISO `https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.302-1/virtio-win-0.1.302.iso` (versioned https; the `stable-virtio` alias redirects over plain http).
- Seed volume label `GHQSEED`; bake sentinel `BAKE-OK`; failure sentinel `BAKE-FAILED`; answer file name `Unattend.xml` (not `Autounattend.xml`).
- Images directory files: `windows-base.vhdx`, `windows-base.vhdx.meta`, `virtio-win.iso`, `bake-windows/`, `base-windows.qcow2`, `base-windows.json`.
- Tests needing `qemu-img` or `genisoimage` skip when the binary is absent (existing pattern); CI installs both.
- The pre-commit hook runs `nix fmt` on staged `.nix` files, so `nix` must be on PATH.
- **Deviation from spec, deliberate:** the spec lists a `NIC (virtio | e1000)` field on `qemu.Spec`. Nothing needs e1000 (virtio-net works from the first bake boot because the driver ISO is attached), so the field is not added. Everything else follows the spec.

---

## File structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` | `Pool.OS`, `Windows` block, defaults, validation, `HasQEMUOS` |
| `internal/config/ovmf.go` (new) | `OVMF` struct and `ResolveOVMF` firmware discovery |
| `internal/qemu/qemu.go` | `Spec` firmware/bus/extra-device fields, `Args`, `CreateOverlay` backing format, `CopyFile` |
| `internal/seed/seed.go` | `BuildISOFiles`; `BuildISO` becomes a wrapper |
| `internal/imagebake/bake.go` | `LatestRunner` gains `platform` |
| `internal/imagebake/releases.go` (new) | `LatestGitForWindows` |
| `internal/imagebake/download.go` (new) | `DownloadConditional` + sidecar |
| `internal/imagebake/unattend.go` (new) | `RenderUnattend`, `RandomPassword` |
| `internal/imagebake/windows.go` (new) | `WindowsOptions`, `BakeWindows` |
| `internal/imageprep/imageprep.go` | `windows` artifact in plan/Ensure |
| `internal/controller/provision.go` | Windows branch of `QEMUProvisioner` |
| `internal/controller/controller.go` | startup checks for Windows base + OVMF |
| `cmd/github-qemu-runner/main.go` | `setup` OVMF check, base image note, doc comment |
| `scripts/embed.go` | embed the three Windows guest files |
| `scripts/guest/windows/Unattend.xml`, `bake.ps1`, `run-one-job.ps1` (new) | guest side |
| `packaging/config.example.yaml`, `packaging/arch/PKGBUILD`, `README.md`, `nix/module.nix` | operator surface |

---

### Task 1: Config — `os: windows`, `windows:` block, OVMF discovery

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/config/ovmf.go`
- Test: `internal/config/config_test.go`, `internal/config/ovmf_test.go`

**Interfaces:**
- Produces:
  - `Pool.OS string` (yaml `os`; defaults to `"linux"`)
  - `type Windows struct { ImageURL, ImageSHA256, VirtioWinURL, VirtioWinSHA256, OVMFDir string }` on `Config.Windows` (yaml `windows`)
  - `const DefaultWindowsImageURL`, `const DefaultVirtioWinURL`
  - `func (c *Config) HasQEMUOS(os string) bool`
  - `type OVMF struct { Code, Vars string }`
  - `func ResolveOVMF(dir string) (OVMF, error)`

- [ ] **Step 1: Write failing config tests**

Append to `internal/config/config_test.go`:

```go
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
	if c.Windows.ImageURL != DefaultWindowsImageURL {
		t.Errorf("ImageURL = %q", c.Windows.ImageURL)
	}
	if c.Windows.VirtioWinURL != DefaultVirtioWinURL {
		t.Errorf("VirtioWinURL = %q", c.Windows.VirtioWinURL)
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
	y := windowsPoolYAML + "windows:\n  image_url: https://example.com/w.vhdx\n  image_sha256: " +
		strings.Repeat("a", 64) + "\n  ovmf_dir: /usr/share/edk2/x64\n"
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Windows.ImageURL != "https://example.com/w.vhdx" || c.Windows.OVMFDir != "/usr/share/edk2/x64" {
		t.Errorf("Windows = %+v", c.Windows)
	}
}
```

Create `internal/config/ovmf_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveOVMFExplicitDir(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "OVMF_CODE_4M.fd"))
	touch(t, filepath.Join(dir, "OVMF_VARS_4M.fd"))
	touch(t, filepath.Join(dir, "OVMF_CODE.secboot.4m.fd")) // must be ignored
	got, err := ResolveOVMF(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != filepath.Join(dir, "OVMF_CODE_4M.fd") || got.Vars != filepath.Join(dir, "OVMF_VARS_4M.fd") {
		t.Errorf("got %+v", got)
	}
}

func TestResolveOVMFPrefersArchNames(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "OVMF_CODE.4m.fd"))
	touch(t, filepath.Join(dir, "OVMF_CODE.fd"))
	touch(t, filepath.Join(dir, "OVMF_VARS.4m.fd"))
	got, err := ResolveOVMF(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got.Code) != "OVMF_CODE.4m.fd" {
		t.Errorf("Code = %q", got.Code)
	}
}

func TestResolveOVMFMissing(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "OVMF_CODE.fd")) // no VARS
	if _, err := ResolveOVMF(dir); err == nil {
		t.Error("want error when VARS missing")
	}
	if _, err := ResolveOVMF(filepath.Join(dir, "nope")); err == nil {
		t.Error("want error for missing dir")
	}
}

func TestResolveOVMFAutoDetect(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "OVMF_CODE.fd"))
	touch(t, filepath.Join(dir, "OVMF_VARS.fd"))
	old := ovmfSearchDirs
	ovmfSearchDirs = []string{filepath.Join(dir, "absent"), dir}
	t.Cleanup(func() { ovmfSearchDirs = old })
	got, err := ResolveOVMF("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Vars != filepath.Join(dir, "OVMF_VARS.fd") {
		t.Errorf("got %+v", got)
	}
	ovmfSearchDirs = []string{filepath.Join(dir, "absent")}
	if _, err := ResolveOVMF(""); err == nil {
		t.Error("want error when no search dir has firmware")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'Windows|LinuxPoolOS|ResolveOVMF' -v`
Expected: compile errors (`OS`, `Windows`, `DefaultWindowsImageURL`, `ResolveOVMF` undefined).

- [ ] **Step 3: Implement config changes**

In `internal/config/config.go`:

Add constants after the imports:

```go
const (
	// DefaultWindowsImageURL is Microsoft's evaluation-center link for the
	// Windows Server 2025 evaluation VHDX (English, x64). It redirects to
	// a versioned file on software-static.download.prss.microsoft.com.
	DefaultWindowsImageURL = "https://go.microsoft.com/fwlink/?linkid=2345826"
	// DefaultVirtioWinURL pins a versioned https URL: the unversioned
	// stable-virtio alias redirects through plain http.
	DefaultVirtioWinURL = "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.302-1/virtio-win-0.1.302.iso"
)
```

Add `Windows Windows \`yaml:"windows"\`` to `Config` (after `Docker`), and the type:

```go
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
```

Add to `Pool` after `Backend`:

```go
	// OS selects the guest for qemu pools: "linux" (default) or
	// "windows". Docker pools are always linux.
	OS string `yaml:"os"`
```

In `applyDefaults`, before the pools loop:

```go
	if c.Windows.ImageURL == "" {
		c.Windows.ImageURL = DefaultWindowsImageURL
	}
	if c.Windows.VirtioWinURL == "" {
		c.Windows.VirtioWinURL = DefaultVirtioWinURL
	}
	c.Windows.OVMFDir = os.ExpandEnv(c.Windows.OVMFDir)
```

and inside the loop after the Backend default:

```go
		if p.OS == "" {
			p.OS = "linux"
		}
```

In `validate`, after the `paths.run` check:

```go
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
```

with, next to `poolNameRe`:

```go
var sha256Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
```

Inside the pools loop, right after the backend check block:

```go
		if p.OS != "linux" && p.OS != "windows" {
			return fmt.Errorf(`pool %s: os must be "linux" or "windows"`, p.Name)
		}
		if p.OS == "windows" && p.Backend != "qemu" {
			return fmt.Errorf("pool %s: os: windows requires backend: qemu", p.Name)
		}
```

and change the memory check to:

```go
		minMem := 256
		if p.OS == "windows" {
			minMem = 2048
		}
		if p.MemoryMB < minMem {
			return fmt.Errorf("pool %s: memory_mb must be >= %d", p.Name, minMem)
		}
```

Append after `HasDockerIsolation`:

```go
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
```

Create `internal/config/ovmf.go`:

```go
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// OVMF is a UEFI firmware pair: the read-only code image and the
// pristine variable store that each VM copies before boot.
type OVMF struct {
	Code string
	Vars string
}

// ovmfSearchDirs are tried in order when windows.ovmf_dir is empty.
// Overridden by tests.
var ovmfSearchDirs = []string{
	"/usr/share/edk2/x64",      // Arch (edk2-ovmf)
	"/usr/share/OVMF",          // Debian/Ubuntu (ovmf)
	"/usr/share/edk2-ovmf/x64", // older Arch layout
}

// Secure-Boot variants (*.secboot.*, *.ms.fd) are deliberately absent:
// Windows Server does not need Secure Boot and the .ms variants require
// a matching enrolled VARS.
var (
	ovmfCodeNames = []string{"OVMF_CODE.4m.fd", "OVMF_CODE_4M.fd", "OVMF_CODE.fd"}
	ovmfVarsNames = []string{"OVMF_VARS.4m.fd", "OVMF_VARS_4M.fd", "OVMF_VARS.fd"}
)

// ResolveOVMF finds the firmware pair in dir, or in the distro search
// paths when dir is empty.
func ResolveOVMF(dir string) (OVMF, error) {
	if dir != "" {
		return resolveOVMFIn(dir)
	}
	for _, d := range ovmfSearchDirs {
		if fw, err := resolveOVMFIn(d); err == nil {
			return fw, nil
		}
	}
	return OVMF{}, fmt.Errorf("OVMF firmware not found in %v; install edk2-ovmf/ovmf or set windows.ovmf_dir", ovmfSearchDirs)
}

func resolveOVMFIn(dir string) (OVMF, error) {
	code, err := firstExisting(dir, ovmfCodeNames)
	if err != nil {
		return OVMF{}, err
	}
	vars, err := firstExisting(dir, ovmfVarsNames)
	if err != nil {
		return OVMF{}, err
	}
	return OVMF{Code: code, Vars: vars}, nil
}

func firstExisting(dir string, names []string) (string, error) {
	for _, n := range names {
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("none of %v found in %s", names, dir)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/config/ -v`
Expected: PASS (all, including the pre-existing ones).

- [ ] **Step 5: Lint and commit**

```bash
mise exec -- golangci-lint run ./internal/config/
git add internal/config
git commit -m "feat(config): add os: windows pools, windows block, and OVMF discovery

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: qemu.Spec — firmware, disk buses, extra devices, reboot control

**Files:**
- Modify: `internal/qemu/qemu.go`
- Test: `internal/qemu/qemu_test.go`

**Interfaces:**
- Produces on `qemu.Spec`:
  - `Firmware *Firmware` where `type Firmware struct { Code, Vars string }` (nil → SeaBIOS, as today)
  - `DiskBus string`: `""` → legacy `if=virtio` drive (Linux, unchanged); `"virtio"` → `-device virtio-blk-pci,...,bootindex=0`; `"ahci"` → `-device ide-hd,...,bus=ide.0,bootindex=0`
  - `SeedBus string`: `""` → legacy `if=virtio` raw drive; `"ahci"` → `ide-cd` on `ide.1`
  - `CDROMs []string`: extra ISOs as `ide-cd` on `ide.2`, `ide.3`, …
  - `ExtraDisks []Disk` where `type Disk struct { Path, Format string }` → `virtio-blk-pci` devices
  - `HyperV bool` → `-cpu host,hv_relaxed,hv_vapic,hv_spinlocks=0x1fff,hv_time`
  - `AllowReboot bool` → omit `-no-reboot`
- `func CreateOverlay(ctx, base, backingFormat, dest string, diskGB int) error` (new `backingFormat` parameter)
- `func CopyFile(src, dst string) error`

- [ ] **Step 1: Write failing tests**

Append to `internal/qemu/qemu_test.go`:

```go
// TestArgsLinuxUnchanged pins the exact Linux argv: Windows support must
// not perturb it.
func TestArgsLinuxUnchanged(t *testing.T) {
	dir := "/run/x"
	got := strings.Join(Args(testSpec(dir)), " ")
	want := "-accel kvm -cpu host -machine q35 -smp 2 -m 2048 " +
		"-drive file=/run/x/overlay.qcow2,if=virtio,format=qcow2 " +
		"-drive file=/run/x/seed.iso,if=virtio,format=raw,readonly=on " +
		"-netdev user,id=n0 -device virtio-net-pci,netdev=n0 -display none " +
		"-serial file:/run/x/console.log -qmp unix:/run/x/qmp.sock,server=on,wait=off " +
		"-pidfile /run/x/qemu.pid -no-reboot -name ghq-fmt-ab12"
	if got != want {
		t.Errorf("linux argv changed:\n got: %s\nwant: %s", got, want)
	}
}

func windowsJobSpec() Spec {
	s := testSpec("/run/w")
	s.Name = "ghq-win-ab12"
	s.Firmware = &Firmware{Code: "/fw/OVMF_CODE.fd", Vars: "/run/w/vars.fd"}
	s.DiskBus = "virtio"
	s.SeedBus = "ahci"
	s.HyperV = true
	return s
}

func TestArgsWindowsJob(t *testing.T) {
	got := strings.Join(Args(windowsJobSpec()), " ")
	want := "-accel kvm -cpu host,hv_relaxed,hv_vapic,hv_spinlocks=0x1fff,hv_time -machine q35 -smp 2 -m 2048 " +
		"-drive if=pflash,format=raw,readonly=on,file=/fw/OVMF_CODE.fd " +
		"-drive if=pflash,format=raw,file=/run/w/vars.fd " +
		"-drive file=/run/w/overlay.qcow2,if=none,id=boot,format=qcow2 " +
		"-device virtio-blk-pci,drive=boot,bootindex=0 " +
		"-drive file=/run/w/seed.iso,if=none,id=seed,format=raw,readonly=on,media=cdrom " +
		"-device ide-cd,drive=seed,bus=ide.1 " +
		"-netdev user,id=n0 -device virtio-net-pci,netdev=n0 -display none " +
		"-serial file:/run/w/console.log -qmp unix:/run/w/qmp.sock,server=on,wait=off " +
		"-pidfile /run/w/qemu.pid -no-reboot -name ghq-win-ab12"
	if got != want {
		t.Errorf("windows job argv:\n got: %s\nwant: %s", got, want)
	}
}

func TestArgsWindowsBake(t *testing.T) {
	s := windowsJobSpec()
	s.DiskBus = "ahci"
	s.AllowReboot = true
	s.ExtraDisks = []Disk{{Path: "/run/w/dummy.raw", Format: "raw"}}
	s.CDROMs = []string{"/img/virtio-win.iso"}
	got := strings.Join(Args(s), " ")
	for _, want := range []string{
		"-drive file=/run/w/overlay.qcow2,if=none,id=boot,format=qcow2 -device ide-hd,drive=boot,bus=ide.0,bootindex=0",
		"-drive file=/run/w/dummy.raw,if=none,id=extra0,format=raw -device virtio-blk-pci,drive=extra0",
		"-drive file=/img/virtio-win.iso,if=none,id=cd0,format=raw,readonly=on,media=cdrom -device ide-cd,drive=cd0,bus=ide.2",
		"-device ide-cd,drive=seed,bus=ide.1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bake argv missing %q\nargs: %s", want, got)
		}
	}
	if slices.Contains(Args(s), "-no-reboot") {
		t.Error("AllowReboot must drop -no-reboot (OOBE reboots)")
	}
}

func TestCreateOverlayBackingFormat(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.vhdx")
	if out, err := exec.Command("qemu-img", "create", "-f", "vhdx", base, "64M").CombinedOutput(); err != nil {
		t.Fatalf("create vhdx: %v: %s", err, out)
	}
	overlay := filepath.Join(dir, "overlay.qcow2")
	if err := CreateOverlay(context.Background(), base, "vhdx", overlay, 1); err != nil {
		t.Fatal(err)
	}
	info, err := exec.Command("qemu-img", "info", overlay).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v: %s", err, info)
	}
	if !strings.Contains(string(info), "backing file format: vhdx") {
		t.Errorf("backing format not vhdx:\n%s", info)
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	if err := os.WriteFile(src, []byte("vars"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "sub", "b")
	if err := CopyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "vars" {
		t.Errorf("copy = %q, %v", b, err)
	}
	fi, _ := os.Stat(dst)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
}
```

Add `"os"` to the test file's imports. Also update the two existing `CreateOverlay(...)` calls in this test file to pass `"qcow2"` as the third argument.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/qemu/ -run 'Args|CreateOverlay|CopyFile' -v`
Expected: compile errors (`Firmware`, `Disk`, `CopyFile` undefined; `CreateOverlay` arity).

- [ ] **Step 3: Implement**

Replace the `Spec` type and `Args` in `internal/qemu/qemu.go`:

```go
// Firmware is a UEFI firmware pair. Vars must be a per-VM writable copy
// (OVMF writes boot entries into it).
type Firmware struct {
	Code string
	Vars string
}

// Disk is an additional virtio-blk disk (raw or qcow2).
type Disk struct {
	Path   string
	Format string
}

type Spec struct {
	Name        string
	CPUs        int
	MemoryMB    int
	OverlayPath string
	SeedISOPath string
	ConsoleLog  string
	QMPSocket   string
	PIDFile     string

	// Firmware selects UEFI (OVMF) when non-nil; nil keeps SeaBIOS.
	Firmware *Firmware
	// DiskBus: "" keeps the legacy if=virtio drive (Linux); "virtio" is
	// an explicit virtio-blk-pci device with bootindex=0; "ahci" is a
	// SATA disk on ide.0 (Windows bake, before viostor is installed).
	DiskBus string
	// SeedBus: "" keeps the legacy if=virtio raw drive; "ahci" attaches
	// the seed ISO as a SATA CD-ROM on ide.1 (Windows has no virtio-blk
	// driver for the seed before the bake, and CD-ROM is what Windows
	// searches for Unattend.xml).
	SeedBus string
	// CDROMs are extra ISOs attached as SATA CD-ROMs on ide.2, ide.3, ...
	CDROMs []string
	// ExtraDisks are attached as virtio-blk-pci devices (the bake's dummy
	// disk that makes viostor bind as a boot-start driver).
	ExtraDisks []Disk
	// HyperV enables the Hyper-V enlightenments Windows guests expect.
	HyperV bool
	// AllowReboot omits -no-reboot. Only the Windows bake sets it: OOBE
	// reboots between the specialize and oobeSystem passes. Job VMs keep
	// -no-reboot so a guest reboot ends the job instead of looping.
	AllowReboot bool
}

// Args builds the qemu-system-x86_64 argv. -display none (not -nographic:
// that would redirect the serial port to stdio and fight -serial file:).
// With every optional Spec field zero the result is the historical Linux
// argv, byte for byte.
func Args(s Spec) []string {
	cpu := "host"
	if s.HyperV {
		cpu += ",hv_relaxed,hv_vapic,hv_spinlocks=0x1fff,hv_time"
	}
	args := []string{
		"-accel", "kvm",
		"-cpu", cpu,
		"-machine", "q35",
		"-smp", strconv.Itoa(s.CPUs),
		"-m", strconv.Itoa(s.MemoryMB),
	}
	if s.Firmware != nil {
		args = append(args,
			"-drive", "if=pflash,format=raw,readonly=on,file="+s.Firmware.Code,
			"-drive", "if=pflash,format=raw,file="+s.Firmware.Vars)
	}
	switch s.DiskBus {
	case "virtio":
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=none,id=boot,format=qcow2", s.OverlayPath),
			"-device", "virtio-blk-pci,drive=boot,bootindex=0")
	case "ahci":
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=none,id=boot,format=qcow2", s.OverlayPath),
			"-device", "ide-hd,drive=boot,bus=ide.0,bootindex=0")
	default:
		args = append(args, "-drive", fmt.Sprintf("file=%s,if=virtio,format=qcow2", s.OverlayPath))
	}
	for i, d := range s.ExtraDisks {
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=none,id=extra%d,format=%s", d.Path, i, d.Format),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=extra%d", i))
	}
	if s.SeedBus == "ahci" {
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=none,id=seed,format=raw,readonly=on,media=cdrom", s.SeedISOPath),
			"-device", "ide-cd,drive=seed,bus=ide.1")
	} else {
		args = append(args, "-drive", fmt.Sprintf("file=%s,if=virtio,format=raw,readonly=on", s.SeedISOPath))
	}
	for i, iso := range s.CDROMs {
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=none,id=cd%d,format=raw,readonly=on,media=cdrom", iso, i),
			"-device", fmt.Sprintf("ide-cd,drive=cd%d,bus=ide.%d", i, i+2))
	}
	args = append(args,
		"-netdev", "user,id=n0",
		"-device", "virtio-net-pci,netdev=n0",
		"-display", "none",
		"-serial", "file:"+s.ConsoleLog,
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", s.QMPSocket),
		"-pidfile", s.PIDFile,
	)
	if !s.AllowReboot {
		args = append(args, "-no-reboot")
	}
	return append(args, "-name", s.Name)
}
```

Change `CreateOverlay`'s signature and the `-F` argument:

```go
// CreateOverlay makes a qcow2 overlay backed by base (which must be an
// absolute path — qemu resolves relative backing paths against the overlay's
// directory) in backingFormat ("qcow2", "vhdx", ...) and grows its virtual
// size to diskGB when that exceeds the backing image's size. ...
func CreateOverlay(ctx context.Context, base, backingFormat, dest string, diskGB int) error {
	if out, err := exec.CommandContext(ctx, "qemu-img", "create",
		"-f", "qcow2", "-b", base, "-F", backingFormat, dest).CombinedOutput(); err != nil {
```

(the rest unchanged). Append:

```go
// CopyFile copies src to dst (mode 0600, parent created). Used for the
// per-VM OVMF variable store: the firmware writes to it, so VMs must
// not share the distro's pristine file.
func CopyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
```

Add `"path/filepath"` to the imports. Update the two existing callers: `internal/controller/provision.go` → `qemu.CreateOverlay(ctx, q.BasePath, "qcow2", overlay, p.DiskGB)`; `internal/imagebake/bake.go` → `qemu.CreateOverlay(ctx, absCloudImg, "qcow2", overlay, 20)`.

- [ ] **Step 4: Run tests**

Run: `go build ./... && go test ./internal/qemu/ ./internal/controller/ ./internal/imagebake/ -v`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
mise exec -- golangci-lint run
git add internal/qemu internal/controller/provision.go internal/imagebake/bake.go
git commit -m "feat(qemu): UEFI firmware, disk bus, extra device, and reboot options on Spec

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: seed.BuildISOFiles

**Files:**
- Modify: `internal/seed/seed.go`
- Test: `internal/seed/seed_test.go`

**Interfaces:**
- Produces: `func BuildISOFiles(ctx context.Context, dir, volid string, files map[string]string) (string, error)` → writes each file into `dir` (0600), packs them into `dir/seed.iso` with `-volid volid -joliet -rock`, chmod 0600, returns the ISO path. `BuildISO` keeps its signature and delegates with `volid "cidata"`.

- [ ] **Step 1: Write failing test**

Append to `internal/seed/seed_test.go`:

```go
func TestBuildISOFiles(t *testing.T) {
	if _, err := exec.LookPath("genisoimage"); err != nil {
		t.Skip("genisoimage not installed")
	}
	dir := t.TempDir()
	iso, err := BuildISOFiles(context.Background(), dir, "GHQSEED", map[string]string{
		"Unattend.xml":    "<unattend/>",
		"runner-jit.conf": "JIT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if iso != filepath.Join(dir, "seed.iso") {
		t.Errorf("iso path = %q", iso)
	}
	fi, err := os.Stat(iso)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("stat = %v, %v", fi, err)
	}
	for _, f := range []string{"Unattend.xml", "runner-jit.conf"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if _, err := exec.LookPath("isoinfo"); err == nil {
		out, err := exec.Command("isoinfo", "-d", "-i", iso).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Volume id: GHQSEED") {
			t.Errorf("volume id: %v\n%s", err, out)
		}
		out, _ = exec.Command("isoinfo", "-J", "-f", "-i", iso).CombinedOutput()
		if !strings.Contains(string(out), "/Unattend.xml") {
			t.Errorf("Joliet name not preserved:\n%s", out)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/seed/ -run BuildISOFiles -v`
Expected: compile error, `BuildISOFiles` undefined.

- [ ] **Step 3: Implement**

Replace `BuildISO` in `internal/seed/seed.go` with:

```go
// BuildISO writes user-data/meta-data into dir and packs them into
// dir/seed.iso with the volume label cloud-init's NoCloud datasource looks
// for. Requires genisoimage on PATH.
func BuildISO(ctx context.Context, dir, userData, metaData string) (string, error) {
	return BuildISOFiles(ctx, dir, "cidata", map[string]string{
		"user-data": userData,
		"meta-data": metaData,
	})
}

// BuildISOFiles writes files (name -> content) into dir and packs them
// into dir/seed.iso with the given volume label. Joliet keeps the exact
// (mixed-case) names, which Windows needs for Unattend.xml; Rock Ridge
// does the same for Linux.
func BuildISOFiles(ctx context.Context, dir, volid string, files map[string]string) (string, error) {
	names := make([]string, 0, len(files))
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return "", err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	iso := filepath.Join(dir, "seed.iso")
	cmd := exec.CommandContext(ctx, "genisoimage",
		append([]string{"-output", "seed.iso", "-volid", volid, "-joliet", "-rock"}, names...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("genisoimage: %v: %s", err, out)
	}
	// The ISO may carry the JIT config; match the source files' 0600.
	if err := os.Chmod(iso, 0o600); err != nil {
		return "", err
	}
	return iso, nil
}
```

Add `"sort"` to the imports.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/seed/ -v`
Expected: PASS (existing `TestBuildISO` still passes through the wrapper).

- [ ] **Step 5: Commit**

```bash
mise exec -- golangci-lint run ./internal/seed/
git add internal/seed
git commit -m "feat(seed): BuildISOFiles for arbitrary seed contents and volume labels

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Release resolvers and conditional download

**Files:**
- Modify: `internal/imagebake/bake.go` (`LatestRunner` platform parameter), `internal/dockerbackend/bake.go` (caller)
- Create: `internal/imagebake/releases.go`, `internal/imagebake/download.go`
- Test: `internal/imagebake/bake_test.go`, `internal/imagebake/releases_test.go`, `internal/imagebake/download_test.go`

**Interfaces:**
- `func LatestRunner(ctx, client *http.Client, apiBase, platform, arch string) (Release, error)` — `platform` is `"linux"` or `"win"`; asset `actions-runner-linux-<arch>-<v>.tar.gz` or `actions-runner-win-<arch>-<v>.zip`; SHA marker `<!-- BEGIN SHA <platform>-<arch> -->`. `Release.TarballURL` keeps its name (it is the asset URL whatever the extension).
- `func LatestGitForWindows(ctx, client *http.Client, apiBase string) (Release, error)` — asset `Git-<v>-64-bit.exe` from `repos/git-for-windows/git/releases/latest`; SHA from the release-notes table row `Git-<v>-64-bit.exe | <sha>`; empty SHA when absent.
- `func DownloadConditional(ctx, client *http.Client, url, dest string) (cached bool, err error)` — sidecar `dest + ".meta"` (JSON `{"etag","last_modified","content_length"}`), `If-None-Match`/`If-Modified-Since`, 304 → `cached=true`.

- [ ] **Step 1: Write failing tests**

In `internal/imagebake/bake_test.go`, change every `LatestRunner(ctx, client, url, "x64")` / `"arm64"` call to `LatestRunner(ctx, client, url, "linux", "x64")` / `"linux", "arm64"`. Then append:

```go
func TestLatestRunnerWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.337.0",
			"body": "- actions-runner-win-x64-2.337.0.zip <!-- BEGIN SHA win-x64 -->" + strings.Repeat("e", 64) + "<!-- END SHA win-x64 -->\n" +
				"- actions-runner-linux-x64-2.337.0.tar.gz <!-- BEGIN SHA linux-x64 -->" + strings.Repeat("c", 64) + "<!-- END SHA linux-x64 -->\n",
			"assets": []map[string]any{
				{"name": "actions-runner-linux-x64-2.337.0.tar.gz", "browser_download_url": "https://x/linux.tar.gz"},
				{"name": "actions-runner-win-x64-2.337.0.zip", "browser_download_url": "https://x/win.zip"},
			},
		})
	}))
	defer srv.Close()
	rel, err := LatestRunner(context.Background(), srv.Client(), srv.URL, "win", "x64")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "2.337.0" || rel.TarballURL != "https://x/win.zip" || rel.SHA256 != strings.Repeat("e", 64) {
		t.Errorf("rel = %+v", rel)
	}
}
```

Create `internal/imagebake/releases_test.go`:

```go
package imagebake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLatestGitForWindows(t *testing.T) {
	sha := strings.Repeat("d", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/git-for-windows/git/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.55.0.windows.5",
			"body": "Filename | SHA-256\n-------- | -------\n" +
				"Git-2.55.0.5-64-bit.exe | " + sha + "\n" +
				"Git-2.55.0.5-arm64.exe | " + strings.Repeat("a", 64) + "\n",
			"assets": []map[string]any{
				{"name": "Git-2.55.0.5-arm64.exe", "browser_download_url": "https://x/arm64.exe"},
				{"name": "Git-2.55.0.5-64-bit.exe", "browser_download_url": "https://x/64.exe"},
				{"name": "PortableGit-2.55.0.5-64-bit.7z.exe", "browser_download_url": "https://x/p.exe"},
			},
		})
	}))
	defer srv.Close()
	rel, err := LatestGitForWindows(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "2.55.0.windows.5" || rel.TarballURL != "https://x/64.exe" || rel.SHA256 != sha {
		t.Errorf("rel = %+v", rel)
	}
}

func TestLatestGitForWindowsNoSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.55.0.windows.5",
			"body":     "no table",
			"assets": []map[string]any{
				{"name": "Git-2.55.0.5-64-bit.exe", "browser_download_url": "https://x/64.exe"},
			},
		})
	}))
	defer srv.Close()
	rel, err := LatestGitForWindows(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel.SHA256 != "" {
		t.Errorf("SHA256 = %q, want empty", rel.SHA256)
	}
}

func TestLatestGitForWindowsMissingAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1", "body": "", "assets": []map[string]any{}})
	}))
	defer srv.Close()
	if _, err := LatestGitForWindows(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Error("want error when no 64-bit installer asset")
	}
}
```

Create `internal/imagebake/download_test.go`:

```go
package imagebake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestDownloadConditional(t *testing.T) {
	var hits atomic.Int32
	body := "vhdx-bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Wed, 16 Oct 2024 15:40:30 GMT")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "windows-base.vhdx")
	ctx := context.Background()

	cached, err := DownloadConditional(ctx, srv.Client(), srv.URL, dest)
	if err != nil || cached {
		t.Fatalf("first: cached=%v err=%v", cached, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != body {
		t.Errorf("content = %q", b)
	}
	var meta downloadMeta
	mb, err := os.ReadFile(dest + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.ETag != `"v1"` || meta.LastModified != "Wed, 16 Oct 2024 15:40:30 GMT" || meta.ContentLength != int64(len(body)) {
		t.Errorf("meta = %+v", meta)
	}

	cached, err = DownloadConditional(ctx, srv.Client(), srv.URL, dest)
	if err != nil || !cached {
		t.Fatalf("second: cached=%v err=%v", cached, err)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2 (one full, one 304)", hits.Load())
	}
}

func TestDownloadConditionalNoSidecarRefetches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			t.Error("validators sent without a sidecar")
		}
		_, _ = w.Write([]byte("new"))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(dest, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	cached, err := DownloadConditional(context.Background(), srv.Client(), srv.URL, dest)
	if err != nil || cached {
		t.Fatalf("cached=%v err=%v", cached, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "new" {
		t.Errorf("content = %q", b)
	}
}

func TestDownloadConditionalHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "f")
	if _, err := DownloadConditional(context.Background(), srv.Client(), srv.URL, dest); err == nil {
		t.Error("want error on 403")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("failed download left a file behind")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/imagebake/ 2>&1 | head`
Expected: compile errors (`LatestRunner` arity, `LatestGitForWindows`, `DownloadConditional`, `downloadMeta` undefined).

- [ ] **Step 3: Implement**

In `internal/imagebake/bake.go`, replace `LatestRunner` with:

```go
// LatestRunner resolves the newest actions/runner release for the given
// platform ("linux" or "win") and arch ("x64" or "arm64"). Linux assets
// are tarballs, Windows assets are zips; TarballURL is the asset URL in
// both cases. The SHA is scraped from the release notes; if the notes
// format changes, SHA256 comes back empty and the caller proceeds on TLS
// alone.
func LatestRunner(ctx context.Context, client *http.Client, apiBase, platform, arch string) (Release, error) {
	rel, err := fetchLatestRelease(ctx, client, apiBase, "actions/runner")
	if err != nil {
		return Release{}, err
	}
	version := strings.TrimPrefix(rel.TagName, "v")
	ext := "tar.gz"
	if platform == "win" {
		ext = "zip"
	}
	want := fmt.Sprintf("actions-runner-%s-%s-%s.%s", platform, arch, version, ext)
	out := Release{Version: version}
	for _, a := range rel.Assets {
		if a.Name == want {
			out.TarballURL = a.BrowserDownloadURL
			break
		}
	}
	if out.TarballURL == "" {
		return Release{}, fmt.Errorf("asset %s not found in release %s", want, rel.TagName)
	}
	// SHA comes from the "<!-- BEGIN SHA <platform>-<arch> -->" markers
	// GitHub embeds in the release notes' checksum table; the asset name
	// alone is ambiguous (it also appears in the install instructions, and
	// the first hex token after that is a different platform's SHA).
	key := regexp.QuoteMeta(platform + "-" + arch)
	re := regexp.MustCompile(`<!-- BEGIN SHA ` + key + ` -->\s*([0-9a-fA-F]{64})\s*<!-- END SHA ` + key + ` -->`)
	if m := re.FindStringSubmatch(rel.Body); m != nil {
		out.SHA256 = strings.ToLower(m[1])
	}
	return out, nil
}
```

and update its caller in the same file: `LatestRunner(ctx, o.HTTP, o.APIBase, "linux", "x64")`. In `internal/dockerbackend/bake.go`: `imagebake.LatestRunner(ctx, o.HTTP, o.APIBase, "linux", RunnerArch(runtime.GOARCH))`.

Create `internal/imagebake/releases.go`:

```go
package imagebake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// ghRelease is the slice of GitHub's release object the bakes need.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatestRelease(ctx context.Context, client *http.Client, apiBase, repo string) (ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(apiBase, "/")+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return ghRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return ghRelease{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ghRelease{}, fmt.Errorf("%s releases/latest: %s", repo, resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return ghRelease{}, err
	}
	return rel, nil
}

var gitForWindowsAssetRe = regexp.MustCompile(`^Git-[0-9][0-9.]*-64-bit\.exe$`)

// LatestGitForWindows resolves the newest Git for Windows 64-bit
// installer. The SHA-256 comes from the "<asset> | <sha>" table in the
// release notes; when the table is absent SHA256 is empty and the caller
// proceeds on TLS alone.
func LatestGitForWindows(ctx context.Context, client *http.Client, apiBase string) (Release, error) {
	rel, err := fetchLatestRelease(ctx, client, apiBase, "git-for-windows/git")
	if err != nil {
		return Release{}, err
	}
	out := Release{Version: strings.TrimPrefix(rel.TagName, "v")}
	var name string
	for _, a := range rel.Assets {
		if gitForWindowsAssetRe.MatchString(a.Name) {
			name, out.TarballURL = a.Name, a.BrowserDownloadURL
			break
		}
	}
	if out.TarballURL == "" {
		return Release{}, fmt.Errorf("no Git-*-64-bit.exe asset in git-for-windows release %s", rel.TagName)
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*\|\s*([0-9a-fA-F]{64})\s*$`)
	if m := re.FindStringSubmatch(rel.Body); m != nil {
		out.SHA256 = strings.ToLower(m[1])
	}
	return out, nil
}
```

Then delete the inline `var rel struct{...}` decoding from `LatestRunner` (it now uses `fetchLatestRelease`; the `encoding/json` import in `bake.go` is still needed by `Bake` for the sidecar).

Create `internal/imagebake/download.go`:

```go
package imagebake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// downloadMeta is the validator sidecar next to a conditionally
// downloaded file (<dest>.meta).
type downloadMeta struct {
	ETag          string `json:"etag"`
	LastModified  string `json:"last_modified"`
	ContentLength int64  `json:"content_length"`
}

// DownloadConditional fetches url to dest unless the server reports it
// unchanged (304) against the validators saved in <dest>.meta. For large
// upstream files with no published checksum (the Windows evaluation
// VHDX). Returns cached=true when the existing file was kept. Any error
// leaves dest as it was.
func DownloadConditional(ctx context.Context, client *http.Client, url, dest string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	metaPath := dest + ".meta"
	if _, statErr := os.Stat(dest); statErr == nil {
		if mb, err := os.ReadFile(metaPath); err == nil {
			var meta downloadMeta
			if json.Unmarshal(mb, &meta) == nil {
				if meta.ETag != "" {
					req.Header.Set("If-None-Match", meta.ETag)
				}
				if meta.LastModified != "" {
					req.Header.Set("If-Modified-Since", meta.LastModified)
				}
			}
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified {
		return true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	meta := downloadMeta{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"), ContentLength: n}
	mb, err := json.Marshal(meta)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(metaPath, mb, 0o644); err != nil && !errors.Is(err, os.ErrPermission) {
		return false, err
	}
	return false, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go build ./... && go test ./internal/imagebake/ ./internal/dockerbackend/ -v`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
mise exec -- golangci-lint run
git add internal/imagebake internal/dockerbackend/bake.go
git commit -m "feat(imagebake): windows runner + git-for-windows release resolvers, conditional download

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Guest assets — Unattend.xml template, bake.ps1, run-one-job.ps1, RenderUnattend

**Files:**
- Create: `scripts/guest/windows/Unattend.xml`, `scripts/guest/windows/bake.ps1`, `scripts/guest/windows/run-one-job.ps1`
- Modify: `scripts/embed.go`
- Create: `internal/imagebake/unattend.go`
- Test: `scripts/embed_test.go`, `internal/imagebake/unattend_test.go`

**Interfaces:**
- `scripts.WindowsUnattend`, `scripts.WindowsBake`, `scripts.WindowsRunOneJob` (strings)
- `func RenderUnattend(adminPassword string) (string, error)` — fills `{{.AdminPassword}}` in the template
- `func RandomPassword(n int) (string, error)` — `n` chars from `[A-Za-z0-9]` via `crypto/rand`, always containing at least one of each class (Windows complexity policy)
- Guest contract: seed volume label `GHQSEED`; bake reads `bake-env.json` with keys `runner_version`, `runner_url`, `runner_sha256`, `git_version`, `git_url`, `git_sha256`; job VM reads `runner-jit.conf`

- [ ] **Step 1: Write failing tests**

Append to `scripts/embed_test.go`:

```go
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
}
```

Create `internal/imagebake/unattend_test.go`:

```go
package imagebake

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestRenderUnattend(t *testing.T) {
	out, err := RenderUnattend("Abc123xyz")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "{{") {
		t.Error("template residue in output")
	}
	if strings.Count(out, "<Value>Abc123xyz</Value>") != 2 {
		t.Errorf("password must appear in AdministratorPassword and AutoLogon:\n%s", out)
	}
	var doc struct {
		XMLName  xml.Name `xml:"unattend"`
		Settings []struct {
			Pass string `xml:"pass,attr"`
		} `xml:"settings"`
	}
	if err := xml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not well-formed XML: %v", err)
	}
	passes := map[string]bool{}
	for _, s := range doc.Settings {
		passes[s.Pass] = true
	}
	if !passes["specialize"] || !passes["oobeSystem"] {
		t.Errorf("passes = %v", passes)
	}
}

func TestRenderUnattendEscapes(t *testing.T) {
	out, err := RenderUnattend(`a<b&"c`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a&lt;b&amp;&#34;c") {
		t.Errorf("password not XML-escaped:\n%s", out)
	}
}

func TestRandomPassword(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		p, err := RandomPassword(32)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 32 {
			t.Errorf("len = %d", len(p))
		}
		var upper, lower, digit bool
		for _, c := range p {
			switch {
			case c >= 'A' && c <= 'Z':
				upper = true
			case c >= 'a' && c <= 'z':
				lower = true
			case c >= '0' && c <= '9':
				digit = true
			default:
				t.Errorf("unexpected char %q", c)
			}
		}
		if !upper || !lower || !digit {
			t.Errorf("%q lacks a character class (Windows complexity policy)", p)
		}
		if seen[p] {
			t.Error("duplicate password")
		}
		seen[p] = true
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./scripts/ ./internal/imagebake/ 2>&1 | head`
Expected: compile errors (`WindowsUnattend`, `RenderUnattend`, `RandomPassword` undefined).

- [ ] **Step 3: Create the answer file template**

`scripts/guest/windows/Unattend.xml` (Go `text/template`; the only action is `{{.AdminPassword}}`, which `RenderUnattend` pre-escapes for XML):

```xml
<?xml version="1.0" encoding="utf-8"?>
<!-- Bake-boot answer file for the Windows Server evaluation VHDX. Found by
     Windows Setup at the specialize and oobeSystem passes because it sits
     at the root of a CD-ROM and is named Unattend.xml (Autounattend.xml is
     NOT searched at these passes despite the documentation table). Job
     VMs never see this file: OOBE is complete in the baked image. -->
<unattend xmlns="urn:schemas-microsoft-com:unattend">
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <ComputerName>ghq-bake</ComputerName>
      <TimeZone>UTC</TimeZone>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <InputLocale>en-US</InputLocale>
      <SystemLocale>en-US</SystemLocale>
      <UILanguage>en-US</UILanguage>
      <UserLocale>en-US</UserLocale>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <HideLocalAccountScreen>true</HideLocalAccountScreen>
        <HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
        <ProtectYourPC>3</ProtectYourPC>
      </OOBE>
      <UserAccounts>
        <AdministratorPassword>
          <Value>{{.AdminPassword}}</Value>
          <PlainText>true</PlainText>
        </AdministratorPassword>
      </UserAccounts>
      <AutoLogon>
        <Enabled>true</Enabled>
        <LogonCount>1</LogonCount>
        <Username>Administrator</Username>
        <Password>
          <Value>{{.AdminPassword}}</Value>
          <PlainText>true</PlainText>
        </Password>
      </AutoLogon>
      <FirstLogonCommands>
        <SynchronousCommand wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
          <Order>1</Order>
          <Description>github-qemu-runner bake</Description>
          <CommandLine>powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$d = (Get-Volume | Where-Object FileSystemLabel -eq 'GHQSEED' | Select-Object -First 1).DriveLetter; &amp; ($d + ':\bake.ps1')"</CommandLine>
        </SynchronousCommand>
      </FirstLogonCommands>
    </component>
  </settings>
</unattend>
```

- [ ] **Step 4: Create bake.ps1**

`scripts/guest/windows/bake.ps1`:

```powershell
# Runs ONCE as Administrator (unattend FirstLogonCommands) during the
# image bake boot. Installs virtio drivers, the runner user, Git, and the
# actions runner, then powers off. The host watches the serial console for
# BAKE-OK; any failure prints BAKE-FAILED and powers off so the bake is
# rejected quickly instead of hanging until the host timeout.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$serial = New-Object System.IO.Ports.SerialPort 'COM1', 115200, 'None', 8, 'One'
$serial.Open()
function Log([string]$msg) {
    $line = "[bake $(Get-Date -Format HH:mm:ss)] $msg"
    $serial.WriteLine($line)
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
        $out = & pnputil.exe /add-driver $inf.FullName /install 2>&1 | Out-String
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
    $gitVer = & 'C:\Program Files\Git\cmd\git.exe' --version
    Log "git: $gitVer"

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
        $lic = Get-CimInstance SoftwareLicensingProduct -Filter "PartialProductKey IS NOT NULL AND ApplicationID='55c92734-d682-4d71-983e-d6ec3f16059f'" | Select-Object -First 1
        Log "licence status=$($lic.LicenseStatus) grace=$($lic.GracePeriodRemaining)min"
    } catch { Log "licence activation skipped: $($_.Exception.Message)" }

    Log 'BAKE-OK'
} catch {
    Log "BAKE-FAILED: $($_.Exception.Message) at $($_.InvocationInfo.PositionMessage)"
} finally {
    $serial.Close()
    Stop-Computer -Force
}
```

- [ ] **Step 5: Create run-one-job.ps1**

`scripts/guest/windows/run-one-job.ps1`:

```powershell
# Runs at `runner` logon (scheduled task ghq-run-one-job, interactive,
# highest privileges) inside the ephemeral job VM. Runs exactly one job,
# then powers the guest off NO MATTER WHAT — host-side teardown depends
# on the qemu process exiting.
$ErrorActionPreference = 'Continue'
$serial = New-Object System.IO.Ports.SerialPort 'COM1', 115200, 'None', 8, 'One'
$serial.Open()
function Log([string]$msg) { $serial.WriteLine("[run-one-job $(Get-Date -Format HH:mm:ss)] $msg") }

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
    $serial.Close()
    Stop-Computer -Force
}
```

- [ ] **Step 6: Embed and render**

Append to `scripts/embed.go`:

```go
//go:embed guest/windows/Unattend.xml
var WindowsUnattend string

//go:embed guest/windows/bake.ps1
var WindowsBake string

//go:embed guest/windows/run-one-job.ps1
var WindowsRunOneJob string
```

Update the package comment to mention PowerShell: `// Package scripts embeds the guest-side shell and PowerShell scripts...`.

Create `internal/imagebake/unattend.go`:

```go
package imagebake

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"html"
	"math/big"
	"text/template"

	"github.com/a1678991/github-qemu-runner/scripts"
)

var unattendTmpl = template.Must(template.New("unattend").Parse(scripts.WindowsUnattend))

// RenderUnattend fills the bake answer file with the one-time
// Administrator password. The value is XML-escaped here; the template
// uses text/template so nothing else is transformed.
func RenderUnattend(adminPassword string) (string, error) {
	var buf bytes.Buffer
	err := unattendTmpl.Execute(&buf, struct{ AdminPassword string }{html.EscapeString(adminPassword)})
	if err != nil {
		return "", fmt.Errorf("render Unattend.xml: %w", err)
	}
	return buf.String(), nil
}

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// RandomPassword returns n alphanumeric characters from crypto/rand
// with at least one upper, one lower, and one digit (Windows complexity
// policy). Alphanumeric only, so it is safe in XML and command lines.
func RandomPassword(n int) (string, error) {
	if n < 3 {
		return "", fmt.Errorf("password length %d too short", n)
	}
	for {
		b := make([]byte, n)
		var upper, lower, digit bool
		for i := range b {
			k, err := rand.Int(rand.Reader, big.NewInt(int64(len(passwordAlphabet))))
			if err != nil {
				return "", err
			}
			c := passwordAlphabet[k.Int64()]
			b[i] = c
			switch {
			case c >= 'A' && c <= 'Z':
				upper = true
			case c >= 'a' && c <= 'z':
				lower = true
			default:
				digit = true
			}
		}
		if upper && lower && digit {
			return string(b), nil
		}
	}
}
```

- [ ] **Step 7: Run tests**

Run: `go test ./scripts/ ./internal/imagebake/ -v`
Expected: PASS.

- [ ] **Step 8: Lint and commit**

```bash
mise exec -- golangci-lint run
git add scripts internal/imagebake/unattend.go internal/imagebake/unattend_test.go
git commit -m "feat(scripts): Windows guest assets (Unattend.xml, bake.ps1, run-one-job.ps1)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: imagebake.BakeWindows

**Files:**
- Create: `internal/imagebake/windows.go`
- Test: `internal/imagebake/windows_test.go`

**Interfaces:**
- Consumes: `qemu.Spec` fields from Task 2, `seed.BuildISOFiles` (Task 3), `LatestRunner`/`LatestGitForWindows`/`DownloadConditional`/`DownloadVerified` (Task 4), `RenderUnattend`/`RandomPassword`/`scripts.WindowsBake`/`scripts.WindowsRunOneJob` (Task 5).
- Produces:

```go
type WindowsOptions struct {
	ImageDir        string
	HTTP            *http.Client
	APIBase         string
	ImageURL        string
	ImageSHA256     string
	VirtioWinURL    string
	VirtioWinSHA256 string
	OVMFCode        string
	OVMFVars        string
	QEMUBin         string
	CPUs            int
	MemoryMB        int
	Timeout         time.Duration
	Log             *slog.Logger
}
func BakeWindows(ctx context.Context, o WindowsOptions) error
```
  Output files: `<ImageDir>/base-windows.qcow2`, `<ImageDir>/base-windows.json`.

- [ ] **Step 1: Write failing test**

Create `internal/imagebake/windows_test.go`:

```go
package imagebake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeQEMU writes sentinel to the file named by "-serial file:<path>"
// and exits, standing in for the bake VM.
func fakeQEMU(t *testing.T, dir, sentinel string) string {
	t.Helper()
	p := filepath.Join(dir, "fake-qemu")
	script := "#!/bin/sh\nprev=\nfor a in \"$@\"; do\n" +
		"  if [ \"$prev\" = -serial ]; then echo '" + sentinel + "' > \"${a#file:}\"; fi\n" +
		"  prev=$a\ndone\nexit 0\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func windowsBakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	vhdx := filepath.Join(t.TempDir(), "src.vhdx")
	if out, err := exec.Command("qemu-img", "create", "-f", "vhdx", vhdx, "64M").CombinedOutput(); err != nil {
		t.Fatalf("create vhdx: %v: %s", err, out)
	}
	vhdxBytes, err := os.ReadFile(vhdx)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/image.vhdx", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"img-1"`)
		_, _ = w.Write(vhdxBytes)
	})
	mux.HandleFunc("/virtio-win.iso", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake-iso"))
	})
	mux.HandleFunc("/repos/actions/runner/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.337.0",
			"body":     "actions-runner-win-x64-2.337.0.zip <!-- BEGIN SHA win-x64 -->" + strings.Repeat("e", 64) + "<!-- END SHA win-x64 -->",
			"assets":   []map[string]any{{"name": "actions-runner-win-x64-2.337.0.zip", "browser_download_url": "https://x/win.zip"}},
		})
	})
	mux.HandleFunc("/repos/git-for-windows/git/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.55.0.windows.5",
			"body":     "Git-2.55.0.5-64-bit.exe | " + strings.Repeat("d", 64),
			"assets":   []map[string]any{{"name": "Git-2.55.0.5-64-bit.exe", "browser_download_url": "https://x/git.exe"}},
		})
	})
	return httptest.NewServer(mux)
}

func requireBakeTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"qemu-img", "genisoimage"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

func TestBakeWindows(t *testing.T) {
	requireBakeTools(t)
	srv := windowsBakeServer(t)
	defer srv.Close()
	dir := t.TempDir()
	fw := filepath.Join(dir, "fw")
	if err := os.MkdirAll(fw, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"OVMF_CODE.fd", "OVMF_VARS.fd"} {
		if err := os.WriteFile(filepath.Join(fw, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	images := filepath.Join(dir, "images")
	err := BakeWindows(context.Background(), WindowsOptions{
		ImageDir:     images,
		HTTP:         srv.Client(),
		APIBase:      srv.URL,
		ImageURL:     srv.URL + "/image.vhdx",
		VirtioWinURL: srv.URL + "/virtio-win.iso",
		OVMFCode:     filepath.Join(fw, "OVMF_CODE.fd"),
		OVMFVars:     filepath.Join(fw, "OVMF_VARS.fd"),
		QEMUBin:      fakeQEMU(t, dir, "BAKE-OK"),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(images, "base-windows.qcow2")
	info, err := exec.Command("qemu-img", "info", base).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v: %s", err, info)
	}
	if strings.Contains(string(info), "backing file") {
		t.Errorf("base must be flattened:\n%s", info)
	}
	mb, err := os.ReadFile(filepath.Join(images, "base-windows.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]string
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["runner_version"] != "2.337.0" || meta["git_version"] != "2.55.0.windows.5" || meta["image_etag"] != `"img-1"` || meta["baked_at"] == "" {
		t.Errorf("meta = %v", meta)
	}
	if _, err := os.Stat(filepath.Join(images, "bake-windows")); !os.IsNotExist(err) {
		t.Error("bake dir not cleaned up")
	}
	for _, f := range []string{"windows-base.vhdx", "windows-base.vhdx.meta", "virtio-win.iso"} {
		if _, err := os.Stat(filepath.Join(images, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestBakeWindowsNoSentinel(t *testing.T) {
	requireBakeTools(t)
	srv := windowsBakeServer(t)
	defer srv.Close()
	dir := t.TempDir()
	for _, f := range []string{"OVMF_CODE.fd", "OVMF_VARS.fd"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	images := filepath.Join(dir, "images")
	err := BakeWindows(context.Background(), WindowsOptions{
		ImageDir: images, HTTP: srv.Client(), APIBase: srv.URL,
		ImageURL: srv.URL + "/image.vhdx", VirtioWinURL: srv.URL + "/virtio-win.iso",
		OVMFCode: filepath.Join(dir, "OVMF_CODE.fd"), OVMFVars: filepath.Join(dir, "OVMF_VARS.fd"),
		QEMUBin: fakeQEMU(t, dir, "BAKE-FAILED: boom"),
	})
	if err == nil || !strings.Contains(err.Error(), "BAKE-FAILED: boom") {
		t.Errorf("err = %v, want console tail with BAKE-FAILED", err)
	}
	if _, err := os.Stat(filepath.Join(images, "base-windows.qcow2")); !os.IsNotExist(err) {
		t.Error("failed bake must not publish a base image")
	}
}

func TestBakeSeedContents(t *testing.T) {
	requireBakeTools(t)
	srv := windowsBakeServer(t)
	defer srv.Close()
	dir := t.TempDir()
	for _, f := range []string{"OVMF_CODE.fd", "OVMF_VARS.fd"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A fake qemu that copies the bake dir aside before exiting so the
	// test can inspect the seed files (BakeWindows removes bake-windows/).
	keep := filepath.Join(dir, "keep")
	fake := filepath.Join(dir, "fake-qemu")
	script := "#!/bin/sh\nprev=\nfor a in \"$@\"; do\n" +
		"  if [ \"$prev\" = -serial ]; then f=\"${a#file:}\"; echo BAKE-OK > \"$f\"; cp -r \"$(dirname \"$f\")\" " + keep + "; fi\n" +
		"  prev=$a\ndone\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := BakeWindows(context.Background(), WindowsOptions{
		ImageDir: filepath.Join(dir, "images"), HTTP: srv.Client(), APIBase: srv.URL,
		ImageURL: srv.URL + "/image.vhdx", VirtioWinURL: srv.URL + "/virtio-win.iso",
		OVMFCode: filepath.Join(dir, "OVMF_CODE.fd"), OVMFVars: filepath.Join(dir, "OVMF_VARS.fd"),
		QEMUBin: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"Unattend.xml", "bake.ps1", "run-one-job.ps1", "bake-env.json", "seed.iso", "vars.fd", "dummy.raw", "bake-overlay.qcow2"} {
		if _, err := os.Stat(filepath.Join(keep, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	env, err := os.ReadFile(filepath.Join(keep, "bake-env.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e map[string]string
	if err := json.Unmarshal(env, &e); err != nil {
		t.Fatal(err)
	}
	if e["runner_url"] != "https://x/win.zip" || e["runner_sha256"] != strings.Repeat("e", 64) ||
		e["git_url"] != "https://x/git.exe" || e["git_sha256"] != strings.Repeat("d", 64) {
		t.Errorf("bake-env = %v", e)
	}
	if vars, _ := os.ReadFile(filepath.Join(keep, "vars.fd")); string(vars) != "OVMF_VARS.fd" {
		t.Error("vars.fd must be a copy of the pristine OVMF_VARS")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/imagebake/ -run BakeWindows -v`
Expected: compile error, `WindowsOptions`/`BakeWindows` undefined.

- [ ] **Step 3: Implement**

Create `internal/imagebake/windows.go`:

```go
package imagebake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/qemu"
	"github.com/a1678991/github-qemu-runner/internal/seed"
	"github.com/a1678991/github-qemu-runner/scripts"
)

// WindowsOptions configures BakeWindows. Zero values for HTTP, APIBase,
// CPUs, MemoryMB, Timeout, and Log take the same defaults as Options.
type WindowsOptions struct {
	ImageDir        string
	HTTP            *http.Client
	APIBase         string
	ImageURL        string
	ImageSHA256     string
	VirtioWinURL    string
	VirtioWinSHA256 string
	OVMFCode        string
	OVMFVars        string
	QEMUBin         string
	CPUs            int
	MemoryMB        int
	Timeout         time.Duration
	Log             *slog.Logger
}

func (o *WindowsOptions) defaults() {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Minute} // 11 GB image
	}
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	if o.CPUs == 0 {
		o.CPUs = 4
	}
	if o.MemoryMB == 0 {
		o.MemoryMB = 8192
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
}

// Windows image artifact names under ImageDir.
const (
	WindowsVHDX     = "windows-base.vhdx"
	VirtioWinISO    = "virtio-win.iso"
	WindowsBase     = "base-windows.qcow2"
	WindowsBaseMeta = "base-windows.json"
	windowsBakeDir  = "bake-windows"
)

// BakeWindows produces <ImageDir>/base-windows.qcow2: download the
// evaluation VHDX and the virtio-win ISO, resolve the runner and Git
// releases, boot an overlay of the VHDX under OVMF with an Unattend.xml
// seed CD that completes OOBE and runs bake.ps1, require the BAKE-OK
// serial sentinel, flatten, swap.
func BakeWindows(ctx context.Context, o WindowsOptions) error {
	o.defaults()
	if err := os.MkdirAll(o.ImageDir, 0o755); err != nil {
		return err
	}
	vhdx := filepath.Join(o.ImageDir, WindowsVHDX)
	if err := o.fetch(ctx, o.ImageURL, vhdx, o.ImageSHA256, "windows image"); err != nil {
		return err
	}
	iso := filepath.Join(o.ImageDir, VirtioWinISO)
	if err := o.fetch(ctx, o.VirtioWinURL, iso, o.VirtioWinSHA256, "virtio-win ISO"); err != nil {
		return err
	}

	runner, err := LatestRunner(ctx, o.HTTP, o.APIBase, "win", "x64")
	if err != nil {
		return fmt.Errorf("resolve runner release: %w", err)
	}
	if runner.SHA256 == "" {
		o.Log.Warn("runner zip SHA not found in release notes; relying on TLS only")
	}
	git, err := LatestGitForWindows(ctx, o.HTTP, o.APIBase)
	if err != nil {
		return fmt.Errorf("resolve git-for-windows release: %w", err)
	}
	if git.SHA256 == "" {
		o.Log.Warn("git installer SHA not found in release notes; relying on TLS only")
	}
	o.Log.Info("baking windows image", "runner_version", runner.Version, "git_version", git.Version)

	bakeDir := filepath.Join(o.ImageDir, windowsBakeDir)
	if err := os.RemoveAll(bakeDir); err != nil {
		return err
	}
	if err := os.MkdirAll(bakeDir, 0o700); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(bakeDir) }()

	absVHDX, err := filepath.Abs(vhdx)
	if err != nil {
		return err
	}
	overlay := filepath.Join(bakeDir, "bake-overlay.qcow2")
	if err := qemu.CreateOverlay(ctx, absVHDX, "vhdx", overlay, 0); err != nil {
		return err
	}
	dummy := filepath.Join(bakeDir, "dummy.raw")
	if out, err := exec.CommandContext(ctx, "qemu-img", "create", "-f", "raw", dummy, "1G").CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create dummy: %v: %s", err, out)
	}
	vars := filepath.Join(bakeDir, "vars.fd")
	if err := qemu.CopyFile(o.OVMFVars, vars); err != nil {
		return fmt.Errorf("copy OVMF vars: %w", err)
	}

	password, err := RandomPassword(32)
	if err != nil {
		return err
	}
	unattend, err := RenderUnattend(password)
	if err != nil {
		return err
	}
	env, err := json.MarshalIndent(map[string]string{
		"runner_version": runner.Version,
		"runner_url":     runner.TarballURL,
		"runner_sha256":  runner.SHA256,
		"git_version":    git.Version,
		"git_url":        git.TarballURL,
		"git_sha256":     git.SHA256,
	}, "", "  ")
	if err != nil {
		return err
	}
	seedISO, err := seed.BuildISOFiles(ctx, bakeDir, "GHQSEED", map[string]string{
		"Unattend.xml":    unattend,
		"bake.ps1":        scripts.WindowsBake,
		"run-one-job.ps1": scripts.WindowsRunOneJob,
		"bake-env.json":   string(env),
	})
	if err != nil {
		return err
	}

	console := filepath.Join(bakeDir, "console.log")
	vm, err := qemu.Start(ctx, o.QEMUBin, qemu.Spec{
		Name: "ghq-bake-windows", CPUs: o.CPUs, MemoryMB: o.MemoryMB,
		OverlayPath: overlay, SeedISOPath: seedISO, ConsoleLog: console,
		QMPSocket:  filepath.Join(bakeDir, "qmp.sock"),
		PIDFile:    filepath.Join(bakeDir, "qemu.pid"),
		Firmware:   &qemu.Firmware{Code: o.OVMFCode, Vars: vars},
		DiskBus:    "ahci", // the VHDX boots from SATA; viostor is installed during this boot
		SeedBus:    "ahci",
		CDROMs:     []string{iso},
		ExtraDisks: []qemu.Disk{{Path: dummy, Format: "raw"}}, // makes viostor bind → boot-start
		HyperV:     true,
		AllowReboot: true, // OOBE reboots between specialize and oobeSystem
	})
	if err != nil {
		return err
	}
	select {
	case <-vm.Done():
	case <-time.After(o.Timeout):
		_ = vm.Kill()
		return fmt.Errorf("windows bake timed out after %v; console tail:\n%s", o.Timeout, consoleTail(console))
	case <-ctx.Done():
		_ = vm.Kill()
		return ctx.Err()
	}
	consoleOut, err := os.ReadFile(console)
	if err != nil {
		return fmt.Errorf("read console log: %w", err)
	}
	if !bytes.Contains(consoleOut, []byte("BAKE-OK")) {
		return fmt.Errorf("windows bake failed (no BAKE-OK sentinel); console tail:\n%s", lastBytes(consoleOut, 2000))
	}

	newBase := filepath.Join(o.ImageDir, WindowsBase+".new")
	if out, err := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2", overlay, newBase).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %v: %s", err, out)
	}
	if err := os.Rename(newBase, filepath.Join(o.ImageDir, WindowsBase)); err != nil {
		return err
	}
	var imgMeta downloadMeta
	if mb, err := os.ReadFile(vhdx + ".meta"); err == nil {
		_ = json.Unmarshal(mb, &imgMeta)
	}
	meta, err := json.MarshalIndent(map[string]string{
		"runner_version": runner.Version,
		"git_version":    git.Version,
		"image_url":      o.ImageURL,
		"image_etag":     imgMeta.ETag,
		"baked_at":       time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.ImageDir, WindowsBaseMeta), append(meta, '\n'), 0o644); err != nil {
		return err
	}
	o.Log.Info("windows bake complete", "base", filepath.Join(o.ImageDir, WindowsBase))
	return nil
}

// fetch downloads url to dest: checksum-verified when sha is set,
// otherwise conditionally (ETag/Last-Modified). A network failure on the
// conditional path keeps an existing file, so an upstream outage does
// not block a rebake.
func (o *WindowsOptions) fetch(ctx context.Context, url, dest, sha, what string) error {
	o.Log.Info("downloading "+what+" (cached if unchanged)", "url", url)
	if sha != "" {
		return DownloadVerified(ctx, o.HTTP, url, dest, sha)
	}
	cached, err := DownloadConditional(ctx, o.HTTP, url, dest)
	if err != nil {
		if _, statErr := os.Stat(dest); statErr == nil {
			o.Log.Warn("conditional download failed; using the cached file", "what", what, "err", err)
			return nil
		}
		return fmt.Errorf("download %s: %w", what, err)
	}
	if cached {
		o.Log.Info(what + " unchanged upstream; using cached file")
	}
	return nil
}
```

Note `CreateOverlay(..., 0)`: a diskGB of 0 never exceeds the backing size, so no resize happens; the bake keeps the VHDX's virtual size.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/imagebake/ -v`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
mise exec -- golangci-lint run
git add internal/imagebake/windows.go internal/imagebake/windows_test.go
git commit -m "feat(imagebake): bake the Windows base image from the evaluation VHDX

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: imageprep — the `windows` artifact

**Files:**
- Modify: `internal/imageprep/imageprep.go`
- Test: `internal/imageprep/imageprep_test.go`

**Interfaces:**
- Consumes: `config.HasQEMUOS`, `config.ResolveOVMF` (Task 1), `imagebake.BakeWindows`/`WindowsOptions`/`WindowsBase` (Task 6).
- Produces: `imagePlan.Windows bool`; artifact name `"windows"` for `present`.

- [ ] **Step 1: Write failing test**

In `internal/imageprep/imageprep_test.go`, add to the `TestPlan` fixtures and cases:

```go
	win := config.Pool{Backend: "qemu", OS: "windows"}
```

and extend the case struct with `wantWindows bool` (add it as the last field), setting `false` on all existing cases and adding:

```go
		{"windows absent", []config.Pool{win}, false, map[string]bool{"windows": false}, false, nil, true},
		{"windows present", []config.Pool{win}, false, map[string]bool{"windows": true}, false, nil, false},
		{"windows force", []config.Pool{qemu, win}, true, map[string]bool{"qemu": true, "windows": true}, true, nil, true},
		{"linux pool does not bake windows", []config.Pool{qemu}, true, map[string]bool{}, true, nil, false},
```

and the assertion:

```go
			if got.Windows != tc.wantWindows {
				t.Errorf("Windows = %v, want %v", got.Windows, tc.wantWindows)
			}
```

Note the existing `qemu` fixture must become `config.Pool{Backend: "qemu", OS: "linux"}` (config defaults are not applied to hand-built structs).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/imageprep/ -v`
Expected: compile error, `Windows` field undefined.

- [ ] **Step 3: Implement**

In `internal/imageprep/imageprep.go`:

```go
// imagePlan is the set of artifacts to bake.
type imagePlan struct {
	QEMU     bool     // base.qcow2 (linux qemu pools)
	Windows  bool     // base-windows.qcow2 (windows qemu pools)
	Variants []string // subset of {"dind","slim"}, in that order
}
```

In `plan`, change the qemu line to gate on the OS and add Windows:

```go
	if cfg.HasQEMUOS("linux") && (force || !present("qemu")) {
		p.QEMU = true
	}
	if cfg.HasQEMUOS("windows") && (force || !present("windows")) {
		p.Windows = true
	}
```

In `Ensure`, add a `"windows"` case to `present`:

```go
		case "windows":
			_, statErr := os.Stat(filepath.Join(cfg.Paths.Images, imagebake.WindowsBase))
			return statErr == nil
```

and after the `p.QEMU` block:

```go
	if p.Windows {
		fw, err := config.ResolveOVMF(cfg.Windows.OVMFDir)
		if err != nil {
			return err
		}
		if err := imagebake.BakeWindows(ctx, imagebake.WindowsOptions{
			ImageDir:        cfg.Paths.Images,
			APIBase:         cfg.GitHub.APIBaseURL,
			ImageURL:        cfg.Windows.ImageURL,
			ImageSHA256:     cfg.Windows.ImageSHA256,
			VirtioWinURL:    cfg.Windows.VirtioWinURL,
			VirtioWinSHA256: cfg.Windows.VirtioWinSHA256,
			OVMFCode:        fw.Code,
			OVMFVars:        fw.Vars,
			QEMUBin:         qemuBin,
			Log:             log,
		}); err != nil {
			return err
		}
	}
```

Update the doc comment on `plan`: `"qemu" (base.qcow2), "windows" (base-windows.qcow2), "dind"/"slim" (docker images)`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/imageprep/ -v`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
mise exec -- golangci-lint run
git add internal/imageprep
git commit -m "feat(imageprep): bake base-windows.qcow2 for windows pools

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Controller — Windows provisioning and startup checks

**Files:**
- Modify: `internal/controller/provision.go`, `internal/controller/controller.go`
- Test: `internal/controller/provision_test.go`

**Interfaces:**
- Consumes: `qemu.Spec`/`CopyFile`/`Firmware` (Task 2), `seed.BuildISOFiles` (Task 3), `config.HasQEMUOS`/`ResolveOVMF` (Task 1), `imagebake.WindowsBase` (Task 6).
- Produces on `QEMUProvisioner`: `WindowsBasePath string` (absolute path to `base-windows.qcow2`, may be empty when no Windows pool) and `Firmware *qemu.Firmware` (pristine OVMF paths).

- [ ] **Step 1: Write failing test**

Append to `internal/controller/provision_test.go`:

```go
func TestProvisionWindows(t *testing.T) {
	for _, bin := range []string{"qemu-img", "genisoimage"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base-windows.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", base, "1G").CombinedOutput(); err != nil {
		t.Fatalf("create base: %v: %s", err, out)
	}
	vars := filepath.Join(dir, "OVMF_VARS.fd")
	if err := os.WriteFile(vars, []byte("pristine"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Fake qemu records its argv and exits.
	argv := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "fake-qemu")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" > "+argv+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "run")
	q := &QEMUProvisioner{
		RunDir: runDir, BasePath: filepath.Join(dir, "missing-linux-base.qcow2"), QEMUBin: fake,
		WindowsBasePath: base,
		Firmware:        &qemu.Firmware{Code: filepath.Join(dir, "OVMF_CODE.fd"), Vars: vars},
	}
	pcfg := config.Pool{Name: "win", OS: "windows", CPUs: 2, MemoryMB: 2048, DiskGB: 10}
	vm, cleanup, err := q.Provision(context.Background(), "ghq-win-test", pcfg, "JITBLOB")
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(runDir, "ghq-win-test")
	for _, f := range []string{"overlay.qcow2", "seed.iso", "runner-jit.conf", "vars.fd"} {
		if _, err := os.Stat(filepath.Join(workdir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(workdir, "vars.fd")); string(b) != "pristine" {
		t.Error("vars.fd must be a copy of the pristine OVMF_VARS")
	}
	if _, err := os.Stat(filepath.Join(workdir, "user-data")); !os.IsNotExist(err) {
		t.Error("windows VMs must not get cloud-init user-data")
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("fake qemu did not exit")
	}
	got, _ := os.ReadFile(argv)
	for _, want := range []string{
		"-device virtio-blk-pci,drive=boot,bootindex=0",
		"-device ide-cd,drive=seed,bus=ide.1",
		"if=pflash,format=raw,file=" + filepath.Join(workdir, "vars.fd"),
		"hv_time",
		"-no-reboot",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("argv missing %q:\n%s", want, got)
		}
	}
	cleanup()
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Errorf("workdir not removed: %v", err)
	}
}
```

Add `"strings"` and `"github.com/a1678991/github-qemu-runner/internal/qemu"` to the test imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run ProvisionWindows -v`
Expected: compile error (`WindowsBasePath`, `Firmware` fields undefined).

- [ ] **Step 3: Implement provisioning**

Replace `internal/controller/provision.go` with:

```go
// Package controller wires config, GitHub client, provisioning, and pools
// into the running daemon.
package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/pool"
	"github.com/a1678991/github-qemu-runner/internal/qemu"
	"github.com/a1678991/github-qemu-runner/internal/seed"
)

// QEMUProvisioner builds a per-VM working directory (overlay + seed ISO)
// under RunDir and boots the VM. Linux pools use BasePath and cloud-init;
// Windows pools use WindowsBasePath, OVMF, and a plain seed CD.
type QEMUProvisioner struct {
	RunDir   string // <Paths.Run>
	BasePath string // <Paths.Images>/base.qcow2, absolute
	QEMUBin  string

	WindowsBasePath string         // <Paths.Images>/base-windows.qcow2, absolute; empty without windows pools
	Firmware        *qemu.Firmware // pristine OVMF pair; each VM copies Vars
}

func (q *QEMUProvisioner) Provision(ctx context.Context, name string, p config.Pool, jitConfig string) (pool.VM, func(), error) {
	dir := filepath.Join(q.RunDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	fail := func(err error) (pool.VM, func(), error) {
		cleanup()
		return nil, nil, err
	}

	spec := qemu.Spec{
		Name:        name,
		CPUs:        p.CPUs,
		MemoryMB:    p.MemoryMB,
		OverlayPath: filepath.Join(dir, "overlay.qcow2"),
		ConsoleLog:  filepath.Join(dir, "console.log"),
		QMPSocket:   filepath.Join(dir, "qmp.sock"),
		PIDFile:     filepath.Join(dir, "qemu.pid"),
	}
	var err error
	if p.OS == "windows" {
		err = q.prepareWindows(ctx, dir, p, jitConfig, &spec)
	} else {
		err = q.prepareLinux(ctx, dir, name, p, jitConfig, &spec)
	}
	if err != nil {
		return fail(err)
	}
	vm, err := qemu.Start(ctx, q.QEMUBin, spec)
	if err != nil {
		return fail(fmt.Errorf("start VM: %w", err))
	}
	return vm, cleanup, nil
}

func (q *QEMUProvisioner) prepareLinux(ctx context.Context, dir, name string, p config.Pool, jitConfig string, spec *qemu.Spec) error {
	if err := qemu.CreateOverlay(ctx, q.BasePath, "qcow2", spec.OverlayPath, p.DiskGB); err != nil {
		return err
	}
	ud, err := seed.UserData(jitConfig)
	if err != nil {
		return err
	}
	iso, err := seed.BuildISO(ctx, dir, ud, seed.MetaData(name, name))
	if err != nil {
		return err
	}
	spec.SeedISOPath = iso
	return nil
}

// prepareWindows: virtio-blk boot disk (viostor is boot-start in the
// baked image), virtio-net, the JIT blob on a SATA CD-ROM that the baked
// run-one-job task reads by volume label, UEFI with a private NVRAM copy.
func (q *QEMUProvisioner) prepareWindows(ctx context.Context, dir string, p config.Pool, jitConfig string, spec *qemu.Spec) error {
	if q.WindowsBasePath == "" || q.Firmware == nil {
		return fmt.Errorf("windows pools are not initialised (no base image or firmware)")
	}
	if err := qemu.CreateOverlay(ctx, q.WindowsBasePath, "qcow2", spec.OverlayPath, p.DiskGB); err != nil {
		return err
	}
	iso, err := seed.BuildISOFiles(ctx, dir, "GHQSEED", map[string]string{"runner-jit.conf": jitConfig})
	if err != nil {
		return err
	}
	vars := filepath.Join(dir, "vars.fd")
	if err := qemu.CopyFile(q.Firmware.Vars, vars); err != nil {
		return fmt.Errorf("copy OVMF vars: %w", err)
	}
	spec.SeedISOPath = iso
	spec.Firmware = &qemu.Firmware{Code: q.Firmware.Code, Vars: vars}
	spec.DiskBus = "virtio"
	spec.SeedBus = "ahci"
	spec.HyperV = true
	return nil
}
```

- [ ] **Step 4: Implement startup checks**

In `internal/controller/controller.go`, replace the `if cfg.HasBackend("qemu") { ... }` block with:

```go
	var qemuProv *QEMUProvisioner
	if cfg.HasBackend("qemu") {
		qemuBin, err := exec.LookPath("qemu-system-x86_64")
		if err != nil {
			return fmt.Errorf("qemu-system-x86_64 not found: %w", err)
		}
		qemuProv = &QEMUProvisioner{RunDir: runDir, QEMUBin: qemuBin}
		if cfg.HasQEMUOS("linux") {
			basePath, err := filepath.Abs(filepath.Join(cfg.Paths.Images, "base.qcow2"))
			if err != nil {
				return err
			}
			if _, err := os.Stat(basePath); err != nil {
				return fmt.Errorf("base image missing (run `github-qemu-runner refresh-image` first): %w", err)
			}
			qemuProv.BasePath = basePath
		}
		if cfg.HasQEMUOS("windows") {
			winPath, err := filepath.Abs(filepath.Join(cfg.Paths.Images, imagebake.WindowsBase))
			if err != nil {
				return err
			}
			if _, err := os.Stat(winPath); err != nil {
				return fmt.Errorf("windows base image missing (run `github-qemu-runner refresh-image` first): %w", err)
			}
			fw, err := config.ResolveOVMF(cfg.Windows.OVMFDir)
			if err != nil {
				return err
			}
			qemuProv.WindowsBasePath = winPath
			qemuProv.Firmware = &qemu.Firmware{Code: fw.Code, Vars: fw.Vars}
		}
	}
```

Add imports `"github.com/a1678991/github-qemu-runner/internal/imagebake"` and `"github.com/a1678991/github-qemu-runner/internal/qemu"`.

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/controller/ -v`
Expected: PASS (both the Linux and Windows provisioning tests).

- [ ] **Step 6: Lint and commit**

```bash
mise exec -- golangci-lint run
git add internal/controller
git commit -m "feat(controller): provision Windows job VMs (OVMF, virtio, seed CD)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: `setup` preflight and command docs

**Files:**
- Modify: `cmd/github-qemu-runner/main.go`

**Interfaces:**
- Consumes: `config.HasQEMUOS`, `config.ResolveOVMF` (Task 1), `imagebake.WindowsBase` (Task 6).

- [ ] **Step 1: Add the Windows checks to `runSetup`**

Inside the existing `if cfg.HasBackend("qemu") { ... }` block that checks binaries and `/dev/kvm`, append:

```go
		if cfg.HasQEMUOS("windows") {
			fw, err := config.ResolveOVMF(cfg.Windows.OVMFDir)
			check("OVMF firmware (windows pools)", err)
			if err == nil {
				fmt.Printf("ok    OVMF code %s\n", fw.Code)
			}
		}
```

Replace the trailing base-image note block with:

```go
	if cfg.HasQEMUOS("linux") {
		base := filepath.Join(cfg.Paths.Images, "base.qcow2")
		if _, err := os.Stat(base); err != nil {
			fmt.Printf("note  base image missing; run `github-qemu-runner refresh-image`\n")
		} else {
			fmt.Printf("ok    base image %s\n", base)
		}
	}
	if cfg.HasQEMUOS("windows") {
		base := filepath.Join(cfg.Paths.Images, imagebake.WindowsBase)
		if _, err := os.Stat(base); err != nil {
			fmt.Printf("note  windows base image missing; run `github-qemu-runner refresh-image`\n")
		} else {
			fmt.Printf("ok    windows base image %s\n", base)
		}
	}
```

Add the `imagebake` import. Update the package doc comment's first sentence to: `runs ephemeral GitHub Actions runners in QEMU/KVM virtual machines (Linux or Windows guests) or sandboxed Docker containers`.

- [ ] **Step 2: Build and smoke-test setup against a Windows config**

```bash
go build -o /tmp/ghq ./cmd/github-qemu-runner
cat > /tmp/ghq-win.yaml <<'EOF'
github:
  app_id: 1
  installation_id: 2
  private_key_path: /nonexistent.pem
state_dir: /tmp/ghq-state
pools:
  - name: win
    os: windows
    scope: org
    org: x
    count: 1
    cpus: 2
    memory_mb: 2048
    disk_gb: 64
    labels: [self-hosted, windows]
EOF
/tmp/ghq -config /tmp/ghq-win.yaml setup; echo "exit=$?"
```

Expected: lines `ok    OVMF firmware (windows pools)`, `ok    OVMF code /usr/share/edk2/x64/OVMF_CODE.4m.fd`, `note  windows base image missing; ...`, `FAIL  private key readable` (expected on this throwaway config), and no `base image` line for Linux. Exit 1 because of the key.

- [ ] **Step 3: Lint, test, commit**

```bash
mise exec -- golangci-lint run && go test ./...
git add cmd/github-qemu-runner/main.go
git commit -m "feat(setup): preflight OVMF and the Windows base image

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: Operator surface — example config, packaging, Nix module, README

**Files:**
- Modify: `packaging/config.example.yaml`, `packaging/arch/PKGBUILD`, `nix/module.nix`, `README.md`

- [ ] **Step 1: Example config**

In `packaging/config.example.yaml`, after the `#docker:` block add:

```yaml
# Windows pools (os: windows on qemu pools) boot a baked copy of the
# Windows Server evaluation image. All keys optional; see "Windows pools"
# in the README for host prerequisites (OVMF) and licensing notes.
#windows:
#  image_url: https://go.microsoft.com/fwlink/?linkid=2345826   # Server 2025 eval VHDX
#  image_sha256: ""          # verify the download when set (Microsoft publishes no checksum)
#  virtio_win_url: https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.302-1/virtio-win-0.1.302.iso
#  virtio_win_sha256: ""
#  ovmf_dir: /usr/share/edk2/x64   # auto-detected when unset
```

and after the `build` pool add:

```yaml
  #- name: win
  #  os: windows         # linux (default) | windows; qemu backend only
  #  scope: org
  #  org: my-org
  #  count: 1
  #  cpus: 4
  #  memory_mb: 8192     # windows pools: >= 2048
  #  disk_gb: 80         # floor is the image's 64 GiB virtual size
  #  labels: [self-hosted, windows, x64]
```

- [ ] **Step 2: PKGBUILD**

After `depends=('qemu-base' 'cdrtools')` add:

```bash
optdepends=('edk2-ovmf: UEFI firmware for Windows pools (os: windows)')
```

- [ ] **Step 3: Nix module**

In `nix/module.nix`, add an option next to `refresh`:

```nix
    windows.enable = lib.mkEnableOption "Windows pools (adds OVMF firmware and sets windows.ovmf_dir)";
```

and in `config = lib.mkIf cfg.enable { ... }` add:

```nix
    services.github-qemu-runner.settings.windows.ovmf_dir = lib.mkIf cfg.windows.enable (
      lib.mkDefault "${pkgs.OVMF.fd}/FV"
    );
```

(`pkgs.OVMF.fd` ships `FV/OVMF_CODE.fd` and `FV/OVMF_VARS.fd`, which `ResolveOVMF` finds by its third name candidate.) Run `nix fmt nix/module.nix` (the pre-commit hook does this too).

- [ ] **Step 4: README**

Add a `## Windows pools` section after the Docker backend section:

```markdown
## Windows pools

`backend: qemu` pools can run Windows guests with `os: windows`. The base
image is baked from Microsoft's **Windows Server 2025 evaluation** VHDX
(English, x64): `refresh-image` downloads it (11 GB, cached across bakes
via ETag), boots it once under UEFI with an answer file that completes
OOBE unattended, installs virtio drivers from the virtio-win ISO, Git for
Windows, and the actions runner (win-x64, checksum-verified), and flattens
the result to `base-windows.qcow2`. Job VMs then clone it exactly like
Linux pools: virtio-blk + virtio-net, the JIT config on a seed CD-ROM, one
job, power off.

```yaml
pools:
  - name: win
    os: windows
    scope: org
    org: my-org
    count: 1
    cpus: 4
    memory_mb: 8192            # >= 2048 on windows pools
    disk_gb: 80                # floor: the image's 64 GiB virtual size
    labels: [self-hosted, windows, x64]

# optional overrides; every key has a default
windows:
  # image_url: https://go.microsoft.com/fwlink/?linkid=2345826   # Server 2025 eval VHDX
  # image_sha256: ""          # verify when set; Microsoft publishes no checksum file
  # virtio_win_url: ...       # versioned https URL of the virtio-win ISO
  # ovmf_dir: /usr/share/edk2/x64   # auto-detected on Arch and Debian/Ubuntu
```

Inside the guest, jobs run as the local administrator `runner` in an
interactive session (parity with GitHub-hosted Windows runners), with
`git` on the PATH, Windows Update disabled, and long paths enabled. There
is no Docker inside Windows jobs.

Host prerequisites on top of the Linux qemu backend: OVMF firmware
(Arch: `pacman -S edk2-ovmf`; Debian/Ubuntu: `apt install ovmf`; NixOS:
`services.github-qemu-runner.windows.enable = true`). `setup` checks for
it when a Windows pool is configured.

Licensing, plainly: the evaluation edition runs for 180 days and is not a
production licence. Each `refresh-image` starts from the pristine download,
so the weekly refresh timer keeps clones inside the window; whether that
use is acceptable is between you and Microsoft. Set `image_url` to a
different VHDX (e.g. Server 2022 eval, or your own generalised image with
the same layout) to change the base.
```

Also add `os` and the memory/disk notes to the pools table:

```markdown
| `os` | no | `linux` | `linux` or `windows`; qemu backend only — see "Windows pools" |
```

and adjust the `memory_mb` row to `≥ 256 (≥ 2048 on windows pools)`. In the `refresh-image` row of the Commands table append `, including the Windows base when a windows pool exists`. In Requirements add `- OVMF firmware for windows pools (see "Windows pools")`.

- [ ] **Step 5: Verify docs build nothing is broken, commit**

```bash
go test ./... && mise exec -- golangci-lint run
git add packaging/config.example.yaml packaging/arch/PKGBUILD nix/module.nix README.md
git commit -m "docs: document Windows pools; ship OVMF hints in packaging and the Nix module

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 11: Manual checkpoint — real bake on this host

**Files:** none (verification only). Requires `/dev/kvm`, `edk2-ovmf`, and a local cache of the evaluation VHDX and the virtio-win ISO (any directory; `$WIN_CACHE/` below) so the bake does not redownload 11 GB.

- [ ] **Step 1: Seed the images directory from the local cache** (avoids an 11 GB redownload; the sidecar carries the validators observed on 2026-09-23 so the conditional GET returns 304)

```bash
mkdir -p /tmp/ghq-state/images
ln $WIN_CACHE/windows-base.vhdx /tmp/ghq-state/images/windows-base.vhdx 2>/dev/null || cp $WIN_CACHE/windows-base.vhdx /tmp/ghq-state/images/
cp $WIN_CACHE/virtio-win.iso /tmp/ghq-state/images/
cat > /tmp/ghq-state/images/windows-base.vhdx.meta <<'EOF'
{"etag":"\"0x7DEF244A2066D9DCF7BD9ABDF684BC9A35AFA9DAC9DF8D9A041A41D907B19D4E\"","last_modified":"Wed, 16 Oct 2024 15:40:30 GMT","content_length":11686379520}
EOF
```

(If `/tmp` is a small tmpfs on the host, use a directory on disk instead and set `state_dir` accordingly; the bake overlay grows by a few GB and the flattened base is about 12 GB.)

- [ ] **Step 2: Run the bake**

```bash
go build -o /tmp/ghq ./cmd/github-qemu-runner
/tmp/ghq -config /tmp/ghq-win.yaml refresh-image 2>&1 | tee /tmp/ghq-bake.log
```

(`/tmp/ghq-win.yaml` is the config from Task 9 with `state_dir: /tmp/ghq-state`; adjust `state_dir` if you moved it.)
Expected within 15 minutes: log lines `windows image unchanged upstream; using cached file`, `baking windows image runner_version=... git_version=...`, `windows bake complete`. Then:

```bash
qemu-img info /tmp/ghq-state/images/base-windows.qcow2 | head -4
cat /tmp/ghq-state/images/base-windows.json
```

Expected: `file format: qcow2`, no `backing file` line, virtual size 64 GiB; JSON with the versions and the ETag.

- [ ] **Step 3: If the bake fails**, the error carries the serial-console tail. `BAKE-FAILED: ...` lines point at the PowerShell step; only firmware `BdsDxe` lines mean the answer file was not applied (check the seed ISO with `isoinfo -J -f -i <bake dir>/seed.iso` before the bake dir is removed by re-running with a breakpoint, or inspect `console.log` while the VM runs). Fix, re-run, and record the fix in the commit that addresses it.

- [ ] **Step 4: Boot one clone by hand and watch it reach the runner task** (no GitHub App needed; a fake JIT blob makes the runner fail fast and the VM shut down)

```bash
mkdir -p /tmp/ghq-clone && cd /tmp/ghq-clone
qemu-img create -f qcow2 -b /tmp/ghq-state/images/base-windows.qcow2 -F qcow2 overlay.qcow2
qemu-img resize overlay.qcow2 80G
printf 'not-a-real-jit' > runner-jit.conf
genisoimage -quiet -output seed.iso -volid GHQSEED -joliet -rock runner-jit.conf
cp /usr/share/edk2/x64/OVMF_VARS.4m.fd vars.fd
qemu-system-x86_64 -accel kvm -cpu host,hv_relaxed,hv_vapic,hv_spinlocks=0x1fff,hv_time -machine q35 -smp 2 -m 4096 \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/edk2/x64/OVMF_CODE.4m.fd -drive if=pflash,format=raw,file=vars.fd \
  -drive file=overlay.qcow2,if=none,id=boot,format=qcow2 -device virtio-blk-pci,drive=boot,bootindex=0 \
  -drive file=seed.iso,if=none,id=seed,format=raw,readonly=on,media=cdrom -device ide-cd,drive=seed,bus=ide.1 \
  -netdev user,id=n0 -device virtio-net-pci,netdev=n0 -display none -serial file:console.log -no-reboot
grep -a 'run-one-job' console.log
```

Expected: the process exits on its own within about 2 minutes and the log shows `[run-one-job ...] start; user=runner session=1`, `C: extended to 80 GB`, `runner exited rc=...` (non-zero, the blob is fake). Clean up with `rm -rf /tmp/ghq-clone`.

- [ ] **Step 5: Record the outcome** in the PR description (bake duration, clone boot-to-task time). No commit unless a fix was needed.

---

### Task 12: Manual checkpoint — real job on the runner host

**Files:** none. Needs the operator's GitHub App and a repository/org that may use self-hosted runners. Run on a host already running the service, after deploying the branch build.

- [ ] **Step 1: Deploy and bake**

Install the branch build, add a `win` pool (`labels: [self-hosted, windows, x64]`, `count: 1`, `memory_mb: 8192`, `disk_gb: 80`) to the host's config, run `sudo -u gh-runner github-qemu-runner setup` (all `ok`; OVMF found), then `sudo -u gh-runner github-qemu-runner refresh-image`, then restart the service.

- [ ] **Step 2: Run a workflow**

```yaml
name: windows-smoke
on: workflow_dispatch
jobs:
  smoke:
    runs-on: [self-hosted, windows, x64]
    steps:
      - uses: actions/checkout@v4
      - run: git --version
      - run: Get-CimInstance Win32_OperatingSystem | Select-Object Caption, BuildNumber
      - run: Get-Disk | Select-Object Number, BusType, Size
      - run: whoami; [Environment]::UserInteractive
```

Expected: the job succeeds; `checkout` uses git (no "git not found, falling back to API" warning); disk bus `SCSI` (virtio); `whoami` is `ghq-bake\runner`; interactive `True`. `journalctl -u github-qemu-runner` shows `runner online` then `VM exited` for the slot, and the workdir under `paths.run` is gone.

- [ ] **Step 3: Drain test**

Start a job that sleeps 3 minutes (`run: Start-Sleep 180`), then `sudo systemctl stop github-qemu-runner`. Expected: `waiting for busy runner to finish`, the job completes, `job finished during drain`, service stops. Start the service again.

- [ ] **Step 4: Open the PR**

```bash
git push -u origin feat/windows-runner
gh pr create --title "feat: Windows job VMs on the qemu backend" --body "$(cat <<'EOF'
Adds `os: windows` qemu pools backed by a baked Windows Server 2025 evaluation image (OVMF, virtio, Unattend.xml seed CD), per docs/superpowers/specs/2026-09-23-windows-runner-design.md.

Verification: unit tests in CI; manual bake and clone boot on a KVM host (Task 11); real job + drain on the runner host (Task 12) — results below.

<paste Task 11/12 outcomes>

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

---

## Self-review

**Spec coverage.** Configuration and validation → Task 1. OVMF discovery table → Task 1 (`ovmf.go`). Host prerequisites and `setup` → Tasks 9, 10. Images directory layout and conditional download → Tasks 4, 6. Bake flow steps 1–8 → Tasks 4, 5, 6 (answer file, bake.ps1 steps 1–8, host-side flatten). Job VM flow and `run-one-job.ps1` → Tasks 5, 8. Code-changes table → Tasks 1–10 one to one (`NIC` field dropped, noted under Global Constraints). Error handling: missing OVMF (Tasks 8, 9), no `BAKE-OK` (Task 6 test), download failure keeping a cached file (Task 6 `fetch`), missing JIT (Task 5 script). Testing list → every bullet has a test in Tasks 1–8; manual checkpoints → Tasks 11, 12.

**Placeholders.** None: every code step carries the code; the only "adjust" instructions are the mechanical import/caller updates named explicitly.

**Type consistency.** `qemu.Firmware{Code, Vars}`, `qemu.Disk{Path, Format}`, `Spec.DiskBus/SeedBus/CDROMs/ExtraDisks/HyperV/AllowReboot` (Task 2) are used with those names in Tasks 6 and 8. `CreateOverlay(ctx, base, backingFormat, dest, diskGB)` order is the same at all four call sites. `seed.BuildISOFiles(ctx, dir, volid, files)` matches Tasks 3, 6, 8. `LatestRunner(ctx, client, apiBase, platform, arch)` matches Tasks 4 and 6 and the docker caller. `config.OVMF{Code, Vars}` / `ResolveOVMF(dir)` match Tasks 1, 7, 8, 9. `imagebake.WindowsBase` constant matches Tasks 6, 7, 8, 9. `downloadMeta` field names match Tasks 4 and 6.
