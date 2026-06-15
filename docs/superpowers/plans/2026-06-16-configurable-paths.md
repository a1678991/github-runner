# Configurable image and run directories — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `paths.images` and `paths.run` config keys that override the
hard-coded `<state_dir>/{images,run}` directories so operators can put VM
disk images (base + per-VM overlays) on a separate filesystem.

**Architecture:** New nested `Paths` struct on `Config`. `applyDefaults`
fills missing fields from `StateDir`. `validate` requires absolute paths.
The `imagebake` and `dockerbackend` packages take an `ImageDir` field
instead of `StateDir`. The controller and `main.go` plumb
`cfg.Paths.Images` / `cfg.Paths.Run` through to their consumers.

**Tech Stack:** Go 1.x, `gopkg.in/yaml.v3`, standard library only.

**Spec:** `docs/superpowers/specs/2026-06-16-configurable-paths-design.md`

---

## File Structure

**Modified files:**

- `internal/config/config.go` — add `Paths` struct + field on `Config`, default and validate it.
- `internal/config/config_test.go` — new tests for defaults, override, env expansion, absolute-path validation.
- `internal/imagebake/bake.go` — rename `Options.StateDir` → `Options.ImageDir`; use the field directly instead of joining `"images"`. Update package comment.
- `internal/dockerbackend/bake.go` — rename `BakeOptions.StateDir` → `BakeOptions.ImageDir`; use the field directly. Update doc comment.
- `internal/dockerbackend/bake_test.go` — update tests to use `ImageDir`.
- `internal/controller/controller.go` — use `cfg.Paths.Run` and `cfg.Paths.Images`.
- `internal/controller/provision.go` — update `BasePath` comment.
- `cmd/github-qemu-runner/main.go` — pass `cfg.Paths.Images` as `ImageDir` to both bake calls in `runRefreshImage`; use `cfg.Paths.Images` in `runSetup`.
- `packaging/config.example.yaml` — add commented `paths:` block.
- `README.md` — document `paths.images`/`paths.run` in the "Top level" table; note operator responsibilities.

**No new files.**

---

