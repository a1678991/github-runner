# Configurable image and run directories

## Problem

VM disk images (`base.qcow2`, per-VM `overlay.qcow2`) and supporting
artifacts currently live under hard-coded subdirectories of `state_dir`:

- `<state_dir>/images/` — baked base image, provenance metadata, cloud
  image download, bake working dir, `docker-base.json` sidecar.
- `<state_dir>/run/<vm>/` — per-VM overlay, seed ISO, console log, QMP
  socket, PID file (QEMU); jit-config mount staging (Docker).

Operators who want qcow2 files on a fast scratch disk separate from
`state_dir` (e.g. NVMe for overlays, HDD for everything else) have no
knob short of bind-mounts at the filesystem layer.

## Goals

- Let the operator override the location of the images directory and the
  run directory independently, while keeping the existing single-disk
  default behaviour.
- Keep one setting per concept — no per-file or per-backend overrides.

## Non-goals

- Auto-migrating data from the old locations on first launch.
- Touching the systemd unit's `StateDirectory=` — that continues to
  manage only `/var/lib/github-qemu-runner`.
- Splitting per-VM bundles across two filesystems (overlay on disk A,
  seed ISO on disk B).

## Design

### Config schema

A new optional `paths:` block, peer to `state_dir`:

```yaml
state_dir: /var/lib/github-qemu-runner
paths:
  images: /mnt/fast/images   # optional; default: <state_dir>/images
  run:    /mnt/fast/run      # optional; default: <state_dir>/run
```

```go
type Config struct {
    GitHub   GitHub `yaml:"github"`
    StateDir string `yaml:"state_dir"`
    Paths    Paths  `yaml:"paths"`
    Docker   Docker `yaml:"docker"`
    Pools    []Pool `yaml:"pools"`
}

type Paths struct {
    Images string `yaml:"images"`
    Run    string `yaml:"run"`
}
```

### Defaults and expansion

In `applyDefaults()`:

- `os.ExpandEnv` is applied to both fields before defaulting, so
  `${VAR}/images` works the same way `private_key_path` already does.
- If `Paths.Images` is empty after expansion, set it to
  `filepath.Join(StateDir, "images")`.
- If `Paths.Run` is empty after expansion, set it to
  `filepath.Join(StateDir, "run")`.

### Validation

In `validate()`:

- When set (non-empty after expansion), both `Paths.Images` and
  `Paths.Run` MUST be absolute paths. Relative paths against a
  systemd-launched daemon resolve against an unpredictable cwd; reject
  at load time with a clear error. This matches the rule already applied
  to `seccomp_profile`.
- No existence check — the controller already errors at startup when
  `base.qcow2` is missing, and `bake` creates directories as needed.

### What moves with each path

| Path           | Contents                                                                                                                          |
| -------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `paths.images` | `base.qcow2`, `base.json`, downloaded cloud image (e.g. `noble-server-cloudimg-amd64.img`), `bake/` working dir, `docker-base.json` |
| `paths.run`    | Per-VM bundles: `<run>/<vm>/{overlay.qcow2, seed ISO, console.log, qmp.sock, qemu.pid}` (QEMU) and jit-config mount staging (Docker) |

`docker-base.json` is a tiny provenance sidecar (kilobytes); it moves
with the rest of the bake output so "everything `refresh-image` writes
lives under `paths.images`" stays true.

### Code touchpoints

- `internal/config/config.go`
  - Add `Paths Paths` field on `Config`.
  - In `applyDefaults`: env-expand, then derive defaults from
    `StateDir`.
  - In `validate`: require absolute paths when set.

- `internal/imagebake/bake.go`
  - Rename `Options.StateDir` → `Options.ImageDir`.
  - Replace `imagesDir := filepath.Join(o.StateDir, "images")` with
    `imagesDir := o.ImageDir`.
  - `bake/` subdir continues to live under `imagesDir`.

- `internal/dockerbackend/bake.go`
  - Rename `Options.StateDir` → `Options.ImageDir`.
  - Replace `imagesDir := filepath.Join(o.StateDir, "images")` with
    `imagesDir := o.ImageDir`.

- `internal/controller/controller.go`
  - `runDir := filepath.Join(cfg.StateDir, "run")` →
    `runDir := cfg.Paths.Run`.
  - `basePath := filepath.Join(cfg.StateDir, "images", "base.qcow2")` →
    `basePath := filepath.Join(cfg.Paths.Images, "base.qcow2")`.

- `cmd/github-qemu-runner/main.go`
  - `runRefreshImage` passes `cfg.Paths.Images` as `ImageDir` to both
    bake options.
  - `runSetup` checks `filepath.Join(cfg.Paths.Images, "base.qcow2")`.

### Migration

- No automatic data move. Operators who set `paths.images` or
  `paths.run` after an existing install must either:
  - re-run `github-qemu-runner refresh-image` to rebuild the base
    image at the new location, and/or
  - manually move `<state_dir>/{images,run}` to the new locations.
- The controller's existing "base image missing" startup error is the
  surface for "you reconfigured but didn't re-bake".

### Operator responsibilities for custom paths

Documented in the README, not coded:

- systemd `StateDirectory=` only auto-creates and `chown`s
  `/var/lib/github-qemu-runner`. Custom `paths.images`/`paths.run`
  outside that tree must be created with appropriate ownership for the
  runner user before starting the unit.
- SELinux/AppArmor contexts on custom mount points are the operator's
  problem (same model as `seccomp_profile`).

## Testing

- `internal/config/config_test.go`:
  - Defaults: when `paths:` is absent, `Paths.Images` and `Paths.Run`
    derive from `state_dir`.
  - Override: when set, values pass through verbatim.
  - Env expansion: `${VAR}/images` resolves at load time.
  - Validation: relative `paths.images` is rejected with a clear error;
    same for `paths.run`.

- `internal/imagebake/bake_test.go` and
  `internal/dockerbackend/bake_test.go`:
  - Update field name from `StateDir` to `ImageDir`. Existing tests
    already use `t.TempDir()` and exercise the join behaviour — the
    rename is the only change.

- `internal/controller/provision_test.go`:
  - No change. Already uses constructed paths, not `cfg.StateDir`.

## Documentation

- `README.md` and `packaging/arch/src/github-qemu-runner-git/README.md`:
  - Add a "disk layout" subsection describing `paths.images` and
    `paths.run`, defaults, the absolute-path requirement, and the
    systemd ownership caveat.
- `packaging/.../README.md` references to
  `/var/lib/github-qemu-runner/images/base.qcow2` updated to note that
  the location is configurable.

## Open questions

None outstanding — design approved 2026-06-16.
