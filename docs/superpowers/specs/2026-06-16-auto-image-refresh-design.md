# Auto image refresh on service start + scheduled refresh timer

## Problem

When the `controller` starts, it fails fast if a required image is
absent:

- QEMU: `os.Stat(<images>/base.qcow2)` →
  `"base image missing (run \`github-qemu-runner refresh-image\` first)"`
  (`internal/controller/controller.go`).
- Docker: `docker image inspect <image>` per used variant →
  `"runner image %s missing (run \`github-qemu-runner refresh-image\` first)"`.

A fresh install therefore needs a manual `refresh-image` run before the
service will start. There is also no built-in way to keep images current
(actions/runner releases land every few weeks; Ubuntu cloud images update
periodically) — operators must script their own periodic `refresh-image`.

## Goals

- On controller start, automatically bake any **missing** image instead
  of failing, gated by a config flag that defaults to on.
- Bake only the artifacts that are actually missing (per backend / per
  docker variant), not everything.
- Ship a systemd timer that periodically runs `refresh-image`, **off by
  default**, with a schedule operators can override via systemd.
- Remove the duplication between `main.go`'s `runRefreshImage` and the
  controller's image-presence checks by giving "refresh the images" a
  single implementation.

## Non-goals

- Re-baking on **staleness** (provenance/version drift). Auto-refresh
  triggers only when an image is absent. Periodic freshness is handled by
  the opt-in timer, not by start-time logic.
- Enabling the timer by default, or auto-enabling any unit on package
  install (consistent with the controller unit, which install never
  enables).
- Changing the controller's fail behaviour: a bake that fails still
  causes the controller to exit with an error.

## Design

### Part 1 — Auto-refresh on controller start

#### Config schema

A new optional top-level `images:` block:

```yaml
images:
  auto_refresh: true   # default true; set false to restore fail-fast
```

```go
type Config struct {
    GitHub   GitHub `yaml:"github"`
    StateDir string `yaml:"state_dir"`
    Paths    Paths  `yaml:"paths"`
    Images   Images `yaml:"images"`
    Docker   Docker `yaml:"docker"`
    Pools    []Pool `yaml:"pools"`
}

type Images struct {
    // AutoRefresh bakes missing images on controller start instead of
    // failing. Pointer so an absent key defaults to true (bool's zero
    // value is false, which would invert the intended default).
    AutoRefresh *bool `yaml:"auto_refresh"`
}
```

In `applyDefaults()`: if `Images.AutoRefresh == nil`, set it to a pointer
to `true`. Validation: none needed.

#### `internal/imageprep` — single refresh implementation (Approach A)

A new package owns the bake orchestration currently inlined in
`main.go`'s `runRefreshImage` and overlapping with the controller's
presence checks:

```go
// Ensure bakes the images the configured pools require. With force, every
// selected artifact is rebaked; otherwise only missing artifacts are
// baked (os.Stat for base.qcow2, `docker image inspect` per variant).
func Ensure(ctx context.Context, cfg *config.Config, log *slog.Logger, force bool) error
```

Responsibilities:

- QEMU (when `cfg.HasBackend("qemu")`): look up `qemu-system-x86_64`. If
  `force` or `<images>/base.qcow2` is absent, call `imagebake.Bake`.
- Docker (when `cfg.HasBackend("docker")`): look up `docker`. Build the
  used-variant set — `dind` if `cfg.HasDockerIsolation("gvisor")`,
  `slim` if `cfg.HasDockerIsolation("seccomp")` (mirrors the existing
  mapping in `runRefreshImage` and `controller.Run`). If `force`, bake
  all used variants; otherwise bake only those whose image
  (`dockerbackend.Image` / `dockerbackend.SlimImage`) fails
  `docker image inspect`. Skip the `dockerbackend.Bake` call entirely
  when the to-bake set is empty.

Image directory and API base come from `cfg.Paths.Images` and
`cfg.GitHub.APIBaseURL`, exactly as `runRefreshImage` passes them today.

Imports: `imageprep` → `{config, imagebake, dockerbackend}`.
`dockerbackend` already imports `imagebake`; `imageprep` is imported only
by `main`, so no cycle.

#### Wiring

- `cmd/github-qemu-runner/main.go`
  - `runRefreshImage` becomes `return imageprep.Ensure(ctx, cfg, log, true)`.
    Its current per-backend bin lookups and bake calls move into
    `imageprep`.
  - `runController`: after `config.Load`, before `controller.Run`:

    ```go
    if cfg.Images.AutoRefresh != nil && *cfg.Images.AutoRefresh {
        if err := imageprep.Ensure(ctx, cfg, log, false); err != nil {
            return fmt.Errorf("auto image refresh: %w", err)
        }
    }
    return controller.Run(ctx, cfg, log)
    ```

- `internal/controller/controller.go` — **unchanged**. Its existing
  "image missing (run `refresh-image` first)" checks remain as the
  safety net for `auto_refresh: false` (and as a post-condition if a bake
  somehow does not produce the expected artifact).

This keeps auto-refresh at the command layer, alongside `refresh-image`,
so the `controller` package stays focused on supervision and gains no new
dependency.

#### Behaviour notes

- Auto-refresh runs while the controller process has already exec'd
  (`Type=exec`), so the bake delays pool startup but does not interact
  with `TimeoutStartSec` (which only governs `Type=oneshot`/`notify`).
- A bake under auto-refresh that fails returns an error from `Ensure`,
  the controller exits non-zero, and systemd's `Restart=on-failure`
  retries after `RestartSec` — the intended fail-fast-then-retry loop.

