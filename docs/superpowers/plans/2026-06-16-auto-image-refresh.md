# Auto Image Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bake missing base/runner images automatically on controller start (config-gated, default on), and ship an off-by-default systemd timer that periodically runs `refresh-image`.

**Architecture:** A new `internal/imageprep` package centralizes "bake the images the pools need" behind `Ensure(ctx, cfg, log, force)`, consumed by both `refresh-image` (force=all) and the controller startup path (force=false, missing-only). The controller package is unchanged — its existing "image missing" checks stay as the fail-fast path for `auto_refresh: false`. Packaging (arch/deb/nix) gains a `refresh.service` + `refresh.timer` pair, disabled by default.

**Tech Stack:** Go 1.x (stdlib only: `os/exec`, `os`, `path/filepath`, `log/slog`), systemd units, nfpm (deb), PKGBUILD (arch), NixOS module.

---

## File Structure

**Create:**
- `internal/imageprep/imageprep.go` — `Ensure` + pure `plan` helper.
- `internal/imageprep/imageprep_test.go` — table tests for `plan`.
- `packaging/github-qemu-runner-refresh.service` — oneshot unit running `refresh-image`.
- `packaging/github-qemu-runner-refresh.timer` — weekly trigger, not enabled.

**Modify:**
- `internal/config/config.go` — `Images` struct + `Config.Images` field + default.
- `internal/config/config_test.go` — `auto_refresh` default/override tests.
- `cmd/github-qemu-runner/main.go` — route `runRefreshImage` + `runController` through `imageprep`.
- `packaging/config.example.yaml` — document `images.auto_refresh`.
- `packaging/arch/PKGBUILD` — install + path-rewrite the two new units.
- `packaging/deb/build.sh` — stage the refresh service with the path rewrite.
- `packaging/deb/nfpm.yaml` — ship the two new units.
- `nix/module.nix` — `refresh.enable` / `refresh.schedule` options + conditional units.
- `README.md` — `images.auto_refresh` row, "Scheduled image refresh" section, NixOS note, runbook row.

---

## Task 1: Config — `images.auto_refresh`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run TestAutoRefresh -v`
Expected: compile failure — `c.Images undefined (type Config has no field or method Images)`.

- [ ] **Step 3: Add the `Images` field and type**

In `internal/config/config.go`, change the `Config` struct to add the `Images` field (insert after the `Paths` line):

```go
type Config struct {
	GitHub   GitHub `yaml:"github"`
	StateDir string `yaml:"state_dir"`
	Paths    Paths  `yaml:"paths"`
	Images   Images `yaml:"images"`
	Docker   Docker `yaml:"docker"`
	Pools    []Pool `yaml:"pools"`
}
```

Add the `Images` type immediately after the `Paths` struct definition (just before `type GitHub struct`):

```go
// Images configures base/runner image lifecycle.
type Images struct {
	// AutoRefresh bakes missing images on controller start instead of
	// failing. Pointer so an absent key defaults to true (a plain bool's
	// zero value is false, which would invert the intended default).
	AutoRefresh *bool `yaml:"auto_refresh"`
}
```

- [ ] **Step 4: Apply the default**

In `applyDefaults()`, add after the `c.Paths.Run` defaulting block (after the `if c.Paths.Run == "" { ... }` block, around line 136):

```go
	if c.Images.AutoRefresh == nil {
		on := true
		c.Images.AutoRefresh = &on
	}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/ -run TestAutoRefresh -v`
Expected: PASS (both tests).

- [ ] **Step 6: Run the full config package tests**

Run: `go test ./internal/config/`
Expected: `ok` (no regressions — `dec.KnownFields(true)` now accepts the `images` key).

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add images.auto_refresh (default on)"
```

---

## Task 2: `internal/imageprep` package

The pure `plan` helper makes the bake decision testable without running real bakes; `Ensure` wires it to the filesystem / docker / bake calls.

**Files:**
- Create: `internal/imageprep/imageprep.go`
- Test: `internal/imageprep/imageprep_test.go`

- [ ] **Step 1: Write the failing test for `plan`**

Create `internal/imageprep/imageprep_test.go`:

```go
package imageprep

