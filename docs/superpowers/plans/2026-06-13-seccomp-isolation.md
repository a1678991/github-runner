# Seccomp Isolation Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Per-pool `isolation: seccomp` for docker pools — native runc + Docker's default seccomp profile instead of gVisor, for jobs that don't need Docker inside.

**Architecture:** A new `Pool.Isolation` config field branches `RunArgs` between today's privileged-runsc argv (image `ghq-runner-base`) and an unprivileged runc argv (new slim image `ghq-runner-slim`, no Docker Engine, built as a multi-stage target of the existing embedded Dockerfile). Preflights in controller and `setup` become isolation-aware so seccomp-only hosts never need gVisor.

**Tech Stack:** Go (stdlib only, `go test ./...`), embedded Dockerfile/shell assets (shellcheck/shfmt via lefthook), docker CLI shell-outs with fake-binary tests.

**Spec:** `docs/superpowers/specs/2026-06-13-seccomp-isolation-design.md`

Conventions used throughout:
- All paths relative to the repo root.
- Run tests with `go test ./internal/... ./scripts/...` (or the package given in the step).
- Commit messages follow commitlint (conventional commits); lefthook runs gofmt/shellcheck/shfmt on staged files automatically.

---

### Task 1: Config — `isolation`, `seccomp_profile`, `HasDockerIsolation`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
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
```

Note: `dockerPoolYAML` ends with a newline after its labels line, so the
string concatenation in `seccompPoolYAML` produces a valid second pool entry.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'Isolation|Seccomp|QEMUPoolHasNo' -v`
Expected: compile error — `c.Pools[0].Isolation` and `HasDockerIsolation` undefined.

- [ ] **Step 3: Implement**

In `internal/config/config.go`:

1. Add `"path/filepath"` to the imports.

2. In the `Pool` struct, after the `Backend` field, add:

```go
	// Isolation selects the sandbox for docker pools: "gvisor" (default;
	// runsc + --privileged, full Docker-in-job) or "seccomp" (native runc,
	// no --privileged, Docker's default seccomp profile; no Docker inside
	// the job). Only valid on backend: docker pools.
	Isolation string `yaml:"isolation"`
	// SeccompProfile optionally replaces Docker's built-in default seccomp
	// profile with a custom one (absolute path on the host). Only valid
	// with Isolation "seccomp"; empty means the built-in default.
	SeccompProfile string `yaml:"seccomp_profile"`
```

3. In `applyDefaults`, inside the `for i := range c.Pools` loop, after the
   `p.Backend == ""` default:

```go
		if p.Backend == "docker" && p.Isolation == "" {
			p.Isolation = "gvisor"
		}
```

4. In `validate`, inside the pool loop, right after the existing
   `if p.Backend != "qemu" && p.Backend != "docker"` check:

```go
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
```

5. Next to `HasBackend` at the bottom of the file, add:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: all PASS (including all pre-existing tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): per-pool isolation and seccomp_profile for docker pools"
```

---

### Task 2: dockerbackend — `SlimImage` and `RunArgs` branch

**Files:**
- Modify: `internal/dockerbackend/docker.go`
- Test: `internal/dockerbackend/docker_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/dockerbackend/docker_test.go`:

```go
func TestRunArgsSeccomp(t *testing.T) {
	p := config.Pool{CPUs: 4, MemoryMB: 4096, Isolation: "seccomp"}
	got := RunArgs("ghq-fast-ab12", "runsc", p, "/var/lib/x/run/ghq-fast-ab12/jit")
	want := []string{
		"run", "--detach",
		"--name", "ghq-fast-ab12",
		"--runtime", "runc",
		"--cap-drop", "NET_RAW", "--cap-drop", "MKNOD",
		"--cpus", "4",
		"--memory", "4096m",
		"--label", "ghq.managed=true",
		"--volume", "/var/lib/x/run/ghq-fast-ab12/jit:/jit:ro",
		"ghq-runner-slim:latest",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunArgs(seccomp):\n got %q\nwant %q", got, want)
	}
}

