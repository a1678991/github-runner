# Docker Backend (gVisor) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a per-pool `backend: docker` that runs each ephemeral GitHub Actions job in a gVisor-sandboxed Docker container (with DinD inside the sandbox), for hosts without `/dev/kvm`.

**Architecture:** A new `internal/dockerbackend` package implements the existing `pool.Provisioner`/`pool.VM` interfaces by shelling out to the `docker` CLI (mirroring how `internal/qemu` shells out to qemu binaries). The slot loop, GitHub client, and drain logic are untouched. `refresh-image` gains a `docker build` mode from an embedded Dockerfile; `setup` and the controller gain per-backend preflights.

**Tech Stack:** Go 1.x (stdlib only — no Docker SDK), docker CLI, gVisor runsc, embedded Dockerfile + bash entrypoint.

**Spec:** `docs/superpowers/specs/2026-06-11-docker-backend-design.md`

**Branch:** `feature/docker-backend` (already created; spec committed on it).

**Conventions you must follow** (from the existing codebase):
- Shell out with `exec.CommandContext(ctx, ...)` / `CombinedOutput()`, wrap errors as `fmt.Errorf("docker run %s: %v: %s", name, err, out)`.
- Tests live next to code, same package (white-box), use `t.TempDir()` and fake binaries (shell scripts) instead of real docker/qemu.
- Run tests with `go test ./internal/<pkg>/ -run TestName -v`. Lint runs via lefthook on commit; `gofmt` your code.
- Commits are conventional-commit style (commitlint enforces it).

## File structure

| File | Action | Responsibility |
|---|---|---|
| `internal/config/config.go` | Modify | `Pool.Backend` field, top-level `Docker.Runtime`, validation, `HasBackend()` |
| `internal/config/config_test.go` | Modify | Table tests for the new fields |
| `internal/imagebake/bake.go` | Modify | `LatestRunner` gains an `arch` parameter |
| `internal/imagebake/bake_test.go` | Modify | Update `LatestRunner` tests for arch |
| `scripts/docker/Dockerfile` | Create | Runner image: Ubuntu 24.04 + Docker Engine + actions-runner |
| `scripts/docker/entrypoint.sh` | Create | Container PID 1: start inner dockerd (no iptables), run one job |
| `scripts/embed.go` | Modify | Embed the two new assets |
| `scripts/embed_test.go` | Modify | Assert new assets are non-empty |
| `internal/dockerbackend/docker.go` | Create | `Provisioner`, `RunArgs`, `RunnerArch`, constants |
| `internal/dockerbackend/docker_test.go` | Create | Golden args, provision lifecycle with fake docker |
| `internal/dockerbackend/container.go` | Create | `Container` implementing `pool.VM` over docker wait/stop/rm/logs |
| `internal/dockerbackend/container_test.go` | Create | Lifecycle tests with fake docker |
| `internal/dockerbackend/bake.go` | Create | `docker build` from embedded assets + provenance JSON |
| `internal/dockerbackend/bake_test.go` | Create | Bake test with fake docker + httptest GitHub API |
| `internal/dockerbackend/reap.go` | Create | `ReapContainers` (label-filtered force-remove) |
| `internal/dockerbackend/reap_test.go` | Create | Reap test with fake docker |
| `internal/controller/controller.go` | Modify | Per-backend preflight + per-pool provisioner wiring |
| `cmd/github-qemu-runner/main.go` | Modify | `refresh-image` bakes per backend; `setup` docker preflights |
| `packaging/config.example.yaml` | Modify | Example docker pool + `docker:` section |
| `README.md` | Modify | Docker backend docs, gVisor install, OCI nftables gotcha |

---

### Task 1: Config — `backend` field and `docker` section

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go` (it is `package config`). The helper writes a config to a temp file and Loads it:

```go
func loadFromYAML(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
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
	c, err := loadFromYAML(t, strings.Replace(dockerPoolYAML, "    backend: docker\n", "", 1))
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
	c, err := loadFromYAML(t, dockerPoolYAML)
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
	c, err := loadFromYAML(t, "docker:\n  runtime: runc\n"+dockerPoolYAML)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Docker.Runtime; got != "runc" {
		t.Errorf("docker.runtime = %q, want runc", got)
	}
}

func TestInvalidBackendRejected(t *testing.T) {
	_, err := loadFromYAML(t, strings.Replace(dockerPoolYAML, "backend: docker", "backend: podman", 1))
	if err == nil || !strings.Contains(err.Error(), "backend") {
		t.Errorf("want backend validation error, got %v", err)
	}
}

func TestInvalidDockerRuntimeRejected(t *testing.T) {
	_, err := loadFromYAML(t, "docker:\n  runtime: kata\n"+dockerPoolYAML)
	if err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Errorf("want runtime validation error, got %v", err)
	}
}
```

Add `"strings"`, `"os"`, `"path/filepath"` to the test file's imports if not present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestBackend|TestDocker|TestInvalid' -v`
Expected: compile errors — `c.Pools[0].Backend undefined`, `c.Docker undefined`, `c.HasBackend undefined`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`:

Add to `Config` (after `StateDir`):

```go
	Docker   Docker `yaml:"docker"`
```

Add the `Docker` type after the `GitHub` type:

```go
// Docker configures the docker backend (pools with backend: docker).
type Docker struct {
	// Runtime is the container runtime for job containers: "runsc"
	// (gVisor, the default) or "runc" (no sandbox — see README warnings).
	Runtime string `yaml:"runtime"`
}
```

Add to `Pool` (after `Name`):

```go
	Backend string `yaml:"backend"`
```

Add to `applyDefaults()` (inside the pools loop add the Backend default; the Runtime default goes before the loop):

```go
	if c.Docker.Runtime == "" {
		c.Docker.Runtime = "runsc"
	}
```

```go
		if p.Backend == "" {
			p.Backend = "qemu"
		}
```

Add to `validate()` — runtime check before the pools loop, backend check inside it (right after the duplicate-name check):

```go
	if c.Docker.Runtime != "runsc" && c.Docker.Runtime != "runc" {
		return fmt.Errorf(`docker.runtime must be "runsc" or "runc"`)
	}