import (
	"slices"
	"testing"

	"github.com/a1678991/github-qemu-runner/internal/config"
)

func TestPlan(t *testing.T) {
	qemu := config.Pool{Backend: "qemu"}
	dind := config.Pool{Backend: "docker", Isolation: "gvisor"}
	slim := config.Pool{Backend: "docker", Isolation: "seccomp"}

	cases := []struct {
		name         string
		pools        []config.Pool
		force        bool
		present      map[string]bool
		wantQEMU     bool
		wantVariants []string
	}{
		{"qemu present, no force", []config.Pool{qemu}, false, map[string]bool{"qemu": true}, false, nil},
		{"qemu absent, no force", []config.Pool{qemu}, false, map[string]bool{"qemu": false}, true, nil},
		{"docker slim missing", []config.Pool{dind, slim}, false, map[string]bool{"dind": true, "slim": false}, false, []string{"slim"}},
		{"all present, no force", []config.Pool{qemu, dind}, false, map[string]bool{"qemu": true, "dind": true}, false, nil},
		{"force bakes everything used", []config.Pool{qemu, dind, slim}, true, map[string]bool{}, true, []string{"dind", "slim"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.Config{Pools: tc.pools}
			got := plan(c, tc.force, func(a string) bool { return tc.present[a] })
			if got.QEMU != tc.wantQEMU {
				t.Errorf("QEMU = %v, want %v", got.QEMU, tc.wantQEMU)
			}
			if !slices.Equal(got.Variants, tc.wantVariants) {
				t.Errorf("Variants = %v, want %v", got.Variants, tc.wantVariants)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/imageprep/ -v`
Expected: build failure — `package github.com/a1678991/github-qemu-runner/internal/imageprep: no Go files` / `undefined: plan`.

- [ ] **Step 3: Write `imageprep.go`**

Create `internal/imageprep/imageprep.go`:

```go
// Package imageprep bakes the base/runner images the configured pools
// need. It backs both the `refresh-image` command (force: rebake all)
// and the controller's start-time auto-refresh (missing-only).
package imageprep

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/dockerbackend"
	"github.com/a1678991/github-qemu-runner/internal/imagebake"
)

// imagePlan is the set of artifacts to bake.
type imagePlan struct {
	QEMU     bool
	Variants []string // subset of {"dind","slim"}, in that order
}

// plan decides what to bake. present reports whether a given artifact is
// already on disk: "qemu" (base.qcow2), "dind"/"slim" (docker images).
// With force, presence is ignored and every used artifact is selected.
func plan(cfg *config.Config, force bool, present func(artifact string) bool) imagePlan {
	var p imagePlan
	if cfg.HasBackend("qemu") && (force || !present("qemu")) {
		p.QEMU = true
	}
	if cfg.HasBackend("docker") {
		for _, v := range []struct{ name, mode string }{
			{"dind", "gvisor"},
			{"slim", "seccomp"},
		} {
			if !cfg.HasDockerIsolation(v.mode) {
				continue
			}
			if force || !present(v.name) {
				p.Variants = append(p.Variants, v.name)
			}
		}
	}
	return p
}

// Ensure bakes the images the configured pools require. With force every
// used artifact is rebaked; otherwise only the missing ones.
func Ensure(ctx context.Context, cfg *config.Config, log *slog.Logger, force bool) error {
	var qemuBin, dockerBin string
	var err error
	if cfg.HasBackend("qemu") {
		if qemuBin, err = exec.LookPath("qemu-system-x86_64"); err != nil {
			return fmt.Errorf("qemu-system-x86_64 not found: %w", err)
		}
	}
	if cfg.HasBackend("docker") {
		if dockerBin, err = exec.LookPath("docker"); err != nil {
			return fmt.Errorf("docker not found: %w", err)
		}
	}

	present := func(artifact string) bool {
		switch artifact {
		case "qemu":
			_, statErr := os.Stat(filepath.Join(cfg.Paths.Images, "base.qcow2"))
			return statErr == nil
		case "dind":
			return exec.CommandContext(ctx, dockerBin, "image", "inspect", dockerbackend.Image).Run() == nil
		case "slim":
			return exec.CommandContext(ctx, dockerBin, "image", "inspect", dockerbackend.SlimImage).Run() == nil
		}
		return false
	}

	p := plan(cfg, force, present)

	if p.QEMU {
		if err := imagebake.Bake(ctx, imagebake.Options{
			ImageDir: cfg.Paths.Images,
			APIBase:  cfg.GitHub.APIBaseURL,
			QEMUBin:  qemuBin,
			Log:      log,
		}); err != nil {
			return err
		}
	}
	if len(p.Variants) > 0 {
		if err := dockerbackend.Bake(ctx, dockerbackend.BakeOptions{
			ImageDir:  cfg.Paths.Images,
			APIBase:   cfg.GitHub.APIBaseURL,
			DockerBin: dockerBin,
			Variants:  p.Variants,
			Log:       log,
		}); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/imageprep/ -v`
Expected: PASS (all `TestPlan` subtests).

- [ ] **Step 5: Build and vet to confirm no import cycle**

Run: `go build ./... && go vet ./internal/imageprep/`
Expected: no output (success). `imageprep` imports `config`/`imagebake`/`dockerbackend` only; nothing imports `imageprep` yet, so no cycle.

- [ ] **Step 6: Commit**

```bash
git add internal/imageprep/
git commit -m "feat(imageprep): add Ensure with missing-only/force bake planning"
```

---

## Task 3: Wire `imageprep` into the CLI

`refresh-image` delegates to `Ensure(force=true)`; `controller` runs `Ensure(force=false)` before supervising when `auto_refresh` is on. `controller.Run` is left untouched as the fail-fast safety net.

**Files:**
- Modify: `cmd/github-qemu-runner/main.go`

- [ ] **Step 1: Update imports**

In `cmd/github-qemu-runner/main.go`, remove the `imagebake` import and add the `imageprep` import. The import block's project imports become:

```go
	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/controller"
	"github.com/a1678991/github-qemu-runner/internal/dockerbackend"
	"github.com/a1678991/github-qemu-runner/internal/github"
	"github.com/a1678991/github-qemu-runner/internal/imageprep"
```

(`dockerbackend` stays — `runSetup` still references `dockerbackend.Image`/`SlimImage`. `imagebake` is dropped — only `runRefreshImage` used it.)

- [ ] **Step 2: Replace `runController`**

Replace the whole `runController` function with:

```go
func runController(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.Images.AutoRefresh != nil && *cfg.Images.AutoRefresh {
		if err := imageprep.Ensure(ctx, cfg, log, false); err != nil {
			return fmt.Errorf("auto image refresh: %w", err)
		}
	}
	return controller.Run(ctx, cfg, log)
}
```

- [ ] **Step 3: Replace `runRefreshImage`**

Replace the whole `runRefreshImage` function (the one that currently does the per-backend bin lookups and `imagebake.Bake`/`dockerbackend.Bake` calls) with:

```go
func runRefreshImage(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	return imageprep.Ensure(ctx, cfg, log, true)
}
```

- [ ] **Step 4: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no output. If vet reports `"os/exec" imported and not used` or similar, confirm `runSetup` still uses `exec`/`filepath`/`runtime`/`strings` (it does) — those imports stay; only `imagebake` should have been removed.

- [ ] **Step 5: Run the full test suite**

Run: `go test ./...`
Expected: all packages `ok`.

- [ ] **Step 6: Commit**

```bash
git add cmd/github-qemu-runner/main.go
git commit -m "feat: auto-refresh missing images on controller start"
```

---

## Task 4: Document `images.auto_refresh` in the config example

**Files:**
- Modify: `packaging/config.example.yaml`

- [ ] **Step 1: Add the `images` block**

In `packaging/config.example.yaml`, insert after the `#paths:` block (the three commented `paths` lines) and before the `# Docker backend` comment:

```yaml
# Image auto-refresh. When the controller starts and a required base/runner
# image is missing, bake it automatically instead of failing. Set to false
# to restore fail-fast (run `refresh-image` manually first).
#images:
#  auto_refresh: true
```

- [ ] **Step 2: Sanity-check the YAML parses**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('packaging/config.example.yaml'))" && echo OK`
Expected: `OK` (commented lines are ignored, but this confirms no stray indentation broke the file).

- [ ] **Step 3: Commit**

```bash
git add packaging/config.example.yaml
git commit -m "docs(config): document images.auto_refresh in the example"
```

---

## Task 5: systemd refresh units

**Files:**
- Create: `packaging/github-qemu-runner-refresh.service`
- Create: `packaging/github-qemu-runner-refresh.timer`

- [ ] **Step 1: Create the oneshot service**

Create `packaging/github-qemu-runner-refresh.service`:

```ini
[Unit]
Description=Refresh github-qemu-runner base/runner images
Documentation=https://github.com/a1678991/github-qemu-runner
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=gh-runner
Group=gh-runner
SupplementaryGroups=kvm
ExecStart=/usr/local/bin/github-qemu-runner -config /etc/github-qemu-runner/config.yaml refresh-image
# A bake (cloud-image download + VM boot, or a docker build) far exceeds
# systemd's 90s default start timeout, which would otherwise kill it.
TimeoutStartSec=30min

# Hardening mirrors github-qemu-runner.service. No LoadCredential: the
# image bake hits only public GitHub release / cloud-image endpoints and
# needs no GitHub App auth.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
StateDirectory=github-qemu-runner
ReadWritePaths=/var/lib/github-qemu-runner
```

(No `[Install]` section — the unit is started by the timer, not enabled directly.)

- [ ] **Step 2: Create the timer**

Create `packaging/github-qemu-runner-refresh.timer`:

```ini
[Unit]
Description=Periodically refresh github-qemu-runner images
Documentation=https://github.com/a1678991/github-qemu-runner

[Timer]
OnCalendar=weekly
Persistent=true
RandomizedDelaySec=1h

[Install]
WantedBy=timers.target
```

- [ ] **Step 3: Sanity-check unit syntax**

Run: `systemd-analyze verify packaging/github-qemu-runner-refresh.timer`
Expected: no output (timer is valid). Then:
Run: `systemd-analyze verify packaging/github-qemu-runner-refresh.service`
Expected: warnings about the missing binary (`/usr/local/bin/...`), unknown user `gh-runner` are normal in a dev checkout — the goal is to catch directive typos, not a clean pass. If `systemd-analyze` is unavailable, skip this step.

- [ ] **Step 4: Commit**

```bash
git add packaging/github-qemu-runner-refresh.service packaging/github-qemu-runner-refresh.timer
git commit -m "feat(packaging): add image refresh systemd service + timer"
```

---

## Task 6: Arch PKGBUILD — install the new units

**Files:**
- Modify: `packaging/arch/PKGBUILD`

- [ ] **Step 1: Install both units and extend the path rewrite**

In `packaging/arch/PKGBUILD`, in `package()`, replace this block:

```bash
  install -Dm644 packaging/github-qemu-runner.service \
    "$pkgdir/usr/lib/systemd/system/github-qemu-runner.service"
  # The shared unit targets the manual-install path (/usr/local/bin); the
  # packaged binary lives in /usr/bin.
  sed -i 's|/usr/local/bin/github-qemu-runner|/usr/bin/github-qemu-runner|' \
    "$pkgdir/usr/lib/systemd/system/github-qemu-runner.service"
```

with:

```bash
  install -Dm644 packaging/github-qemu-runner.service \
    "$pkgdir/usr/lib/systemd/system/github-qemu-runner.service"
  install -Dm644 packaging/github-qemu-runner-refresh.service \
    "$pkgdir/usr/lib/systemd/system/github-qemu-runner-refresh.service"
  install -Dm644 packaging/github-qemu-runner-refresh.timer \
    "$pkgdir/usr/lib/systemd/system/github-qemu-runner-refresh.timer"
  # The shared units target the manual-install path (/usr/local/bin); the
  # packaged binary lives in /usr/bin.
  sed -i 's|/usr/local/bin/github-qemu-runner|/usr/bin/github-qemu-runner|' \
    "$pkgdir/usr/lib/systemd/system/github-qemu-runner.service" \
    "$pkgdir/usr/lib/systemd/system/github-qemu-runner-refresh.service"
```

- [ ] **Step 2: Sanity-check the script**

Run: `shellcheck -s bash packaging/arch/PKGBUILD || true`
Expected: no new errors introduced by the edit (PKGBUILD uses pacman globals; pre-existing SC warnings, if any, are unchanged). A full `makepkg` build happens in CI, not here.

- [ ] **Step 3: Commit**

```bash
git add packaging/arch/PKGBUILD
git commit -m "feat(packaging): install refresh units in the Arch package"
```

---

## Task 7: Debian package — stage and ship the new units

**Files:**
- Modify: `packaging/deb/build.sh`
- Modify: `packaging/deb/nfpm.yaml`

- [ ] **Step 1: Stage the refresh service with the path rewrite**

In `packaging/deb/build.sh`, after the existing controller-service `sed` block (the one writing `$staging/github-qemu-runner.service`), add:

```bash
# Same /usr/local/bin -> /usr/bin rewrite for the refresh service. The
# timer carries no exec path, so nfpm ships it verbatim from packaging/.
sed 's|/usr/local/bin/github-qemu-runner|/usr/bin/github-qemu-runner|' \
  packaging/github-qemu-runner-refresh.service >"$staging/github-qemu-runner-refresh.service"
```

- [ ] **Step 2: Add the units to nfpm contents**

In `packaging/deb/nfpm.yaml`, after the controller-service `contents` entry (the `src: ${DEB_STAGING}/github-qemu-runner.service` block), add:

```yaml
  - src: ${DEB_STAGING}/github-qemu-runner-refresh.service
    dst: /usr/lib/systemd/system/github-qemu-runner-refresh.service
    expand: true
  - src: packaging/github-qemu-runner-refresh.timer
    dst: /usr/lib/systemd/system/github-qemu-runner-refresh.timer
```

(`postinst.sh` already runs `systemctl daemon-reload`; nothing is enabled — operators opt in. No script change needed.)

- [ ] **Step 3: Sanity-check the edits**

Run: `shellcheck packaging/deb/build.sh`
Expected: no errors.
Run: `python3 -c "import yaml; yaml.safe_load(open('packaging/deb/nfpm.yaml'))" && echo OK`
Expected: `OK` (the `${...}` placeholders are plain strings to the YAML parser).

- [ ] **Step 4: Commit**

```bash
git add packaging/deb/build.sh packaging/deb/nfpm.yaml
git commit -m "feat(packaging): ship refresh units in the Debian package"
```

---

## Task 8: NixOS module — `refresh.enable` / `refresh.schedule`

**Files:**
- Modify: `nix/module.nix`

- [ ] **Step 1: Add the options**

In `nix/module.nix`, inside `options.services.github-qemu-runner = { ... }`, add after the `privateKeyFile` option (before the closing `};` of the options block):

```nix
    refresh = {
      enable = lib.mkEnableOption "periodic image refresh via a systemd timer";
      schedule = lib.mkOption {
        type = lib.types.str;
        default = "weekly";
        example = "daily";
        description = ''
          systemd OnCalendar expression for the image refresh timer.
          Only used when refresh.enable is true.
        '';
      };
    };
```

- [ ] **Step 2: Add the conditional service + timer**

In `nix/module.nix`, inside `config = lib.mkIf cfg.enable { ... }`, add after the `systemd.services.github-qemu-runner = { ... };` block (before the closing `};` of `config`):

```nix
    systemd.services.github-qemu-runner-refresh = lib.mkIf cfg.refresh.enable {
      description = "Refresh github-qemu-runner base/runner images";
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      path = [
        pkgs.qemu_kvm
        pkgs.cdrkit
      ];
      serviceConfig = {
        Type = "oneshot";
        User = "gh-runner";
        Group = "gh-runner";
        SupplementaryGroups = [ "kvm" ];
        # No LoadCredential: the bake needs no GitHub App auth.
        ExecStart = "${lib.getExe cfg.package} -config ${configFile} refresh-image";
        TimeoutStartSec = "30min";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        StateDirectory = "github-qemu-runner";
        ReadWritePaths = [ "/var/lib/github-qemu-runner" ];
      };
    };

    systemd.timers.github-qemu-runner-refresh = lib.mkIf cfg.refresh.enable {
      description = "Periodically refresh github-qemu-runner images";
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnCalendar = cfg.refresh.schedule;
        Persistent = true;
        RandomizedDelaySec = "1h";
      };
    };
```

- [ ] **Step 3: Parse-check the module**

Run: `nix-instantiate --parse nix/module.nix >/dev/null && echo OK`
Expected: `OK`. If `nix` is unavailable, skip — the repo's lefthook `nixfmt` hook will format/validate on commit, and `nix flake check` runs in CI.

- [ ] **Step 4: Commit**

```bash
git add nix/module.nix
git commit -m "feat(nix): add refresh.enable/refresh.schedule timer options"
```

---

## Task 9: README documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add the config-table row**

In `README.md`, in the `### Top level` table, add a row after the `docker.runtime` row:

```markdown
| `images.auto_refresh` | no | `true` | When the controller starts and a required image is missing, bake it instead of failing. Set `false` to restore fail-fast (`refresh-image` must be run manually first) |
```

- [ ] **Step 2: Add a "Scheduled image refresh" section**

In `README.md`, immediately after the `## Commands` section (after the commands table, before `## Install (manual)`), add:

```markdown
## Scheduled image refresh

`images.auto_refresh` only bakes images that are *missing* at controller
start. To keep images current (new actions/runner or Ubuntu releases),
enable the bundled timer — shipped by the Arch/Debian packages, **off by
default**:

```bash
sudo systemctl enable --now github-qemu-runner-refresh.timer
```

It runs `refresh-image` weekly. Change the schedule with a drop-in:

```bash
sudo systemctl edit github-qemu-runner-refresh.timer
# [Timer]
# OnCalendar=daily
```

Running VMs/containers are unaffected; new ones pick up the rebaked image.
On NixOS use the module options instead (see Install (NixOS)).
```

- [ ] **Step 3: Add the NixOS note**

In `README.md`, in the `## Install (NixOS)` section, after the closing ``` of the `services.github-qemu-runner` example block (the line before "The module wires the key via systemd `LoadCredential`"), add:

```markdown
To enable the periodic image-refresh timer (off by default), add:

```nix
services.github-qemu-runner.refresh = {
  enable = true;
  schedule = "daily"; # any systemd OnCalendar value; default "weekly"
};
```
```

- [ ] **Step 4: Add a runbook row**

In `README.md`, in the `## Runbook` table, add a row after the `Refresh base image` row:

```markdown
| Scheduled refresh | enable `github-qemu-runner-refresh.timer` (off by default; weekly) — see "Scheduled image refresh" |
```

- [ ] **Step 5: Verify rendering**

Run: `grep -n "auto_refresh\|Scheduled image refresh\|refresh.timer" README.md`
Expected: matches in the Top-level table, the new section heading, and the runbook row.

- [ ] **Step 6: Commit**

```bash
git add README.md
git commit -m "docs: document auto_refresh and the scheduled refresh timer"
```

---

## Final verification

- [ ] **Run the whole suite + build:** `go test ./... && go build ./... && go vet ./...` → all `ok`, no output from build/vet.
- [ ] **Confirm fail-fast still works with the flag off:** review that `controller.Run` retains its `base image missing` / `runner image ... missing` checks (it is unmodified) — these now fire only when `auto_refresh: false`.

---

## Self-Review notes

- **Spec coverage:** Part 1 config → Task 1; `imageprep`/Approach A → Task 2; CLI wiring (both entry points, controller unchanged) → Task 3; config.example → Task 4; units (oneshot, TimeoutStartSec, no LoadCredential, weekly timer not enabled) → Task 5; arch → Task 6; deb → Task 7; nix options → Task 8; README (auto_refresh, timer, nix, runbook) → Task 9. Testing strategy (pure `plan`, config defaults) → Tasks 1–2.
- **Type consistency:** `Images.AutoRefresh *bool`, `imagePlan{QEMU bool, Variants []string}`, `plan(cfg, force, present)`, `Ensure(ctx, cfg, log, force)` used identically across Tasks 1–3 and the tests.
- **No placeholders:** every code/edit step shows full content; verification commands have expected output.