func TestRunArgsSeccompCustomProfile(t *testing.T) {
	p := config.Pool{CPUs: 1, MemoryMB: 512, Isolation: "seccomp",
		SeccompProfile: "/etc/ghq/strict.json"}
	got := strings.Join(RunArgs("ghq-x-y", "runsc", p, "/run/jit"), " ")
	if !strings.Contains(got, "--security-opt seccomp=/etc/ghq/strict.json") {
		t.Errorf("custom profile missing from argv: %s", got)
	}
	if strings.Contains(got, "--privileged") {
		t.Errorf("seccomp isolation must never be privileged: %s", got)
	}
	if strings.Contains(got, "runsc") {
		t.Errorf("seccomp isolation must ignore docker.runtime: %s", got)
	}
}
```

Note the deliberate pass of `"runsc"` as the runtime argument in both tests:
seccomp pools must pin `runc` regardless of `docker.runtime`. The existing
`TestRunArgs` (gvisor path, `Isolation` zero-value) must keep passing
unchanged — it is the backward-compatibility golden test.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/dockerbackend/ -run RunArgs -v`
Expected: `TestRunArgsSeccomp` FAILs (argv has `--runtime runsc --privileged`
and `ghq-runner-base:latest`); `TestRunArgs` PASSes.

- [ ] **Step 3: Implement**

In `internal/dockerbackend/docker.go`, add below the `Image` const:

```go
// SlimImage is the Docker-Engine-free runner image for isolation: seccomp
// pools (see Bake).
const SlimImage = "ghq-runner-slim:latest"
```

Replace the `RunArgs` function (and its doc comment) with:

```go
// RunArgs builds the `docker run` argv for one job container, branching on
// the pool's isolation mode.
//
// gvisor: --privileged is required by the inner dockerd (DinD); under
// runtime runsc it grants capabilities inside gVisor's sandbox, not on the
// host.
//
// seccomp: native runc WITHOUT --privileged, so Docker's default seccomp
// profile and capability bounding apply (NET_RAW and MKNOD dropped on
// top); docker.runtime is ignored — pinning runc is the performance point.
//
// jitDir must be an absolute path without ':' (the bind-mount spec is
// colon-delimited).
func RunArgs(name, runtime string, p config.Pool, jitDir string) []string {
	args := []string{"run", "--detach", "--name", name}
	image := Image
	if p.Isolation == "seccomp" {
		image = SlimImage
		args = append(args, "--runtime", "runc",
			"--cap-drop", "NET_RAW", "--cap-drop", "MKNOD")
		if p.SeccompProfile != "" {
			args = append(args, "--security-opt", "seccomp="+p.SeccompProfile)
		}
	} else {
		args = append(args, "--runtime", runtime, "--privileged")
	}
	return append(args,
		"--cpus", strconv.Itoa(p.CPUs),
		"--memory", strconv.Itoa(p.MemoryMB)+"m",
		"--label", managedLabel,
		"--volume", jitDir+":/jit:ro",
		image,
	)
}
```

No `Provisioner` changes: `Provision` already passes the full `config.Pool`
through to `RunArgs`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/dockerbackend/ -v`
Expected: all PASS (including `TestRunArgs` and `TestProvisionLifecycle`).

- [ ] **Step 5: Commit**

```bash
git add internal/dockerbackend/docker.go internal/dockerbackend/docker_test.go
git commit -m "feat(dockerbackend): seccomp-isolation run args and slim image tag"
```

---

### Task 3: Image assets — slim entrypoint, multi-stage Dockerfile, embeds

**Files:**
- Create: `scripts/docker/entrypoint-slim.sh`
- Modify: `scripts/docker/Dockerfile`
- Modify: `scripts/embed.go`
- Test: `scripts/embed_test.go`

- [ ] **Step 1: Write the failing tests**

In `scripts/embed_test.go`, append to the body of `TestDockerAssetsEmbedded`:

```go
	if !strings.Contains(Dockerfile, "FROM ubuntu:24.04 AS base") ||
		!strings.Contains(Dockerfile, "FROM base AS dind") ||
		!strings.Contains(Dockerfile, "FROM base AS slim") {
		t.Error("Dockerfile must define dind and slim build stages sharing a common base stage")
	}
	if !strings.Contains(DockerEntrypointSlim, "runuser -u runner") {
		t.Error("slim entrypoint must drop privileges via runuser -u runner before exec'ing run.sh")
	}
	if !strings.Contains(DockerEntrypointSlim, "--jitconfig") {
		t.Error("slim entrypoint must pass the JIT config to run.sh")
	}
	if strings.Contains(DockerEntrypointSlim, "dockerd") {
		t.Error("slim entrypoint must not start dockerd (seccomp pools run without --privileged; DinD is gvisor-pool-only)")
	}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./scripts/ -v`
Expected: compile error — `DockerEntrypointSlim` undefined.

- [ ] **Step 3: Create `scripts/docker/entrypoint-slim.sh`**

```bash
#!/usr/bin/env bash
# PID 1 of the ephemeral job container for isolation: seccomp pools (run
# with --runtime=runc, NO --privileged — Docker's default seccomp profile
# applies). No inner dockerd: jobs that need Docker belong on a gvisor
# pool. Runs exactly one GitHub Actions job as the unprivileged "runner"
# user; the container exits when the job finishes — host-side teardown
# depends on that exit.
set -uo pipefail