```

```go
		if p.Backend != "qemu" && p.Backend != "docker" {
			return fmt.Errorf(`pool %s: backend must be "qemu" or "docker"`, p.Name)
		}
```

Add the helper after `CapacityWarnings`:

```go
// HasBackend reports whether any pool uses the given backend.
func (c *Config) HasBackend(b string) bool {
	for _, p := range c.Pools {
		if p.Backend == b {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: all PASS (including pre-existing tests — if an existing test fixture now fails validation, fix the fixture, not the validation).

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): per-pool backend field and docker.runtime section"
```

---

### Task 2: `imagebake.LatestRunner` arch parameter

The function currently hardcodes `linux-x64`. The docker backend needs `linux-arm64` on arm hosts.

**Files:**
- Modify: `internal/imagebake/bake.go:161-210`
- Test: `internal/imagebake/bake_test.go`

- [ ] **Step 1: Update the existing `LatestRunner` test(s) and add an arm64 case**

In `internal/imagebake/bake_test.go`, find the existing `LatestRunner` test (it serves a fake release JSON via `httptest`). Change every call `LatestRunner(ctx, client, base)` to `LatestRunner(ctx, client, base, "x64")`. Then add a test that the asset name follows the arch (reuse the test's existing fake-server pattern; the JSON below works with a fresh `httptest.NewServer` if preferred):

```go
func TestLatestRunnerArm64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"tag_name": "v2.335.1",
			"body": "actions-runner-linux-arm64-2.335.1.tar.gz aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"assets": [
				{"name": "actions-runner-linux-x64-2.335.1.tar.gz", "browser_download_url": "https://example.com/x64.tar.gz"},
				{"name": "actions-runner-linux-arm64-2.335.1.tar.gz", "browser_download_url": "https://example.com/arm64.tar.gz"}
			]
		}`))
	}))
	defer srv.Close()
	rel, err := LatestRunner(context.Background(), srv.Client(), srv.URL, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if rel.TarballURL != "https://example.com/arm64.tar.gz" {
		t.Errorf("TarballURL = %q, want arm64 asset", rel.TarballURL)
	}
	if rel.SHA256 != strings.Repeat("a", 64) {
		t.Errorf("SHA256 = %q", rel.SHA256)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/imagebake/ -run TestLatestRunner -v`
Expected: compile error — too many arguments to `LatestRunner`.

- [ ] **Step 3: Implement**

In `internal/imagebake/bake.go`:

```go
// LatestRunner resolves the newest actions/runner release for the given
// arch ("x64" or "arm64"). The tarball SHA is scraped from the release
// notes; if the notes format changes, SHA256 comes back empty and the
// caller proceeds on TLS alone.
func LatestRunner(ctx context.Context, client *http.Client, apiBase, arch string) (Release, error) {
```

and inside, change the asset name line to:

```go
	want := fmt.Sprintf("actions-runner-linux-%s-%s.tar.gz", arch, version)
```

Update the doc comment on `Release` from "for linux-x64" to "for one linux arch". Update the one caller in `Bake()` (`internal/imagebake/bake.go:264`):

```go
	rel, err := LatestRunner(ctx, o.HTTP, o.APIBase, "x64")
```