### Part 2 — systemd refresh timer

Two new units under `packaging/`, peers to
`github-qemu-runner.service`.

#### `github-qemu-runner-refresh.service` (`Type=oneshot`)

Mirrors the controller unit's identity and hardening, with two
deliberate differences:

- **No `LoadCredential`** — `refresh-image` resolves the latest
  actions/runner release from the public GitHub API and downloads the
  Ubuntu cloud image; it needs no GitHub App auth.
- **`TimeoutStartSec=30min`** — a bake (cloud-image download + VM boot +
  provisioning, or a docker build) far exceeds systemd's 90s
  `DefaultTimeoutStartSec`, which would otherwise SIGTERM the bake.

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
TimeoutStartSec=30min

# Hardening (mirrors the controller unit)
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
StateDirectory=github-qemu-runner
ReadWritePaths=/var/lib/github-qemu-runner
```

Note: the docker backend additionally requires the runner user to reach
the docker socket; that is existing host setup documented for the
controller, not something the unit grants. The `/usr/local/bin` path is
the manual-install target and is rewritten to `/usr/bin` by the deb/arch
packaging (see below), exactly like the controller unit.

#### `github-qemu-runner-refresh.timer`

```ini
[Unit]
Description=Periodically refresh github-qemu-runner images

[Timer]
OnCalendar=weekly
Persistent=true
RandomizedDelaySec=1h

[Install]
WantedBy=timers.target
```

- **Default cadence weekly**, with `Persistent=true` (run a missed timer
  on next boot) and `RandomizedDelaySec=1h` (avoid thundering-herd on
  upstream image hosts).
- **Not enabled by any installer.** Operators run
  `systemctl enable --now github-qemu-runner-refresh.timer`.
- **Schedule is operator-overridable** via
  `systemctl edit github-qemu-runner-refresh.timer` (drop-in setting a
  new `OnCalendar=`).

#### Packaging wiring

- `packaging/arch/PKGBUILD`: `install -Dm644` both units into
  `/usr/lib/systemd/system/`; extend the existing
  `/usr/local/bin` → `/usr/bin` `sed` to the refresh service. Timer needs
  no rewrite. No change to `postinst`-style enabling (Arch never enables).
- `packaging/deb/build.sh`: stage the refresh service with the same
  `sed` rewrite into `$staging/github-qemu-runner-refresh.service`
  (alongside the controller service it already stages).
- `packaging/deb/nfpm.yaml`: add `contents` entries for
  `${DEB_STAGING}/github-qemu-runner-refresh.service` (expand) →
  `/usr/lib/systemd/system/github-qemu-runner-refresh.service` and
  `packaging/github-qemu-runner-refresh.timer` (static) →
  `/usr/lib/systemd/system/github-qemu-runner-refresh.timer`. The
  existing `postinst` `daemon-reload` covers the new units; nothing is
  enabled.
- `nix/module.nix`: new options under `services.github-qemu-runner`:

  ```nix
  refresh.enable   = lib.mkEnableOption "periodic image refresh timer";  # default false
  refresh.schedule = lib.mkOption {
    type = lib.types.str;
    default = "weekly";
    description = "systemd OnCalendar expression for the refresh timer.";
  };
  ```

  When `refresh.enable`, define `systemd.services.github-qemu-runner-refresh`
  (oneshot; same `path`, `User`/`Group`/`SupplementaryGroups`, hardening,
  and `TimeoutStartSec` as above; `ExecStart` = `... refresh-image`; no
  `LoadCredential`) and `systemd.timers.github-qemu-runner-refresh`
  (`wantedBy = [ "timers.target" ]`, `timerConfig.OnCalendar =
  cfg.refresh.schedule`, `Persistent = true`). Here "configurable by
  systemd" is surfaced as the `refresh.schedule` module option.

## Testing

- `internal/config/config_test.go`:
  - `images.auto_refresh` absent → `Images.AutoRefresh` defaults to a
    non-nil pointer to `true`.
  - `auto_refresh: false` → pointer to `false` (distinguished from
    absent).

- `internal/imageprep/imageprep_test.go` (new), table-driven on a
  `*config.Config`, asserting the **decision** `Ensure` makes — which
  backends and which docker variants it would bake for a given
  `force` and set of already-present artifacts — without invoking real
  bakes. To make this testable, factor the selection into a pure helper
  (e.g. `plan(cfg, force, present) -> (bakeQEMU bool, variants []string)`)
  that `Ensure` consumes; tests target the helper. Cases:
  - qemu-only, base present, `force=false` → no bake.
  - qemu-only, base absent, `force=false` → bake qemu.
  - docker gvisor+seccomp, slim absent / dind present, `force=false` →
    bake `["slim"]` only.
  - any config, `force=true` → bake all selected artifacts regardless of
    presence.
  - empty to-bake set → `dockerbackend.Bake` not called.

- Packaging and nix changes are verified by building the package / `nix
  build`, not by Go unit tests.

## Documentation

- `README.md`: document `images.auto_refresh` (default on, what it does,
  when to disable) and a "scheduled refresh" subsection covering
  `systemctl enable --now github-qemu-runner-refresh.timer` and
  `systemctl edit` to change the schedule; for NixOS, the
  `refresh.enable` / `refresh.schedule` options.
- `packaging/config.example.yaml`: add the `images:` block with
  `auto_refresh` documented.

## Open questions

None outstanding — design approved 2026-06-16 (weekly default cadence
confirmed).