JIT_FILE=/jit/config
if [ ! -s "$JIT_FILE" ]; then
  echo "entrypoint: $JIT_FILE missing or empty" >&2
  exit 1
fi

cd /opt/actions-runner || exit 1
# The JIT config registers a pre-created ephemeral runner; run.sh executes
# one job, deregisters, and exits. The blob is single-use, so its
# visibility in container-internal argv is harmless.
exec runuser -u runner -- ./run.sh --jitconfig "$(cat "$JIT_FILE")"
```

- [ ] **Step 4: Rewrite `scripts/docker/Dockerfile` as multi-stage**

Replace the entire file with:

```dockerfile
# Runner images for the docker backend, built locally by
# `github-qemu-runner refresh-image`; never pushed to a registry.
#   --target dind -> ghq-runner-base:latest  (Docker Engine inside, for
#       isolation: gvisor pools; run with --runtime runsc --privileged)
#   --target slim -> ghq-runner-slim:latest  (no Docker Engine, for
#       isolation: seccomp pools; native runc, no --privileged)
FROM ubuntu:24.04 AS base
ARG RUNNER_VERSION
ARG RUNNER_TARBALL_URL
ARG RUNNER_TARBALL_SHA256

LABEL org.opencontainers.image.title="ghq-runner-base" \
      org.opencontainers.image.version="${RUNNER_VERSION}"

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl git sudo && \
    rm -rf /var/lib/apt/lists/*

# uid 1001: ubuntu:24.04 ships an "ubuntu" user at 1000. Passwordless sudo
# matches GitHub-hosted images; the disposable sandbox is the trust
# boundary.
RUN useradd --create-home --uid 1001 runner && \
    echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner && \
    chmod 0440 /etc/sudoers.d/runner

RUN mkdir -p /opt/actions-runner && cd /opt/actions-runner && \
    curl -fsSL -o runner.tar.gz "$RUNNER_TARBALL_URL" && \
    { if [ -z "$RUNNER_TARBALL_SHA256" ]; then echo "WARNING: RUNNER_TARBALL_SHA256 empty; skipping tarball integrity check" >&2; else echo "$RUNNER_TARBALL_SHA256  runner.tar.gz" | sha256sum -c -; fi; } && \
    tar xzf runner.tar.gz && rm runner.tar.gz && \
    apt-get update && ./bin/installdependencies.sh && rm -rf /var/lib/apt/lists/* && \
    chown -R runner:runner /opt/actions-runner

# DinD variant: Docker Engine + entrypoint that boots the inner dockerd.
# runner joins the docker group (root-equivalent inside the container).
FROM base AS dind
RUN install -m 0755 -d /etc/apt/keyrings && \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc && \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" \
        > /etc/apt/sources.list.d/docker.list && \
    apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin && \
    rm -rf /var/lib/apt/lists/* && \
    usermod -aG docker runner

COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh

# Anonymous volume so inner-docker state never lands on the container's
# overlay; removed with the container (docker rm --volumes).
VOLUME /var/lib/docker

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

# Slim variant: no Docker Engine. A job step that runs `docker` fails with
# command-not-found, which is the accurate error for a seccomp pool.
FROM base AS slim
COPY --chmod=0755 entrypoint-slim.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
```

Behavior-preserving notes: the dind stage's final filesystem matches today's
image (same packages, same user/groups, same entrypoint path); only the
instruction grouping changed (`usermod -aG docker` moved next to the
docker-ce install because the base stage has no docker group).

- [ ] **Step 5: Add the embed**

In `scripts/embed.go`, append:

```go
//go:embed docker/entrypoint-slim.sh
var DockerEntrypointSlim string
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./scripts/ -v && shellcheck scripts/docker/entrypoint-slim.sh && shfmt -d scripts/docker/entrypoint-slim.sh`
Expected: tests PASS, shellcheck/shfmt clean.

- [ ] **Step 7: Commit**

```bash
git add scripts/docker/entrypoint-slim.sh scripts/docker/Dockerfile scripts/embed.go scripts/embed_test.go
git commit -m "feat(images): multi-stage Dockerfile with docker-engine-free slim variant"
```

---

### Task 4: Bake variants + `refresh-image` wiring

**Files:**
- Modify: `internal/dockerbackend/bake.go`
- Modify: `cmd/github-qemu-runner/main.go:85-98` (`runRefreshImage`)
- Test: `internal/dockerbackend/bake_test.go`

- [ ] **Step 1: Update the existing test and add the variants test**

In `internal/dockerbackend/bake_test.go`:

1. In `TestBake`, extend the argv expectations and context-file list:

```go
	for _, want := range []string{
		"build --pull",
		"--target dind",
		"--build-arg RUNNER_VERSION=2.335.1",
		"--tag " + Image,
	} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("docker build argv missing %q:\n%s", want, argv)
		}
	}
	if strings.Contains(string(argv), "--target slim") {
		t.Error("default bake must build only the dind variant")
	}
	for _, f := range []string{"Dockerfile", "entrypoint.sh", "entrypoint-slim.sh"} {
		if _, err := os.Stat(filepath.Join(dir, "context", f)); err != nil {
			t.Errorf("build context missing %s: %v", f, err)
		}
	}
```

2. Change the provenance decode in `TestBake` from `map[string]string` to
   `map[string]any` (the `meta["runner_version"] != "2.335.1"` comparison
   still works on `any`), and add:

```go
	if got := fmt.Sprint(meta["variants"]); got != "[dind]" {
		t.Errorf("provenance variants = %s, want [dind]", got)
	}
```

   Add `"fmt"` to the test file's imports.

3. Append a new test (the fake-docker setup is copied from `TestBake` so the
   test stays self-contained):

```go
func TestBakeBothVariants(t *testing.T) {
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
	script := `#!/bin/sh
echo "$@" >> ` + argvLog + `
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
		Variants:  []string{"dind", "slim"},
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	argv, _ := os.ReadFile(argvLog)
	for _, want := range []string{
		"--target dind",
		"--tag " + Image,
		"--target slim",
		"--tag " + SlimImage,
	} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("docker build argv missing %q:\n%s", want, argv)
		}
	}

	var meta map[string]any
	b, err := os.ReadFile(filepath.Join(stateDir, "images", "docker-base.json"))
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(meta["variants"]); got != "[dind slim]" {
		t.Errorf("provenance variants = %s, want [dind slim]", got)
	}
}

func TestBakeRejectsUnknownVariant(t *testing.T) {
	err := Bake(context.Background(), BakeOptions{
		StateDir:  t.TempDir(),
		DockerBin: "/bin/false",
		Variants:  []string{"fat"},
		Log:       slog.New(slog.DiscardHandler),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown image variant") {
		t.Fatalf("want unknown-variant error, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/dockerbackend/ -run Bake -v`
Expected: compile error — `Variants` field undefined.

- [ ] **Step 3: Implement in `internal/dockerbackend/bake.go`**

1. Add to `BakeOptions`:

```go
	// Variants selects which Dockerfile targets to build: "dind"
	// (-> Image, for gvisor pools) and/or "slim" (-> SlimImage, for
	// seccomp pools). Empty means ["dind"].
	Variants []string
```

2. In `(*BakeOptions) defaults()`, add:

```go
	if len(o.Variants) == 0 {
		o.Variants = []string{"dind"}
	}
```

3. In `Bake`, validate variants right after `o.defaults()` (before the
   network call, so a bad variant fails fast):

```go
	tags := map[string]string{"dind": Image, "slim": SlimImage}
	for _, v := range o.Variants {
		if _, ok := tags[v]; !ok {
			return fmt.Errorf("unknown image variant %q", v)
		}
	}
```

4. Write the slim entrypoint into the build context, next to the existing
   `entrypoint.sh` write:

```go
	if err := os.WriteFile(filepath.Join(buildDir, "entrypoint-slim.sh"), []byte(scripts.DockerEntrypointSlim), 0o755); err != nil {
		return err
	}
```

5. Replace the single `docker build` invocation (the `cmd := ...` through
   `cmd.Run()` block, and the preceding "building runner image" log line)
   with a loop:

```go
	for _, v := range o.Variants {
		tag := tags[v]
		o.Log.Info("building runner image", "runner_version", rel.Version, "variant", v, "tag", tag)
		cmd := exec.CommandContext(ctx, o.DockerBin, "build", "--pull",
			"--target", v,
			"--build-arg", "RUNNER_VERSION="+rel.Version,
			"--build-arg", "RUNNER_TARBALL_URL="+rel.TarballURL,
			"--build-arg", "RUNNER_TARBALL_SHA256="+rel.SHA256,
			"--tag", tag, buildDir)
		cmd.Stdout = os.Stderr // long build; stream progress instead of buffering
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("docker build %s: %w", v, err)
		}
	}
```

6. Change the provenance map to `map[string]any` and add the variants:

```go
	meta, err := json.MarshalIndent(map[string]any{
		"runner_version": rel.Version,
		"arch":           RunnerArch(runtime.GOARCH),
		"variants":       o.Variants,
		"baked_at":       time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
```

7. Change the final log line to `o.Log.Info("runner images built", "variants", o.Variants)`.

- [ ] **Step 4: Wire `runRefreshImage` in `cmd/github-qemu-runner/main.go`**

Inside the `if cfg.HasBackend("docker")` block, compute variants and pass
them:

```go
	if cfg.HasBackend("docker") {
		dockerBin, err := exec.LookPath("docker")
		if err != nil {
			return err
		}
		var variants []string
		if cfg.HasDockerIsolation("gvisor") {
			variants = append(variants, "dind")
		}
		if cfg.HasDockerIsolation("seccomp") {
			variants = append(variants, "slim")
		}
		if err := dockerbackend.Bake(ctx, dockerbackend.BakeOptions{
			StateDir:  cfg.StateDir,
			APIBase:   cfg.GitHub.APIBaseURL,
			DockerBin: dockerBin,
			Variants:  variants,
			Log:       log,
		}); err != nil {
			return err
		}
	}
```

(A docker backend pool always has a non-empty isolation after
`applyDefaults`, so `variants` is never empty here.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/dockerbackend/ -v && go build ./...`
Expected: all PASS, build clean.

- [ ] **Step 6: Commit**

```bash
git add internal/dockerbackend/bake.go internal/dockerbackend/bake_test.go cmd/github-qemu-runner/main.go
git commit -m "feat(bake): build dind/slim image variants per configured isolation"
```

---

### Task 5: Controller preflight — per-variant image check, profile existence

**Files:**
- Modify: `internal/controller/controller.go:55-72`

No new unit tests: this block shells out to `docker` found via `LookPath` and
has no existing test coverage; the gating logic it consumes
(`HasDockerIsolation`) is unit-tested in Task 1. Verify by build + full test
suite.

- [ ] **Step 1: Implement**

Replace the body of the `if cfg.HasBackend("docker")` block in
`controller.Run` with:

```go
	var dockerProv *dockerbackend.Provisioner
	if cfg.HasBackend("docker") {
		dockerBin, err := exec.LookPath("docker")
		if err != nil {
			return fmt.Errorf("docker not found: %w", err)
		}
		for _, v := range []struct {
			image string
			used  bool
		}{
			{dockerbackend.Image, cfg.HasDockerIsolation("gvisor")},
			{dockerbackend.SlimImage, cfg.HasDockerIsolation("seccomp")},
		} {
			if !v.used {
				continue
			}
			if err := exec.CommandContext(ctx, dockerBin, "image", "inspect", v.image).Run(); err != nil {
				return fmt.Errorf("runner image %s missing (run `github-qemu-runner refresh-image` first): %w", v.image, err)
			}
		}
		// Fail at startup, not at the first job, on a missing custom profile.
		for _, p := range cfg.Pools {
			if p.SeccompProfile == "" {
				continue
			}
			if _, err := os.Stat(p.SeccompProfile); err != nil {
				return fmt.Errorf("pool %s: seccomp_profile: %w", p.Name, err)
			}
		}
		if cfg.Docker.Runtime == "runc" && cfg.HasDockerIsolation("gvisor") {
			log.Warn("gvisor-isolation pools run WITHOUT gVisor (docker.runtime: runc): " +
				"--privileged DinD containers have effectively no isolation boundary")
		}
		// Containers reference jit mounts under runDir, so reap them
		// before ReapOrphans deletes the workdirs.
		dockerbackend.ReapContainers(ctx, dockerBin, log)
		dockerProv = &dockerbackend.Provisioner{RunDir: runDir, DockerBin: dockerBin, Runtime: cfg.Docker.Runtime}
	}
```

(Two changes vs. today: the image check became a per-variant loop, the
profile-existence loop is new, and the runc warning is now conditioned on a
gvisor pool actually existing — on a seccomp-only host `docker.runtime` is
inert and the warning would be noise.)

- [ ] **Step 2: Verify**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: clean build, all tests PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/controller.go
git commit -m "feat(controller): isolation-aware image and seccomp-profile preflights"
```

---

### Task 6: `setup` — isolation-aware checks

**Files:**
- Modify: `cmd/github-qemu-runner/main.go:131-157` (`runSetup`, docker block)

No new unit tests (main package, no test infrastructure; gating logic tested
via `HasDockerIsolation` in Task 1). Verify by build + full suite.

- [ ] **Step 1: Implement**

Replace the `if cfg.HasBackend("docker")` block in `runSetup` with:

```go
	if cfg.HasBackend("docker") {
		dockerBin, lookErr := exec.LookPath("docker")
		check("docker on PATH", lookErr)
		if lookErr == nil {
			check("docker daemon reachable", exec.CommandContext(ctx, dockerBin, "info").Run())
			if cfg.HasDockerIsolation("gvisor") {
				if cfg.Docker.Runtime == "runsc" {
					out, err := exec.CommandContext(ctx, dockerBin, "info", "--format", "{{json .Runtimes}}").Output()
					if err == nil && !strings.Contains(string(out), `"runsc"`) {
						err = fmt.Errorf(`runsc not in docker runtimes — register it in /etc/docker/daemon.json with runtimeArgs ["--net-raw","--allow-packet-socket-write"], then restart docker`)
					}
					check("runsc runtime registered", err)
				} else {
					fmt.Printf("warn  docker.runtime is runc: gvisor-isolation pools run WITHOUT gVisor; --privileged DinD has effectively no isolation boundary\n")
				}
			}
			// One image + outbound-connectivity check per isolation mode in
			// use, each with its matching runtime and image. Catches
			// host-firewall problems (e.g. OCI's default inet-filter forward
			// DROP) that only bite inside containers.
			type modeCheck struct{ runtime, image string }
			var modes []modeCheck
			if cfg.HasDockerIsolation("gvisor") {
				modes = append(modes, modeCheck{cfg.Docker.Runtime, dockerbackend.Image})
			}
			if cfg.HasDockerIsolation("seccomp") {
				modes = append(modes, modeCheck{"runc", dockerbackend.SlimImage})
			}
			for _, m := range modes {
				if err := exec.CommandContext(ctx, dockerBin, "image", "inspect", m.image).Run(); err != nil {
					fmt.Printf("note  runner image %s missing; run `github-qemu-runner refresh-image`\n", m.image)
					continue
				}
				fmt.Printf("ok    runner image %s\n", m.image)
				check("container outbound connectivity ("+m.image+")", exec.CommandContext(ctx, dockerBin,
					"run", "--rm", "--runtime", m.runtime, "--entrypoint", "curl",
					m.image, "-fsS", "--max-time", "30",
					"-o", "/dev/null", "https://api.github.com").Run())
			}
		}
	}
```

(The slim image includes curl — installed in the shared base stage — so the
`--entrypoint curl` connectivity probe works for both images.)

- [ ] **Step 2: Verify**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: clean build, all tests PASS.

- [ ] **Step 3: Commit**

```bash
git add cmd/github-qemu-runner/main.go
git commit -m "feat(setup): gate runsc check on gvisor pools; per-isolation connectivity checks"
```

---

### Task 7: Docs — README and example config

**Files:**
- Modify: `README.md` (Docker backend section)
- Modify: `packaging/config.example.yaml`

- [ ] **Step 1: README**

In the "## Docker backend (hosts without /dev/kvm)" section, after the
existing security trade-off paragraph (the one starting "Security trade-off,
explicitly:"), insert a new subsection:

````markdown
### Seccomp isolation mode (higher performance, no Docker-in-job)

Docker pools that don't need Docker inside the job (build / test / lint
workloads) can opt into `isolation: seccomp` per pool:

```yaml
pools:
  - name: fast
    backend: docker
    isolation: seccomp   # gvisor (default) | seccomp
    # seccomp_profile: /etc/ghq/strict.json   # optional custom profile
    ...
```

The job container then runs under native `runc` **without** `--privileged`,
so Docker's default seccomp profile and capability bounding apply (plus
`NET_RAW`/`MKNOD` dropped — `ping` won't work inside jobs). This removes
gVisor's syscall-interception overhead entirely; gVisor is not even required
on the host if every docker pool uses seccomp isolation.

Trade-offs, explicitly:

- **Weaker than gVisor:** the job shares the host kernel behind the standard
  container boundary (namespaces + cgroups + seccomp allowlist + capability
  bounding). A kernel 0-day reachable through allowlisted syscalls escapes.
  Isolation ladder: qemu > gvisor > seccomp > `docker.runtime: runc`
  (privileged + unconfined — note seccomp mode is strictly *stronger* than
  that escape hatch).
- **No Docker inside jobs:** `container:` jobs, service containers, and
  `docker build` fail (the slim image `ghq-runner-slim:latest` ships no
  Docker Engine). Keep such jobs on a gvisor pool — one host can run both.
- `sudo`/`apt-get` keep working (GitHub-hosted parity); `docker.runtime` is
  ignored by seccomp pools.
- `seccomp_profile` (absolute path) swaps in a custom profile instead of
  Docker's built-in default; it can tighten the sandbox, never disable it.
````

Also update the feature-list bullet "Optional [Docker backend](...)" to
mention the mode, e.g. append: "with per-pool `isolation: gvisor | seccomp`
(seccomp = native-runc speed for jobs that don't need Docker inside)".

- [ ] **Step 2: Example config**

In `packaging/config.example.yaml`, extend the commented docker pool example:

```yaml
  #- name: oci-arm
  #  backend: docker     # qemu (default) | docker
  #  isolation: gvisor   # gvisor (default; DinD via runsc) | seccomp
  #                      # (native runc + Docker's default seccomp profile;
  #                      # faster, but NO docker inside jobs — see README)
  #  # seccomp_profile: /etc/ghq/strict.json  # optional custom profile
  #  scope: org
  #  org: my-org
  #  count: 2
  #  cpus: 2
  #  memory_mb: 8192
  #  disk_gb: 30         # advisory only on docker pools (not enforced)
  #  labels: [self-hosted, linux, arm64, oci]
```

- [ ] **Step 3: Verify and commit**

Run: `go test ./...` (docs-only change; confirms nothing else broke)

```bash
git add README.md packaging/config.example.yaml
git commit -m "docs: document seccomp isolation mode for docker pools"
```

---

### Task 8: Final verification

- [ ] **Step 1: Full suite + linters**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: gofmt prints nothing, vet clean, all tests PASS.

- [ ] **Step 2: Spec cross-check**

Re-read `docs/superpowers/specs/2026-06-13-seccomp-isolation-design.md` and
confirm each section maps to landed code: config (Task 1), container launch
(Task 2), slim image (Task 3), bake variants (Task 4), controller preflight
(Task 5), setup (Task 6), docs (Task 7). Fix any gap before proceeding.

- [ ] **Step 3: Manual integration smoke (deferred to a docker-capable host)**

Not executable in this workspace; record as a follow-up for the deployment
host: `refresh-image` with one seccomp pool configured → run a trivial
workflow containing `sudo apt-get install -y jq` (proves the sudo path) and
a `docker ps` step expected to fail with command-not-found → confirm clean
teardown.