(The qemu backend stays x64-only: it launches `qemu-system-x86_64`.)

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/imagebake/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/imagebake/
git commit -m "feat(imagebake): parameterize LatestRunner by runner arch"
```

---

### Task 3: Embedded Dockerfile and entrypoint

**Files:**
- Create: `scripts/docker/Dockerfile`
- Create: `scripts/docker/entrypoint.sh`
- Modify: `scripts/embed.go`
- Test: `scripts/embed_test.go`

- [ ] **Step 1: Write the failing embed test**

Append to `scripts/embed_test.go` (match the existing test's style — it asserts the embedded guest scripts are non-empty):

```go
func TestDockerAssetsEmbedded(t *testing.T) {
	if !strings.Contains(Dockerfile, "FROM ubuntu:24.04") {
		t.Error("Dockerfile missing or wrong base image")
	}
	if !strings.Contains(DockerEntrypoint, "--iptables=false") {
		t.Error("entrypoint must disable inner dockerd iptables (gVisor has no netfilter)")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./scripts/ -run TestDockerAssets -v`
Expected: compile error — `undefined: Dockerfile`.

- [ ] **Step 3: Create the assets**

`scripts/docker/Dockerfile`:

```dockerfile
# Runner image for the docker backend: Ubuntu 24.04 + Docker Engine
# (DinD, run inside gVisor) + actions-runner. Built locally by
# `github-qemu-runner refresh-image`; never pushed to a registry.
FROM ubuntu:24.04
ARG RUNNER_VERSION
ARG RUNNER_TARBALL_URL
ARG RUNNER_TARBALL_SHA256

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl git && \
    install -m 0755 -d /etc/apt/keyrings && \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc && \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" \
        > /etc/apt/sources.list.d/docker.list && \
    apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin && \
    rm -rf /var/lib/apt/lists/*

# uid 1001: ubuntu:24.04 ships an "ubuntu" user at 1000.
RUN useradd --create-home --uid 1001 runner && usermod -aG docker runner

RUN mkdir -p /opt/actions-runner && cd /opt/actions-runner && \
    curl -fsSL -o runner.tar.gz "$RUNNER_TARBALL_URL" && \
    { [ -z "$RUNNER_TARBALL_SHA256" ] || echo "$RUNNER_TARBALL_SHA256  runner.tar.gz" | sha256sum -c -; } && \
    tar xzf runner.tar.gz && rm runner.tar.gz && \
    apt-get update && ./bin/installdependencies.sh && rm -rf /var/lib/apt/lists/* && \
    chown -R runner:runner /opt/actions-runner

COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# Anonymous volume so inner-docker state never lands on the container's
# overlay; removed with the container (docker rm --volumes).
VOLUME /var/lib/docker

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
```

`scripts/docker/entrypoint.sh`:

```bash
#!/usr/bin/env bash
# PID 1 of the ephemeral job container (run with --runtime=runsc
# --privileged). Starts the inner dockerd, then runs exactly one GitHub
# Actions job as the unprivileged "runner" user; the container exits when
# the job finishes — host-side teardown depends on that exit.
set -uo pipefail

JIT_FILE=/jit/config
if [ ! -s "$JIT_FILE" ]; then
  echo "entrypoint: $JIT_FILE missing or empty" >&2
  exit 1
fi

# gVisor exposes no netfilter to the sandbox: without --iptables=false the
# inner dockerd dies at boot ("Failed to initialize nft"). Validated on
# runsc release-20260601.0 / Docker 29.x.
dockerd --iptables=false --ip6tables=false >/var/log/dockerd.log 2>&1 &

for _ in $(seq 1 60); do
  docker info >/dev/null 2>&1 && break
  sleep 1
done
if ! docker info >/dev/null 2>&1; then
  echo "entrypoint: inner dockerd failed to start" >&2
  tail -n 50 /var/log/dockerd.log >&2
  exit 1
fi

cd /opt/actions-runner || exit 1
# The JIT config registers a pre-created ephemeral runner; run.sh executes
# one job, deregisters, and exits. The blob is single-use, so its
# visibility in container-internal argv is harmless.
exec runuser -u runner -- ./run.sh --jitconfig "$(cat "$JIT_FILE")"
```

Make it executable: `chmod +x scripts/docker/entrypoint.sh`.

Append to `scripts/embed.go`:

```go
//go:embed docker/Dockerfile
var Dockerfile string

//go:embed docker/entrypoint.sh
var DockerEntrypoint string
```

- [ ] **Step 4: Run tests + shellcheck**

Run: `go test ./scripts/ -v` — expected: PASS.
Run: `shellcheck scripts/docker/entrypoint.sh` — expected: clean (lefthook runs it on commit anyway).

- [ ] **Step 5: Commit**

```bash
git add scripts/
git commit -m "feat(dockerbackend): embedded runner-image Dockerfile and entrypoint"
```

---

### Task 4: `dockerbackend` — constants, `RunnerArch`, `RunArgs`

**Files:**
- Create: `internal/dockerbackend/docker.go`
- Test: `internal/dockerbackend/docker_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/dockerbackend/docker_test.go`:

```go
package dockerbackend

import (
	"reflect"
	"testing"

	"github.com/a1678991/github-qemu-runner/internal/config"
)

func TestRunnerArch(t *testing.T) {
	for goarch, want := range map[string]string{
		"amd64": "x64",
		"arm64": "arm64",
	} {
		if got := RunnerArch(goarch); got != want {
			t.Errorf("RunnerArch(%q) = %q, want %q", goarch, got, want)
		}
	}
}

func TestRunArgs(t *testing.T) {
	p := config.Pool{CPUs: 2, MemoryMB: 2048}
	got := RunArgs("ghq-oci-ab12", "runsc", p, "/var/lib/x/run/ghq-oci-ab12/jit")
	want := []string{
		"run", "--detach",
		"--name", "ghq-oci-ab12",
		"--runtime", "runsc",
		"--privileged",
		"--cpus", "2",
		"--memory", "2048m",
		"--label", "ghq.managed=true",
		"--volume", "/var/lib/x/run/ghq-oci-ab12/jit:/jit:ro",
		"ghq-runner-base:latest",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunArgs:\n got %q\nwant %q", got, want)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dockerbackend/ -v`
Expected: compile error — package and functions do not exist.

- [ ] **Step 3: Implement**

`internal/dockerbackend/docker.go`:

```go
// Package dockerbackend runs ephemeral runner jobs in Docker containers
// sandboxed by gVisor (runsc), for hosts without /dev/kvm. One job = one
// container; the container exiting is the job-done signal (the docker
// analogue of guest poweroff in the qemu backend).
package dockerbackend

import (
	"strconv"

	"github.com/a1678991/github-qemu-runner/internal/config"
)

// Image is the locally-built runner image tag (see Bake).
const Image = "ghq-runner-base:latest"

// managedLabel marks containers owned by this controller for reaping.
const managedLabel = "ghq.managed=true"

// RunnerArch maps GOARCH to the actions-runner release arch suffix.
func RunnerArch(goarch string) string {
	if goarch == "arm64" {
		return "arm64"
	}
	return "x64"
}

// RunArgs builds the `docker run` argv for one job container.
// --privileged is required by the inner dockerd (DinD); under runtime
// runsc it grants capabilities inside gVisor's sandbox, not on the host.
func RunArgs(name, runtime string, p config.Pool, jitDir string) []string {
	return []string{
		"run", "--detach",
		"--name", name,
		"--runtime", runtime,
		"--privileged",
		"--cpus", strconv.Itoa(p.CPUs),
		"--memory", strconv.Itoa(p.MemoryMB) + "m",
		"--label", managedLabel,
		"--volume", jitDir + ":/jit:ro",
		Image,
	}
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/dockerbackend/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dockerbackend/
git commit -m "feat(dockerbackend): docker run argv builder and arch mapping"
```

---

### Task 5: `dockerbackend.Container` (implements `pool.VM`)

**Files:**
- Create: `internal/dockerbackend/container.go`
- Test: `internal/dockerbackend/container_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/dockerbackend/container_test.go`. The fake docker is a shell script that logs its argv and scripts `wait`/`logs` behavior:

```go
package dockerbackend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDocker writes a docker stand-in script that appends each invocation
// to argv.log and emulates the subcommands Container uses.
// waitExit is what `docker wait` prints (the container's exit code).
func fakeDocker(t *testing.T, waitExit string) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "docker")
	script := `#!/bin/sh
echo "$@" >> ` + argvLog + `
case "$1" in
  wait) echo ` + waitExit + ` ;;
  logs) echo "console output here" ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

func awaitDone(t *testing.T, c *Container) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("container did not report done")
	}
}

func TestContainerCleanExit(t *testing.T) {
	bin, _ := fakeDocker(t, "0")
	c := newContainer(bin, "ghq-x-1")
	awaitDone(t, c)
	if err := c.Err(); err != nil {
		t.Errorf("Err() = %v, want nil for exit 0", err)
	}
}

func TestContainerNonzeroExit(t *testing.T) {
	bin, _ := fakeDocker(t, "137")
	c := newContainer(bin, "ghq-x-2")
	awaitDone(t, c)
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "137") {
		t.Errorf("Err() = %v, want exit-status error containing 137", err)
	}
}

func TestContainerKill(t *testing.T) {
	bin, argvLog := fakeDocker(t, "0")
	c := newContainer(bin, "ghq-x-3")
	if err := c.Kill(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(b), "rm --force --volumes ghq-x-3") {
		t.Errorf("Kill did not force-remove with volumes; argv log:\n%s", b)
	}
}

func TestContainerPowerdown(t *testing.T) {
	bin, argvLog := fakeDocker(t, "0")
	c := newContainer(bin, "ghq-x-4")
	if err := c.Powerdown(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(b), "stop --time 30 ghq-x-4") {
		t.Errorf("Powerdown did not docker stop; argv log:\n%s", b)
	}
}

func TestContainerConsoleTail(t *testing.T) {
	bin, _ := fakeDocker(t, "0")
	c := newContainer(bin, "ghq-x-5")
	if got := c.ConsoleTail(); !strings.Contains(got, "console output here") {
		t.Errorf("ConsoleTail() = %q", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dockerbackend/ -run TestContainer -v`
Expected: compile error — `undefined: newContainer`.

- [ ] **Step 3: Implement**

`internal/dockerbackend/container.go`:

```go
package dockerbackend

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Container supervises one job container via the docker CLI. It satisfies
// pool.VM: Done fires when the container exits (docker wait returns).
type Container struct {
	bin  string
	name string
	done chan struct{}

	mu  sync.Mutex
	err error
}

// newContainer starts watching an already-running container. docker wait
// blocks until the container stops and prints its exit code.
func newContainer(bin, name string) *Container {
	c := &Container{bin: bin, name: name, done: make(chan struct{})}
	go func() {
		out, err := exec.Command(bin, "wait", name).Output()
		switch {
		case err != nil:
			c.setErr(fmt.Errorf("docker wait %s: %w", name, err))
		case strings.TrimSpace(string(out)) != "0":
			c.setErr(fmt.Errorf("container %s exited with status %s",
				name, strings.TrimSpace(string(out))))
		}
		close(c.done)
	}()
	return c
}

func (c *Container) setErr(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

// Done is closed when the container has exited.
func (c *Container) Done() <-chan struct{} { return c.done }

// Err reports the container exit error; only meaningful after Done.
func (c *Container) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Kill force-removes the container (and its anonymous volumes, including
// the inner /var/lib/docker) and waits for the watcher to notice.
func (c *Container) Kill() error {
	_ = exec.Command(c.bin, "rm", "--force", "--volumes", c.name).Run()
	<-c.done
	return nil
}

// Powerdown stops the container gracefully: SIGTERM to the entrypoint,
// SIGKILL after timeout (docker stop's built-in escalation), Kill as the
// last resort. Always terminates the container.
func (c *Container) Powerdown(timeout time.Duration) error {
	secs := max(int(timeout/time.Second), 1)
	if err := exec.Command(c.bin, "stop", "--time", strconv.Itoa(secs), c.name).Run(); err != nil {
		return c.Kill()
	}
	select {
	case <-c.done:
		return nil
	case <-time.After(timeout + 30*time.Second):
		return c.Kill()
	}
}

// ConsoleTail returns the last 2 KiB of the container's logs, so failures
// can be surfaced into the journal before teardown removes the container.
func (c *Container) ConsoleTail() string {
	out, err := exec.Command(c.bin, "logs", "--tail", "50", c.name).CombinedOutput()
	if err != nil {
		return ""
	}
	if len(out) > 2048 {
		out = out[len(out)-2048:]
	}
	return string(out)
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/dockerbackend/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dockerbackend/
git commit -m "feat(dockerbackend): container supervisor implementing pool.VM"
```

---

### Task 6: `dockerbackend.Provisioner`

**Files:**
- Modify: `internal/dockerbackend/docker.go`
- Test: `internal/dockerbackend/docker_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/dockerbackend/docker_test.go` (reuses `fakeDocker` from Task 5's test file — same package):

```go
func TestProvisionLifecycle(t *testing.T) {
	bin, argvLog := fakeDocker(t, "0")
	runDir := filepath.Join(t.TempDir(), "run")
	d := &Provisioner{RunDir: runDir, DockerBin: bin, Runtime: "runsc"}
	p := config.Pool{Name: "oci", CPUs: 2, MemoryMB: 2048}

	vm, cleanup, err := d.Provision(context.Background(), "ghq-oci-ab12", p, "JITBLOB")
	if err != nil {
		t.Fatal(err)
	}
	jitFile := filepath.Join(runDir, "ghq-oci-ab12", "jit", "config")
	b, err := os.ReadFile(jitFile)
	if err != nil || string(b) != "JITBLOB" {
		t.Errorf("jit file: %v, content %q", err, b)
	}
	if fi, _ := os.Stat(jitFile); fi.Mode().Perm() != 0o600 {
		t.Errorf("jit file mode = %v, want 0600", fi.Mode().Perm())
	}
	argv, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "run --detach --name ghq-oci-ab12 --runtime runsc --privileged") {
		t.Errorf("docker run argv wrong:\n%s", argv)
	}

	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("fake container did not exit")
	}
	cleanup()
	if _, err := os.Stat(filepath.Join(runDir, "ghq-oci-ab12")); !os.IsNotExist(err) {
		t.Error("workdir not removed by cleanup")
	}
	argv, _ = os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "rm --force --volumes ghq-oci-ab12") {
		t.Errorf("cleanup did not remove container; argv log:\n%s", argv)
	}
}

func TestProvisionFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	failingDocker := filepath.Join(dir, "docker")
	// `docker run` fails; `docker rm` (cleanup) succeeds.
	script := "#!/bin/sh\ncase \"$1\" in run) exit 1 ;; esac\nexit 0\n"
	if err := os.WriteFile(failingDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "run")
	d := &Provisioner{RunDir: runDir, DockerBin: failingDocker, Runtime: "runsc"}
	_, _, err := d.Provision(context.Background(), "ghq-x-y", config.Pool{CPUs: 1, MemoryMB: 512}, "J")
	if err == nil {
		t.Fatal("want error from failed docker run")
	}
	if _, statErr := os.Stat(filepath.Join(runDir, "ghq-x-y")); !os.IsNotExist(statErr) {
		t.Error("failed provision left workdir behind")
	}
}
```

Add `"context"`, `"os"`, `"path/filepath"`, `"strings"`, `"time"` to the imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dockerbackend/ -run TestProvision -v`
Expected: compile error — `undefined: Provisioner`.

- [ ] **Step 3: Implement**

Append to `internal/dockerbackend/docker.go` (add imports `"context"`, `"fmt"`, `"os"`, `"os/exec"`, `"path/filepath"`, and `"github.com/a1678991/github-qemu-runner/internal/pool"`):

```go
// Provisioner creates one Docker container per job. It implements
// pool.Provisioner; the per-job workdir under RunDir holds only the JIT
// config file (bind-mounted read-only into the container).
type Provisioner struct {
	RunDir    string // <state>/run
	DockerBin string
	Runtime   string // "runsc" or "runc"
}

func (d *Provisioner) Provision(ctx context.Context, name string, p config.Pool, jitConfig string) (pool.VM, func(), error) {
	dir := filepath.Join(d.RunDir, name)
	jitDir := filepath.Join(dir, "jit")
	if err := os.MkdirAll(jitDir, 0o700); err != nil {
		return nil, nil, err
	}
	// rm of a container that never started is a harmless no-op, so one
	// cleanup covers every exit path.
	cleanup := func() {
		_ = exec.Command(d.DockerBin, "rm", "--force", "--volumes", name).Run()
		_ = os.RemoveAll(dir)
	}
	fail := func(err error) (pool.VM, func(), error) {
		cleanup()
		return nil, nil, err
	}

	if err := os.WriteFile(filepath.Join(jitDir, "config"), []byte(jitConfig), 0o600); err != nil {
		return fail(err)
	}
	absJitDir, err := filepath.Abs(jitDir)
	if err != nil {
		return fail(err)
	}
	if out, err := exec.CommandContext(ctx, d.DockerBin,
		RunArgs(name, d.Runtime, p, absJitDir)...).CombinedOutput(); err != nil {
		return fail(fmt.Errorf("docker run %s: %v: %s", name, err, out))
	}
	return newContainer(d.DockerBin, name), cleanup, nil
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/dockerbackend/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dockerbackend/
git commit -m "feat(dockerbackend): per-job container provisioner"
```

---

### Task 7: `dockerbackend.ReapContainers`

**Files:**
- Create: `internal/dockerbackend/reap.go`
- Test: `internal/dockerbackend/reap_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/dockerbackend/reap_test.go`:

```go
package dockerbackend

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reapFakeDocker emits container ids for `ps` and logs all argv.
func reapFakeDocker(t *testing.T, psOutput string) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "docker")
	script := `#!/bin/sh
echo "$@" >> ` + argvLog + `
case "$1" in
  ps) printf '` + psOutput + `' ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

func TestReapContainersRemovesManaged(t *testing.T) {
	bin, argvLog := reapFakeDocker(t, `abc123\ndef456\n`)
	ReapContainers(context.Background(), bin, slog.New(slog.DiscardHandler))
	argv, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "ps --all --quiet --filter label=ghq.managed=true") {
		t.Errorf("ps filter wrong:\n%s", argv)
	}
	if !strings.Contains(string(argv), "rm --force --volumes abc123 def456") {
		t.Errorf("rm argv wrong:\n%s", argv)
	}
}

func TestReapContainersNoneFound(t *testing.T) {
	bin, argvLog := reapFakeDocker(t, ``)
	ReapContainers(context.Background(), bin, slog.New(slog.DiscardHandler))
	argv, _ := os.ReadFile(argvLog)
	if strings.Contains(string(argv), "rm") {
		t.Errorf("rm must not run when nothing matched:\n%s", argv)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dockerbackend/ -run TestReap -v`
Expected: compile error — `undefined: ReapContainers`.

- [ ] **Step 3: Implement**

`internal/dockerbackend/reap.go`:

```go
package dockerbackend

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
)

// ReapContainers force-removes all containers (running or exited) labeled
// as managed by this controller — crash leftovers from a previous run.
// Best-effort; failures are logged, never fatal (mirrors ReapOrphans).
func ReapContainers(ctx context.Context, dockerBin string, log *slog.Logger) {
	out, err := exec.CommandContext(ctx, dockerBin,
		"ps", "--all", "--quiet", "--filter", "label="+managedLabel).Output()
	if err != nil {
		log.Warn("list managed containers", "err", err)
		return
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return
	}
	args := append([]string{"rm", "--force", "--volumes"}, ids...)
	if out, err := exec.CommandContext(ctx, dockerBin, args...).CombinedOutput(); err != nil {
		log.Warn("remove orphan containers", "err", err, "output", string(out))
		return
	}
	log.Info("removed orphan containers", "count", len(ids))
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/dockerbackend/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dockerbackend/
git commit -m "feat(dockerbackend): orphan container reaping"
```

---

### Task 8: `dockerbackend.Bake`

**Files:**
- Create: `internal/dockerbackend/bake.go`
- Test: `internal/dockerbackend/bake_test.go`

- [ ] **Step 1: Write the failing test**

`internal/dockerbackend/bake_test.go`:

```go
package dockerbackend

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBake(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"tag_name": "v2.335.1",
			"body": "",
			"assets": [
				{"name": "actions-runner-linux-x64-2.335.1.tar.gz", "browser_download_url": "https://example.com/x64.tar.gz"},
				{"name": "actions-runner-linux-arm64-2.335.1.tar.gz", "browser_download_url": "https://example.com/arm64.tar.gz"}
			]
		}`))
	}))
	defer api.Close()

	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	dockerBin := filepath.Join(dir, "docker")
	// The fake records argv and captures the build context directory
	// (last argument) so the test can inspect what would be built.
	script := `#!/bin/sh
echo "$@" >> ` + argvLog + `
for last; do :; done
cp -r "$last" ` + filepath.Join(dir, "context") + `
exit 0
`
	if err := os.WriteFile(dockerBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(dir, "state")
	err := Bake(context.Background(), BakeOptions{
		StateDir:  stateDir,
		HTTP:      api.Client(),
		APIBase:   api.URL,
		DockerBin: dockerBin,
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	argv, _ := os.ReadFile(argvLog)
	for _, want := range []string{
		"build --pull",
		"--build-arg RUNNER_VERSION=2.335.1",
		"--tag " + Image,
	} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("docker build argv missing %q:\n%s", want, argv)
		}
	}
	for _, f := range []string{"Dockerfile", "entrypoint.sh"} {
		if _, err := os.Stat(filepath.Join(dir, "context", f)); err != nil {
			t.Errorf("build context missing %s: %v", f, err)
		}
	}

	var meta map[string]string
	b, err := os.ReadFile(filepath.Join(stateDir, "images", "docker-base.json"))
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["runner_version"] != "2.335.1" {
		t.Errorf("provenance runner_version = %q", meta["runner_version"])
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dockerbackend/ -run TestBake -v`
Expected: compile error — `undefined: Bake`, `undefined: BakeOptions`.

- [ ] **Step 3: Implement**

`internal/dockerbackend/bake.go`:

```go
package dockerbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/imagebake"
	"github.com/a1678991/github-qemu-runner/scripts"
)

type BakeOptions struct {
	StateDir  string
	HTTP      *http.Client
	APIBase   string
	DockerBin string
	Log       *slog.Logger
}

func (o *BakeOptions) defaults() {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 15 * time.Minute}
	}
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
}

// Bake builds the runner container image from the embedded Dockerfile and
// tags it Image. The build runs natively on this host, so the image arch
// always matches (arm64 hosts get linux-arm64 runners). A provenance
// sidecar lands at <state>/images/docker-base.json, mirroring base.json.
func Bake(ctx context.Context, o BakeOptions) error {
	o.defaults()
	rel, err := imagebake.LatestRunner(ctx, o.HTTP, o.APIBase, RunnerArch(runtime.GOARCH))
	if err != nil {
		return fmt.Errorf("resolve runner release: %w", err)
	}
	if rel.SHA256 == "" {
		o.Log.Warn("runner tarball SHA not found in release notes; relying on TLS only")
	}
	o.Log.Info("building runner image", "runner_version", rel.Version, "tag", Image)

	buildDir, err := os.MkdirTemp("", "ghq-docker-bake-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(buildDir) }()
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(scripts.Dockerfile), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildDir, "entrypoint.sh"), []byte(scripts.DockerEntrypoint), 0o755); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, o.DockerBin, "build", "--pull",
		"--build-arg", "RUNNER_VERSION="+rel.Version,
		"--build-arg", "RUNNER_TARBALL_URL="+rel.TarballURL,
		"--build-arg", "RUNNER_TARBALL_SHA256="+rel.SHA256,
		"--tag", Image, buildDir)
	cmd.Stdout = os.Stderr // long build; stream progress instead of buffering
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}

	imagesDir := filepath.Join(o.StateDir, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return err
	}
	meta, err := json.MarshalIndent(map[string]string{
		"runner_version": rel.Version,
		"arch":           RunnerArch(runtime.GOARCH),
		"baked_at":       time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(imagesDir, "docker-base.json"), append(meta, '\n'), 0o644); err != nil {
		return err
	}
	o.Log.Info("runner image built", "tag", Image)
	return nil
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/dockerbackend/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dockerbackend/
git commit -m "feat(dockerbackend): bake runner image via docker build"
```

---

### Task 9: Controller wiring

**Files:**
- Modify: `internal/controller/controller.go:22-69`

No new unit test: `controller.Run` is integration glue (it has no existing test either) and everything it wires is tested in its own package. Verification is compile + full test suite + the manual smoke in Task 12.

- [ ] **Step 1: Rewrite `controller.Run`**

Replace the body of `Run` in `internal/controller/controller.go` (keep `apiPrefixes` and `HostMemMB` as they are; add import `"github.com/a1678991/github-qemu-runner/internal/dockerbackend"`):

```go
// Run wires everything together and blocks until ctx is cancelled and all
// pools have drained.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	keyPEM, err := os.ReadFile(cfg.GitHub.PrivateKeyPath)
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}
	key, err := github.ParseRSAPrivateKey(keyPEM)
	if err != nil {
		return err
	}
	gh := github.New(cfg.GitHub.APIBaseURL, cfg.GitHub.AppID, cfg.GitHub.InstallationID, key)

	runDir := filepath.Join(cfg.StateDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return err
	}

	var qemuProv *QEMUProvisioner
	if cfg.HasBackend("qemu") {
		qemuBin, err := exec.LookPath("qemu-system-x86_64")
		if err != nil {
			return fmt.Errorf("qemu-system-x86_64 not found: %w", err)
		}
		basePath, err := filepath.Abs(filepath.Join(cfg.StateDir, "images", "base.qcow2"))
		if err != nil {
			return err
		}
		if _, err := os.Stat(basePath); err != nil {
			return fmt.Errorf("base image missing (run `github-qemu-runner refresh-image` first): %w", err)
		}
		qemuProv = &QEMUProvisioner{RunDir: runDir, BasePath: basePath, QEMUBin: qemuBin}
	}

	var dockerProv *dockerbackend.Provisioner
	if cfg.HasBackend("docker") {
		dockerBin, err := exec.LookPath("docker")
		if err != nil {
			return fmt.Errorf("docker not found: %w", err)
		}
		if err := exec.CommandContext(ctx, dockerBin, "image", "inspect", dockerbackend.Image).Run(); err != nil {
			return fmt.Errorf("runner image %s missing (run `github-qemu-runner refresh-image` first): %w", dockerbackend.Image, err)
		}
		if cfg.Docker.Runtime == "runc" {
			log.Warn("docker pools run WITHOUT gVisor (docker.runtime: runc): " +
				"--privileged DinD containers have effectively no isolation boundary")
		}
		// Containers reference jit mounts under runDir, so reap them
		// before ReapOrphans deletes the workdirs.
		dockerbackend.ReapContainers(ctx, dockerBin, log)
		dockerProv = &dockerbackend.Provisioner{RunDir: runDir, DockerBin: dockerBin, Runtime: cfg.Docker.Runtime}
	}

	for _, w := range cfg.CapacityWarnings(runtime.NumCPU(), HostMemMB()) {
		log.Warn(w)
	}

	ReapOrphans(ctx, runDir, gh, apiPrefixes(cfg), log)

	var wg sync.WaitGroup
	for _, pc := range cfg.Pools {
		var prov pool.Provisioner = qemuProv
		if pc.Backend == "docker" {
			prov = dockerProv
		}
		p := &pool.Pool{Cfg: pc, GH: gh, Prov: prov, Log: log}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Run(ctx)
		}()
	}
	log.Info("controller running", "pools", len(cfg.Pools))
	wg.Wait()
	log.Info("all pools drained; exiting")
	return nil
}
```

- [ ] **Step 2: Build and run the full suite**

Run: `go build ./... && go test ./...`
Expected: everything compiles, all tests PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/
git commit -m "feat(controller): per-backend preflight and provisioner wiring"
```

---

### Task 10: `main.go` — `refresh-image` and `setup`

**Files:**
- Modify: `cmd/github-qemu-runner/main.go:63-134`

- [ ] **Step 1: Update `runRefreshImage`**

Bake only what the config uses (add import `"github.com/a1678991/github-qemu-runner/internal/dockerbackend"`):

```go
func runRefreshImage(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.HasBackend("qemu") {
		qemuBin, err := exec.LookPath("qemu-system-x86_64")
		if err != nil {
			return err
		}
		if err := imagebake.Bake(ctx, imagebake.Options{
			StateDir: cfg.StateDir,
			APIBase:  cfg.GitHub.APIBaseURL,
			QEMUBin:  qemuBin,
			Log:      log,
		}); err != nil {
			return err
		}
	}
	if cfg.HasBackend("docker") {
		dockerBin, err := exec.LookPath("docker")
		if err != nil {
			return err
		}
		if err := dockerbackend.Bake(ctx, dockerbackend.BakeOptions{
			StateDir:  cfg.StateDir,
			APIBase:   cfg.GitHub.APIBaseURL,
			DockerBin: dockerBin,
			Log:       log,
		}); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 2: Update `runSetup`**

Gate the QEMU checks behind `cfg.HasBackend("qemu")` and add docker checks. Replace the middle of `runSetup` — everything between the config check and the private-key check — with:

```go
	if cfg.HasBackend("qemu") {
		for _, bin := range []string{"qemu-system-x86_64", "qemu-img", "genisoimage"} {
			_, lookErr := exec.LookPath(bin)
			check(bin+" on PATH", lookErr)
		}
		kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err == nil {
			_ = kvm.Close()
		}
		check("/dev/kvm read-write access", err)
	}

	if cfg.HasBackend("docker") {
		dockerBin, lookErr := exec.LookPath("docker")
		check("docker on PATH", lookErr)
		if lookErr == nil {
			check("docker daemon reachable", exec.CommandContext(ctx, dockerBin, "info").Run())
			if cfg.Docker.Runtime == "runsc" {
				out, err := exec.CommandContext(ctx, dockerBin, "info", "--format", "{{json .Runtimes}}").Output()
				if err == nil && !strings.Contains(string(out), `"runsc"`) {
					err = fmt.Errorf(`runsc not in docker runtimes — register it in /etc/docker/daemon.json with runtimeArgs ["--net-raw","--allow-packet-socket-write"], then restart docker`)
				}
				check("runsc runtime registered", err)
			} else {
				fmt.Printf("warn  docker.runtime is runc: job containers run WITHOUT gVisor; --privileged DinD has effectively no isolation boundary\n")
			}
			if err := exec.CommandContext(ctx, dockerBin, "image", "inspect", dockerbackend.Image).Run(); err != nil {
				fmt.Printf("note  runner image missing; run `github-qemu-runner refresh-image`\n")
			} else {
				fmt.Printf("ok    runner image %s\n", dockerbackend.Image)
				// Catches host-firewall problems (e.g. OCI's default
				// inet-filter forward DROP) that only bite inside containers.
				check("container outbound connectivity", exec.CommandContext(ctx, dockerBin,
					"run", "--rm", "--runtime", cfg.Docker.Runtime, "--entrypoint", "curl",
					dockerbackend.Image, "-fsS", "--max-time", "30",
					"-o", "/dev/null", "https://api.github.com").Run())
			}
		}
	}
```

Also move the qemu base-image note (the `base.qcow2` stat near the end of `runSetup`) inside an `if cfg.HasBackend("qemu") { ... }` guard. Add `"strings"` and `"github.com/a1678991/github-qemu-runner/internal/dockerbackend"` to imports. Update the package doc comment on line 1-4 to mention containers, e.g. "runs ephemeral GitHub Actions runners in QEMU/KVM virtual machines or gVisor-sandboxed Docker containers".

- [ ] **Step 3: Build, vet, full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: clean build, all PASS.

- [ ] **Step 4: Manual sanity check of setup output**

```bash
cat > /tmp/ghq-docker-test.yaml <<'EOF'
github:
  app_id: 1
  installation_id: 1
  private_key_path: /nonexistent.pem
pools:
  - name: t
    backend: docker
    scope: org
    org: x
    count: 1
    cpus: 1
    memory_mb: 512
    disk_gb: 10
    labels: [t]
EOF
go run ./cmd/github-qemu-runner -config /tmp/ghq-docker-test.yaml setup; rm /tmp/ghq-docker-test.yaml
```
Expected: NO qemu/kvm checks printed; docker checks printed (FAIL/note lines are fine on a dev machine — the point is which checks run); exits nonzero because the key is missing.

- [ ] **Step 5: Commit**

```bash
git add cmd/
git commit -m "feat(cli): docker-aware refresh-image and setup preflights"
```

---

### Task 11: Example config, README, packaging docs

**Files:**
- Modify: `packaging/config.example.yaml`
- Modify: `README.md`

- [ ] **Step 1: Extend `packaging/config.example.yaml`**

Append a commented docker section and pool example (match the file's existing comment style):

```yaml
# Docker backend (for hosts without /dev/kvm). Jobs run in gVisor-sandboxed
# containers; see "Docker backend" in the README for host prerequisites.
#docker:
#  runtime: runsc        # runsc (default) | runc (NO sandbox — read README first)

#  - name: oci-arm
#    backend: docker     # qemu (default) | docker
#    scope: org
#    org: my-org
#    count: 2
#    cpus: 2
#    memory_mb: 8192
#    disk_gb: 30         # advisory only on docker pools (not enforced)
#    labels: [self-hosted, linux, arm64, oci]
```

- [ ] **Step 2: Add a "Docker backend" section to `README.md`**

Insert after the "How it works" section:

```markdown
## Docker backend (hosts without /dev/kvm)

Pools with `backend: docker` run each job in a disposable Docker container
sandboxed by gVisor instead of a VM — for hosts without nested
virtualization (e.g. OCI Ampere A1 free-tier instances) and for arm64.
Jobs keep full Docker support: a private dockerd runs *inside* the
sandboxed container (DinD), so `container:` jobs, service containers, and
`docker build` work as on the QEMU backend.

Security trade-off, explicitly: gVisor is a userspace-kernel sandbox —
weaker than a KVM VM, far stronger than a plain container. The job
container runs `--privileged` so the inner dockerd works; under `runsc`
those privileges apply to gVisor's synthetic kernel, not the host. Setting
`docker.runtime: runc` removes the sandbox entirely and `--privileged`
becomes root on the host — never do this on a machine you care about.
`disk_gb` is advisory on docker pools (standard storage drivers cannot
enforce per-container quotas); a runaway job can fill the host filesystem.

Host prerequisites:

1. Docker Engine, with the `gh-runner` user in the `docker` group
   (docker-socket access is root-equivalent — this is the documented
   widening vs. the qemu backend's kvm-group-only posture).
2. gVisor (`runsc`) from [gvisor.dev](https://gvisor.dev/docs/user_guide/install/),
   registered in `/etc/docker/daemon.json`:

   ```json
   {
     "runtimes": {
       "runsc": {
         "path": "/usr/bin/runsc",
         "runtimeArgs": ["--net-raw", "--allow-packet-socket-write"]
       }
     }
   }
   ```

   Both runtimeArgs are required for networking inside the inner dockerd
   (the second is mandatory with Docker 28+). Restart docker after editing.
3. **OCI Ubuntu images only:** the stock nftables `inet filter` table has
   a `forward` chain with policy DROP that silently kills all container
   traffic *before* Docker's own rules. Persist accept rules (e.g. in
   `/etc/nftables.conf`):

   ```
   nft add rule inet filter forward ct state related,established accept
   nft add rule inet filter forward iifname docker0 accept
   ```

   `setup` runs an outbound-connectivity check from inside a container to
   catch exactly this.

Then: `setup` → `refresh-image` (builds the `ghq-runner-base:latest` image
natively, so the arch always matches the host) → `systemctl enable --now
github-qemu-runner`. Label docker pools with the real architecture (e.g.
`arm64`), and as with the qemu backend: never attach runners to public
repositories.
```

Also update the README intro line "Every job runs in a disposable QEMU/KVM virtual machine" to mention the docker backend, and add `images/docker-base.json` to the Runbook's image-provenance row.

- [ ] **Step 3: Commit**

```bash
git add packaging/config.example.yaml README.md
git commit -m "docs: docker backend configuration and host prerequisites"
```

---

### Task 12: Full verification + manual smoke checklist

- [ ] **Step 1: Full local gate**

Run: `gofmt -l . && go vet ./... && go test ./... && golangci-lint run`
Expected: gofmt prints nothing; vet/tests/lint clean.

- [ ] **Step 2: Verify the plan's spec coverage**

Re-read `docs/superpowers/specs/2026-06-11-docker-backend-design.md` section by section and confirm each requirement maps to shipped code. Known intentional deferral: none — everything in the spec's scope is in Tasks 1-11.

- [ ] **Step 3: Manual end-to-end smoke (requires the OCI host + a GitHub App; coordinate with the user)**

On the OCI A1 host (Docker 29.x + runsc already installed and registered from the validation run):

1. Re-apply the nftables forward rules if the host rebooted (see README) — and persist them.
2. Deploy the binary (build with `GOOS=linux GOARCH=arm64 go build -o github-qemu-runner ./cmd/github-qemu-runner` and `scp`), config with a one-slot `backend: docker` pool (labels `[self-hosted, linux, arm64, smoke]`), and the App key.
3. `github-qemu-runner -config ... setup` → expect all `ok` including "runner image missing" note.
4. `github-qemu-runner -config ... refresh-image` → builds `ghq-runner-base:latest` (~5-10 min).
5. `setup` again → "container outbound connectivity" must be `ok`.
6. Run the controller in the foreground; in a scratch repo, run a workflow with `runs-on: [self-hosted, linux, arm64, smoke]` containing both a plain step (`uname -m`) and a `container: alpine` job (exercises inner dockerd).
7. Assert: job succeeds; `docker ps -a` shows no leftover container afterwards; runner record is gone from GitHub; `run/` workdir is empty.

- [ ] **Step 4: Commit any fixes found during smoke, then hand off**

Use the superpowers:finishing-a-development-branch skill (merge/PR decision is the user's).

---

## Self-review notes (done at plan time)

- **Spec coverage:** config fields (Task 1), arch-aware runner resolve (Task 2), embedded image assets with validated DinD flags (Task 3), provisioner + container + `pool.VM` mapping (Tasks 4-6), reaping (Task 7), bake + provenance (Task 8), per-backend preflight/wiring + runc warning + reap-before-workdir-delete ordering (Task 9), CLI refresh-image/setup incl. outbound-connectivity preflight (Task 10), docs incl. OCI nftables + advisory disk_gb + security posture (Task 11), E2E (Task 12). JIT-blob hygiene: 0600 file, ro bind mount, not in env/host argv (Task 6).
- **Type consistency:** `Provisioner{RunDir, DockerBin, Runtime}`, `newContainer(bin, name)`, `RunArgs(name, runtime, p, jitDir)`, `ReapContainers(ctx, dockerBin, log)`, `Bake(ctx, BakeOptions)` — names checked against every usage site in Tasks 4-10.
- **`max()` builtin** (Task 5 `Powerdown`) requires Go ≥ 1.21; the codebase already uses `min()` in `pool.go`, so this is fine.