## Task 1: Add `Paths` config field with defaults and validation

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/...`
Expected: compilation error (`c.Paths` undefined).

- [ ] **Step 3: Add the `Paths` struct and field**

Edit `internal/config/config.go`. Add the field to `Config`:

```go
type Config struct {
	GitHub   GitHub `yaml:"github"`
	StateDir string `yaml:"state_dir"`
	Paths    Paths  `yaml:"paths"`
	Docker   Docker `yaml:"docker"`
	Pools    []Pool `yaml:"pools"`
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
```

- [ ] **Step 4: Default and validate the new fields**

In `internal/config/config.go`, extend `applyDefaults` (after the existing
`StateDir` default block):

```go
c.Paths.Images = os.ExpandEnv(c.Paths.Images)
c.Paths.Run = os.ExpandEnv(c.Paths.Run)
if c.Paths.Images == "" {
	c.Paths.Images = filepath.Join(c.StateDir, "images")
}
if c.Paths.Run == "" {
	c.Paths.Run = filepath.Join(c.StateDir, "run")
}
```

In `validate`, before the pool loop, add:

```go
if !filepath.IsAbs(c.Paths.Images) {
	return fmt.Errorf("paths.images must be an absolute path")
}
if !filepath.IsAbs(c.Paths.Run) {
	return fmt.Errorf("paths.run must be an absolute path")
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/...`
Expected: all tests pass, including the five new ones.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
feat(config): add paths.images and paths.run overrides

Both fields default to <state_dir>/images and <state_dir>/run when empty.
Env vars expand the same way as github.private_key_path. Explicit values
must be absolute paths.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Use `ImageDir` in the imagebake package

**Files:**
- Modify: `internal/imagebake/bake.go`
- Modify: `cmd/github-qemu-runner/main.go` (caller — keeps the tree compiling)

`internal/imagebake/bake_test.go` does not currently call `Bake` with a
`StateDir`, so no test updates are needed in this task.

- [ ] **Step 1: Rename the field and update the body**

Edit `internal/imagebake/bake.go`. In the `Options` struct, replace
`StateDir string` with:

```go
ImageDir string
```

In `Bake`, replace the first non-comment line:

```go
imagesDir := filepath.Join(o.StateDir, "images")
```

with:

```go
imagesDir := o.ImageDir
```

Update the package-level doc comment on line 1-3 from:

```go
// Package imagebake builds the base VM image: Ubuntu 24.04 cloud image +
// Docker + actions-runner, flattened into <state>/images/base.qcow2 and
// swapped in atomically.
```

to:

```go
// Package imagebake builds the base VM image: Ubuntu 24.04 cloud image +
// Docker + actions-runner, flattened into <ImageDir>/base.qcow2 and
// swapped in atomically.
```

Update the `Bake` function comment on line 242-244 from:

```go
// Bake produces <state>/images/base.qcow2: download + verify the cloud
// image, resolve the runner release, boot a build overlay with the bake
// seed, require the BAKE-OK serial-console sentinel, flatten, swap.
```

to:

```go
// Bake produces <ImageDir>/base.qcow2: download + verify the cloud
// image, resolve the runner release, boot a build overlay with the bake
// seed, require the BAKE-OK serial-console sentinel, flatten, swap.
```

- [ ] **Step 2: Update the caller in main.go**

Edit `cmd/github-qemu-runner/main.go`. In `runRefreshImage`, change the
`imagebake.Options{...}` literal at lines 77-82 from:

```go
if err := imagebake.Bake(ctx, imagebake.Options{
	StateDir: cfg.StateDir,
	APIBase:  cfg.GitHub.APIBaseURL,
	QEMUBin:  qemuBin,
	Log:      log,
}); err != nil {
```

to:

```go
if err := imagebake.Bake(ctx, imagebake.Options{
	ImageDir: cfg.Paths.Images,
	APIBase:  cfg.GitHub.APIBaseURL,
	QEMUBin:  qemuBin,
	Log:      log,
}); err != nil {
```

- [ ] **Step 3: Run tests to verify nothing broke**

Run: `go build ./... && go test ./internal/imagebake/... ./cmd/...`
Expected: build succeeds, imagebake tests pass.

- [ ] **Step 4: Commit**

```bash
git add internal/imagebake/bake.go cmd/github-qemu-runner/main.go
git commit -m "$(cat <<'EOF'
refactor(imagebake): take ImageDir instead of StateDir

Options now names the images directory directly, fed from cfg.Paths.Images.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Use `ImageDir` in the dockerbackend package

**Files:**
- Modify: `internal/dockerbackend/bake.go`
- Modify: `internal/dockerbackend/bake_test.go`
- Modify: `cmd/github-qemu-runner/main.go`

- [ ] **Step 1: Update the failing tests first**

Edit `internal/dockerbackend/bake_test.go`. Three places reference
`StateDir:` in a `BakeOptions{...}` literal — rename each to `ImageDir:`.

In `TestBake` (around lines 44-51):

```go
stateDir := filepath.Join(dir, "state")
err := Bake(context.Background(), BakeOptions{
	StateDir:  stateDir,
	HTTP:      api.Client(),
	...
})
```

Replace with:

```go
imageDir := filepath.Join(dir, "images")
err := Bake(context.Background(), BakeOptions{
	ImageDir:  imageDir,
	HTTP:      api.Client(),
	...
})
```

Then update the read-back at line 77:

```go
b, err := os.ReadFile(filepath.Join(stateDir, "images", "docker-base.json"))
```

to:

```go
b, err := os.ReadFile(filepath.Join(imageDir, "docker-base.json"))
```

In `TestBakeBothVariants` (around lines 116-123), make the same `stateDir`
→ `imageDir` and `StateDir:` → `ImageDir:` replacements, and update the
read-back at line 142 similarly.

In `TestBakeRejectsUnknownVariant` (around lines 154-159):

```go
err := Bake(context.Background(), BakeOptions{
	StateDir:  t.TempDir(),
	...
})
```

to:

```go
err := Bake(context.Background(), BakeOptions{
	ImageDir:  t.TempDir(),
	...
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/dockerbackend/...`
Expected: compilation error (`ImageDir` undefined in `BakeOptions`).

- [ ] **Step 3: Rename the field and update the body**

Edit `internal/dockerbackend/bake.go`. In `BakeOptions` (around line 19-22),
rename `StateDir string` to:

```go
ImageDir string
```

In `Bake`, replace line 98:

```go
imagesDir := filepath.Join(o.StateDir, "images")
```

with:

```go
imagesDir := o.ImageDir
```

Update the doc comment on lines 46-50 from:

```go
// Bake builds the runner container image variants selected by o.Variants
// from the embedded Dockerfile, tagging Image (dind) and/or SlimImage
// (slim). The build runs natively on this host, so the image arch
// always matches (arm64 hosts get linux-arm64 runners). A provenance
// sidecar lands at <state>/images/docker-base.json, mirroring base.json.
```

to:

```go
// Bake builds the runner container image variants selected by o.Variants
// from the embedded Dockerfile, tagging Image (dind) and/or SlimImage
// (slim). The build runs natively on this host, so the image arch
// always matches (arm64 hosts get linux-arm64 runners). A provenance
// sidecar lands at <ImageDir>/docker-base.json, mirroring base.json.
```

- [ ] **Step 4: Update the caller in main.go**

Edit `cmd/github-qemu-runner/main.go`. In `runRefreshImage`, change the
`dockerbackend.BakeOptions{...}` literal at lines 98-104 from:

```go
if err := dockerbackend.Bake(ctx, dockerbackend.BakeOptions{
	StateDir:  cfg.StateDir,
	APIBase:   cfg.GitHub.APIBaseURL,
	DockerBin: dockerBin,
	Variants:  variants,
	Log:       log,
}); err != nil {
```

to:

```go
if err := dockerbackend.Bake(ctx, dockerbackend.BakeOptions{
	ImageDir:  cfg.Paths.Images,
	APIBase:   cfg.GitHub.APIBaseURL,
	DockerBin: dockerBin,
	Variants:  variants,
	Log:       log,
}); err != nil {
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/dockerbackend/... ./cmd/...`
Expected: build succeeds, dockerbackend tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/dockerbackend/bake.go internal/dockerbackend/bake_test.go cmd/github-qemu-runner/main.go
git commit -m "$(cat <<'EOF'
refactor(dockerbackend): take ImageDir instead of StateDir

BakeOptions now names the images directory directly, fed from
cfg.Paths.Images.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Wire `Paths.Run` and `Paths.Images` through the controller and setup check

**Files:**
- Modify: `internal/controller/controller.go`
- Modify: `internal/controller/provision.go` (comment only)
- Modify: `cmd/github-qemu-runner/main.go`

- [ ] **Step 1: Update the controller**

Edit `internal/controller/controller.go`. Replace line 34:

```go
runDir := filepath.Join(cfg.StateDir, "run")
```

with:

```go
runDir := cfg.Paths.Run
```

Replace line 45:

```go
basePath, err := filepath.Abs(filepath.Join(cfg.StateDir, "images", "base.qcow2"))
```

with:

```go
basePath, err := filepath.Abs(filepath.Join(cfg.Paths.Images, "base.qcow2"))
```

- [ ] **Step 2: Update the provision.go field comment**

Edit `internal/controller/provision.go`. On line 21, change:

```go
BasePath string // <state>/images/base.qcow2, absolute
```

to:

```go
BasePath string // <Paths.Images>/base.qcow2, absolute
```

- [ ] **Step 3: Update the setup base-image check**

Edit `cmd/github-qemu-runner/main.go`. In `runSetup`, change line 194:

```go
base := filepath.Join(cfg.StateDir, "images", "base.qcow2")
```

to:

```go
base := filepath.Join(cfg.Paths.Images, "base.qcow2")
```

- [ ] **Step 4: Run the full test suite**

Run: `go build ./... && go test ./...`
Expected: every package compiles and all tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/controller.go internal/controller/provision.go cmd/github-qemu-runner/main.go
git commit -m "$(cat <<'EOF'
feat(controller): honor paths.images and paths.run at startup

Controller now reads the run dir and base image location from
cfg.Paths.{Run,Images} instead of joining state_dir hard-coded subpaths.
The setup base-image check follows the same path.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Update example config and README

**Files:**
- Modify: `packaging/config.example.yaml`
- Modify: `README.md`

- [ ] **Step 1: Add a commented `paths:` block to the example config**

Edit `packaging/config.example.yaml`. After the existing
`# state_dir: /var/lib/github-qemu-runner` line (around line 12), insert:

```yaml

# Override per-concern subdirectories. Defaults to state_dir/images and
# state_dir/run when unset; both must be absolute paths if set. Useful
# for putting qcow2 files on a separate fast disk.
#paths:
#  images: /mnt/fast/github-qemu-runner/images
#  run:    /mnt/fast/github-qemu-runner/run
```

- [ ] **Step 2: Document the keys in the README "Top level" table**

Edit `README.md`. In the "Top level" table (starts around line 187), replace:

```markdown
| Key | Required | Default | Notes |
|---|---|---|---|
| `state_dir` | no | `/var/lib/github-qemu-runner` | Images, per-VM workdirs, runtime state |
| `docker.runtime` | no | `runsc` | Runtime for docker-backend job containers: `runsc` (gVisor) or `runc` (no sandbox — read the Docker backend section first) |
```

with:

```markdown
| Key | Required | Default | Notes |
|---|---|---|---|
| `state_dir` | no | `/var/lib/github-qemu-runner` | Base for the default `paths.*` directories; also holds anything outside the configurable paths |
| `paths.images` | no | `<state_dir>/images` | Absolute path. Holds `base.qcow2`, `base.json`, the cloud image download, the bake working dir, and `docker-base.json`. Operator must create + chown to the runner user when outside `<state_dir>` (systemd `StateDirectory=` does not cover it) |
| `paths.run` | no | `<state_dir>/run` | Absolute path. Holds per-VM workdirs (QEMU) and jit-config mount staging (Docker). Same ownership caveat as `paths.images` |
| `docker.runtime` | no | `runsc` | Runtime for docker-backend job containers: `runsc` (gVisor) or `runc` (no sandbox — read the Docker backend section first) |
```

- [ ] **Step 3: Update the Runbook image-provenance row**

Edit `README.md`. In the Runbook table (around line 391), replace:

```markdown
| Image provenance | `/var/lib/github-qemu-runner/images/base.json` (qemu), `/var/lib/github-qemu-runner/images/docker-base.json` (docker) |
```

with:

```markdown
| Image provenance | `<paths.images>/base.json` (qemu), `<paths.images>/docker-base.json` (docker); defaults to `/var/lib/github-qemu-runner/images/` |
```

And replace the per-VM console row above it:

```markdown
| Per-VM console | `/var/lib/github-qemu-runner/run/<vm>/console.log` (gone after teardown) |
```

with:

```markdown
| Per-VM console | `<paths.run>/<vm>/console.log` (gone after teardown); defaults to `/var/lib/github-qemu-runner/run/<vm>/console.log` |
```

- [ ] **Step 4: Verify build still passes**

Run: `go build ./... && go test ./...`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add packaging/config.example.yaml README.md
git commit -m "$(cat <<'EOF'
docs: describe paths.images and paths.run config keys

Example config gets a commented block; README documents the two new
top-level keys and the operator ownership caveat for custom paths.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Self-review

After completing all tasks, run a final check:

```bash
go build ./...
go test ./...
git log --oneline main..HEAD
```

Confirm the five commits exist, the tree builds, and every test passes.
