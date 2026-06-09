# github-qemu-runner Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go binary + systemd service that runs ephemeral GitHub Actions runners, one disposable QEMU/KVM VM per job, per `docs/superpowers/specs/2026-06-10-qemu-runner-design.md`.

**Architecture:** `controller` subcommand supervises static pools of slots; each slot loop: JIT-register → qcow2 overlay + NoCloud seed ISO → boot QEMU child process → wait for guest poweroff → teardown → repeat. `refresh-image` bakes the Ubuntu 24.04 + Docker + actions-runner base image. `setup` is preflight.

**Tech Stack:** Go 1.26 (stdlib + `gopkg.in/yaml.v3` only), qemu-system-x86_64/qemu-img, genisoimage, cloud-init NoCloud, GitHub REST API (App auth, hand-rolled RS256 JWT), systemd.

**Prerequisite:** The repo-setup plan (`2026-06-10-repo-setup.md`) is complete: mise toolchain, lefthook, golangci-lint, CI all working. Module is `github.com/a1678991/github-qemu-runner`. All commands run from the repo root.

**Conventions for every task:** lefthook runs lint/format on commit — if a commit is rejected, fix the reported issue, re-stage, retry. Tests gated on external binaries use `t.Skip` when the binary is absent (CI installs `qemu-utils` + `genisoimage`; qemu-system tests use a fake binary and never boot real VMs).

## File map

| Path | Responsibility |
|---|---|
| `internal/config/config.go` | YAML config: types, defaults, validation, capacity warnings |
| `internal/github/jwt.go` | RS256 App JWT mint + PEM key parsing (no deps) |
| `internal/github/client.go` | REST client: installation-token cache, JIT config, runner CRUD, runner groups |
| `internal/seed/seed.go` | NoCloud user-data/meta-data rendering + seed ISO via genisoimage |
| `internal/qemu/qemu.go` | Overlay creation, QEMU argv builder, VM process supervision |
| `internal/qemu/qmp.go` | Minimal QMP client (`system_powerdown`) |
| `scripts/guest/run-one-job.sh` | In-guest: run one job as `runner` user, then poweroff (always) |
| `scripts/guest/bake.sh` | In-guest: install Docker + runner during image bake |
| `scripts/embed.go` | `go:embed` the guest scripts (kept as real files for shellcheck) |
| `internal/pool/pool.go` | Slot supervisor: JIT → provision → liveness gate → wait → teardown; backoff; drain |
| `internal/controller/provision.go` | Real Provisioner: workdir + overlay + seed + qemu.Start |
| `internal/controller/reap.go` | Startup orphan reaping (processes, workdirs, offline runner records) |
| `internal/controller/controller.go` | Wire-up: config → client → pools; signal-driven shutdown |
| `internal/imagebake/bake.go` | Image download/verify, runner release resolve, bake boot, flatten+swap |
| `cmd/github-qemu-runner/main.go` | Subcommand dispatch: controller / refresh-image / setup |
| `packaging/github-qemu-runner.service` | Hardened systemd unit |
| `packaging/config.example.yaml` | Annotated example config |
| `README.md` | Install, setup, runbook |

---

### Task 1: config package

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Add the yaml dependency**

Run: `go get gopkg.in/yaml.v3@latest`
Expected: `go.mod` gains `require gopkg.in/yaml.v3 v3.x.x`.

- [ ] **Step 2: Write the failing tests**

`internal/config/config_test.go`:

```go
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
		{"bad scope", strings.Replace(validYAML, "scope: org", "scope: enterprise", 1),
			`scope must be "org" or "repo"`},
		{"repo without owner/name", strings.NewReplacer(
			"scope: org", "scope: repo", "org: my-org", "repo: justname",
		).Replace(validYAML), "owner/name"},
		{"bad pool name", strings.Replace(validYAML, "name: fmt", "name: FMT_pool", 1),
			"pool name"},
		{"duplicate pool name", validYAML + strings.TrimPrefix(strings.ReplaceAll(validYAML, "github:", "ignore:"), "\n"),
			""}, // replaced below — see note
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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: Load` (package doesn't compile yet).

- [ ] **Step 4: Write the implementation**

`internal/config/config.go`:

```go
// Package config loads and validates the controller's YAML configuration.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
	GitHub   GitHub `yaml:"github"`
	StateDir string `yaml:"state_dir"`
	Pools    []Pool `yaml:"pools"`
}

type GitHub struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	APIBaseURL     string `yaml:"api_base_url"`
}

type Pool struct {
	Name            string   `yaml:"name"`
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
	// Lets the systemd unit pass the App key via LoadCredential:
	// private_key_path: ${CREDENTIALS_DIRECTORY}/app-key.pem
	c.GitHub.PrivateKeyPath = os.ExpandEnv(c.GitHub.PrivateKeyPath)
	for i := range c.Pools {
		p := &c.Pools[i]
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
	seen := map[string]bool{}
	for _, p := range c.Pools {
		if !poolNameRe.MatchString(p.Name) {
			return fmt.Errorf("pool name %q must match %s", p.Name, poolNameRe)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		seen[p.Name] = true
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
		if p.MemoryMB < 256 {
			return fmt.Errorf("pool %s: memory_mb must be >= 256", p.Name)
		}
		if p.DiskGB < 10 {
			return fmt.Errorf("pool %s: disk_gb must be >= 10", p.Name)
		}
		if len(p.Labels) == 0 {
			return fmt.Errorf("pool %s: at least one label is required", p.Name)
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/`
Expected: PASS (all tests).

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/config/
git commit -m "feat: add config package with validation and defaults"
```

---

### Task 2: GitHub App JWT

**Files:**
- Create: `internal/github/jwt.go`
- Test: `internal/github/jwt_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/github/jwt_test.go`:

```go
package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestMintJWT(t *testing.T) {
	key := testKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok, err := mintJWT(42, key, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("want 3 JWT parts, got %d", len(parts))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "42" {
		t.Errorf("iss = %q", claims.Iss)
	}
	if claims.Iat != now.Unix()-60 {
		t.Errorf("iat = %d", claims.Iat)
	}
	if claims.Exp != now.Unix()+540 {
		t.Errorf("exp = %d", claims.Exp)
	}
}

func TestParseRSAPrivateKey(t *testing.T) {
	key := testKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})

	for name, blob := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		if _, err := ParseRSAPrivateKey(blob); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseRSAPrivateKey([]byte("not a key")); err == nil {
		t.Error("garbage input: want error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/github/`
Expected: FAIL — `undefined: mintJWT`, `undefined: ParseRSAPrivateKey`.

- [ ] **Step 3: Write the implementation**

`internal/github/jwt.go`:

```go
// Package github is a minimal GitHub REST client for App-authenticated
// ephemeral-runner management. Only the handful of endpoints the controller
// needs — no SDK dependency.
package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// ParseRSAPrivateKey parses a GitHub App private key in PKCS#1 or PKCS#8 PEM.
func ParseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found in private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rk, nil
}

// mintJWT builds the short-lived RS256 App JWT GitHub requires for
// installation-token requests (iat 60s in the past for clock skew, exp
// 9 minutes out — under GitHub's 10-minute cap).
func mintJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	b64 := base64.RawURLEncoding
	header := b64.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": fmt.Sprintf("%d", appID),
	})
	if err != nil {
		return "", err
	}
	unsigned := header + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + b64.EncodeToString(sig), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/github/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/github/
git commit -m "feat: add GitHub App JWT minting and key parsing"
```

---

### Task 3: GitHub REST client

**Files:**
- Create: `internal/github/client.go`
- Test: `internal/github/client_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/github/client_test.go`:

```go
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a Client pointed at a test server plus the mux to
// add endpoint handlers to. The token endpoint is pre-wired and counts mints.
func newTestClient(t *testing.T) (*Client, *http.ServeMux, *atomic.Int32) {
	t.Helper()
	mux := http.NewServeMux()
	var mints atomic.Int32
	mux.HandleFunc("POST /app/installations/7/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mints.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("itok-%d", mints.Load()),
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL, 42, 7, testKey(t))
	return c, mux, &mints
}

func TestInstallationTokenCached(t *testing.T) {
	c, mux, mints := newTestClient(t)
	mux.HandleFunc("GET /orgs/o/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer itok-1" {
			t.Errorf("Authorization = %q", got)
		}
		json.NewEncoder(w).Encode(Runner{ID: 5, Status: "online"})
	})
	ctx := context.Background()
	for range 3 {
		if _, err := c.GetRunner(ctx, "orgs/o", 5); err != nil {
			t.Fatal(err)
		}
	}
	if mints.Load() != 1 {
		t.Errorf("token minted %d times, want 1", mints.Load())
	}
}

func TestInstallationTokenRefreshAfterExpiry(t *testing.T) {
	c, mux, mints := newTestClient(t)
	mux.HandleFunc("GET /orgs/o/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Runner{ID: 5})
	})
	ctx := context.Background()
	if _, err := c.GetRunner(ctx, "orgs/o", 5); err != nil {
		t.Fatal(err)
	}
	c.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := c.GetRunner(ctx, "orgs/o", 5); err != nil {
		t.Fatal(err)
	}
	if mints.Load() != 2 {
		t.Errorf("token minted %d times, want 2", mints.Load())
	}
}

func TestGenerateJITConfig(t *testing.T) {
	c, mux, _ := newTestClient(t)
	mux.HandleFunc("POST /repos/o/r/actions/runners/generate-jitconfig", func(w http.ResponseWriter, r *http.Request) {
		var req JITRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Name != "ghq-fmt-abc" || req.RunnerGroupID != 1 || len(req.Labels) != 2 {
			t.Errorf("unexpected request: %+v", req)
		}
		json.NewEncoder(w).Encode(JITResult{
			Runner:           Runner{ID: 99, Name: req.Name},
			EncodedJITConfig: "blob123",
		})
	})
	got, err := c.GenerateJITConfig(context.Background(), "repos/o/r", JITRequest{
		Name: "ghq-fmt-abc", RunnerGroupID: 1, Labels: []string{"self-hosted", "fmt"}, WorkFolder: "_work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner.ID != 99 || got.EncodedJITConfig != "blob123" {
		t.Errorf("got %+v", got)
	}
}

func TestDeleteRunnerNotFound(t *testing.T) {
	c, mux, _ := newTestClient(t)
	mux.HandleFunc("DELETE /orgs/o/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	err := c.DeleteRunner(context.Background(), "orgs/o", 5)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListRunnersPaginates(t *testing.T) {
	c, mux, _ := newTestClient(t)
	mux.HandleFunc("GET /orgs/o/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		var runners []Runner
		if page == "1" {
			for i := range 100 {
				runners = append(runners, Runner{ID: int64(i)})
			}
		} else {
			runners = []Runner{{ID: 100}}
		}
		json.NewEncoder(w).Encode(map[string]any{"runners": runners})
	})
	got, err := c.ListRunners(context.Background(), "orgs/o")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 101 {
		t.Errorf("got %d runners, want 101", len(got))
	}
}

func TestRunnerGroupID(t *testing.T) {
	c, mux, _ := newTestClient(t)
	mux.HandleFunc("GET /orgs/o/actions/runner-groups", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"runner_groups": []map[string]any{
				{"id": 1, "name": "Default"},
				{"id": 5, "name": "heavy"},
			},
		})
	})
	id, err := c.RunnerGroupID(context.Background(), "orgs/o", "heavy")
	if err != nil {
		t.Fatal(err)
	}
	if id != 5 {
		t.Errorf("id = %d", id)
	}
	if _, err := c.RunnerGroupID(context.Background(), "orgs/o", "absent"); err == nil {
		t.Error("want error for unknown group")
	}
	// repo scope never calls the API
	id, err = c.RunnerGroupID(context.Background(), "repos/o/r", "Default")
	if err != nil || id != 1 {
		t.Errorf("repo scope: id=%d err=%v", id, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/github/`
Expected: FAIL — `undefined: New`, `undefined: Client`, etc.

- [ ] **Step 3: Write the implementation**

`internal/github/client.go`:

```go
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned for 404 responses, e.g. deleting an
// already-deregistered runner.
var ErrNotFound = errors.New("not found")

type Client struct {
	BaseURL        string
	HTTP           *http.Client
	AppID          int64
	InstallationID int64
	Key            *rsa.PrivateKey
	Now            func() time.Time

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func New(baseURL string, appID, installationID int64, key *rsa.PrivateKey) *Client {
	return &Client{
		BaseURL:        strings.TrimRight(baseURL, "/"),
		HTTP:           &http.Client{Timeout: 30 * time.Second},
		AppID:          appID,
		InstallationID: installationID,
		Key:            key,
		Now:            time.Now,
	}
}

type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

type JITRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int64    `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder,omitempty"`
}

type JITResult struct {
	Runner           Runner `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

// CheckAuth verifies the App credentials by minting an installation token.
func (c *Client) CheckAuth(ctx context.Context) error {
	_, err := c.installationToken(ctx)
	return err
}

func (c *Client) installationToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.Now().Before(c.tokenExp.Add(-5*time.Minute)) {
		return c.token, nil
	}
	jwt, err := mintJWT(c.AppID, c.Key, c.Now())
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("app/installations/%d/access_tokens", c.InstallationID)
	if err := c.do(ctx, http.MethodPost, path, jwt, nil, &out); err != nil {
		return "", fmt.Errorf("mint installation token: %w", err)
	}
	c.token, c.tokenExp = out.Token, out.ExpiresAt
	return c.token, nil
}

// api performs an installation-token-authenticated request.
func (c *Client) api(ctx context.Context, method, path string, in, out any) error {
	tok, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, tok, in, out)
}

func (c *Client) do(ctx context.Context, method, path, bearer string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) GenerateJITConfig(ctx context.Context, prefix string, req JITRequest) (*JITResult, error) {
	var out JITResult
	if err := c.api(ctx, http.MethodPost, prefix+"/actions/runners/generate-jitconfig", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetRunner(ctx context.Context, prefix string, id int64) (*Runner, error) {
	var out Runner
	if err := c.api(ctx, http.MethodGet, fmt.Sprintf("%s/actions/runners/%d", prefix, id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteRunner(ctx context.Context, prefix string, id int64) error {
	return c.api(ctx, http.MethodDelete, fmt.Sprintf("%s/actions/runners/%d", prefix, id), nil, nil)
}

func (c *Client) ListRunners(ctx context.Context, prefix string) ([]Runner, error) {
	var all []Runner
	for page := 1; ; page++ {
		var out struct {
			Runners []Runner `json:"runners"`
		}
		path := fmt.Sprintf("%s/actions/runners?per_page=100&page=%d", prefix, page)
		if err := c.api(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Runners...)
		if len(out.Runners) < 100 {
			return all, nil
		}
	}
}

// RunnerGroupID resolves a runner-group name to its ID. Repo-level runners
// always belong to the default group (ID 1); the API has no repo-level
// runner-groups endpoint.
func (c *Client) RunnerGroupID(ctx context.Context, prefix, name string) (int64, error) {
	if strings.HasPrefix(prefix, "repos/") {
		return 1, nil
	}
	for page := 1; ; page++ {
		var out struct {
			RunnerGroups []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"runner_groups"`
		}
		path := fmt.Sprintf("%s/actions/runner-groups?per_page=100&page=%d", prefix, page)
		if err := c.api(ctx, http.MethodGet, path, nil, &out); err != nil {
			return 0, err
		}
		for _, g := range out.RunnerGroups {
			if g.Name == name {
				return g.ID, nil
			}
		}
		if len(out.RunnerGroups) < 100 {
			return 0, fmt.Errorf("runner group %q not found in %s", name, prefix)
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/github/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/github/
git commit -m "feat: add GitHub REST client with JIT config and runner CRUD"
```

---

### Task 4: seed package (NoCloud user-data + ISO)

**Files:**
- Create: `internal/seed/seed.go`
- Test: `internal/seed/seed_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/seed/seed_test.go`:

```go
package seed

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUserData(t *testing.T) {
	ud, err := UserData("JITBLOB==")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatalf("missing #cloud-config header: %q", ud[:30])
	}
	var doc struct {
		WriteFiles []struct {
			Path        string `yaml:"path"`
			Owner       string `yaml:"owner"`
			Permissions string `yaml:"permissions"`
			Content     string `yaml:"content"`
		} `yaml:"write_files"`
		RunCmd [][]string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(ud), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.WriteFiles) != 1 {
		t.Fatalf("write_files = %+v", doc.WriteFiles)
	}
	wf := doc.WriteFiles[0]
	if wf.Path != "/etc/runner-jit.conf" || wf.Owner != "runner:runner" ||
		wf.Permissions != "0600" || wf.Content != "JITBLOB==" {
		t.Errorf("write_files[0] = %+v", wf)
	}
	if len(doc.RunCmd) != 1 || len(doc.RunCmd[0]) != 1 || doc.RunCmd[0][0] != "/usr/local/bin/run-one-job" {
		t.Errorf("runcmd = %+v", doc.RunCmd)
	}
}

func TestMetaData(t *testing.T) {
	got := MetaData("ghq-fmt-ab12", "ghq-fmt-ab12")
	want := "instance-id: ghq-fmt-ab12\nlocal-hostname: ghq-fmt-ab12\n"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestBuildISO(t *testing.T) {
	if _, err := exec.LookPath("genisoimage"); err != nil {
		t.Skip("genisoimage not installed")
	}
	dir := t.TempDir()
	iso, err := BuildISO(context.Background(), dir, "#cloud-config\n{}\n", MetaData("i", "h"))
	if err != nil {
		t.Fatal(err)
	}
	if iso != filepath.Join(dir, "seed.iso") {
		t.Errorf("iso path = %q", iso)
	}
	fi, err := os.Stat(iso)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Error("seed.iso is empty")
	}
	// the source files must exist alongside (genisoimage read them)
	for _, f := range []string{"user-data", "meta-data"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/seed/`
Expected: FAIL — `undefined: UserData` etc.

- [ ] **Step 3: Write the implementation**

`internal/seed/seed.go`:

```go
// Package seed renders cloud-init NoCloud data and packs it into the seed
// ISO each ephemeral VM boots with.
package seed

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// UserData renders the per-VM cloud-config: write the JIT blob where the
// baked run-one-job script expects it, then invoke that script. Everything
// else (runner install, user, Docker) is baked into the base image.
func UserData(jitConfig string) (string, error) {
	doc := map[string]any{
		"write_files": []map[string]any{{
			"path":        "/etc/runner-jit.conf",
			"owner":       "runner:runner",
			"permissions": "0600",
			"content":     jitConfig,
		}},
		"runcmd": [][]string{{"/usr/local/bin/run-one-job"}},
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(b), nil
}

// MetaData gives each VM a unique instance-id so cloud-init treats every
// boot as a first boot of a new instance.
func MetaData(instanceID, hostname string) string {
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, hostname)
}

// BuildISO writes user-data/meta-data into dir and packs them into
// dir/seed.iso with the volume label cloud-init's NoCloud datasource looks
// for. Requires genisoimage on PATH.
func BuildISO(ctx context.Context, dir, userData, metaData string) (string, error) {
	if err := os.WriteFile(filepath.Join(dir, "user-data"), []byte(userData), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta-data"), []byte(metaData), 0o600); err != nil {
		return "", err
	}
	iso := filepath.Join(dir, "seed.iso")
	cmd := exec.CommandContext(ctx, "genisoimage",
		"-output", "seed.iso", "-volid", "cidata", "-joliet", "-rock",
		"user-data", "meta-data")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("genisoimage: %v: %s", err, out)
	}
	return iso, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/seed/`
Expected: PASS (TestBuildISO runs for real — genisoimage exists on this host).

- [ ] **Step 5: Commit**

```bash
git add internal/seed/
git commit -m "feat: add NoCloud seed rendering and ISO builder"
```

---

### Task 5: qemu package — overlay + argv builder

**Files:**
- Create: `internal/qemu/qemu.go`
- Test: `internal/qemu/qemu_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/qemu/qemu_test.go`:

```go
package qemu

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testSpec(dir string) Spec {
	return Spec{
		Name:        "ghq-fmt-ab12",
		CPUs:        2,
		MemoryMB:    2048,
		OverlayPath: filepath.Join(dir, "overlay.qcow2"),
		SeedISOPath: filepath.Join(dir, "seed.iso"),
		ConsoleLog:  filepath.Join(dir, "console.log"),
		QMPSocket:   filepath.Join(dir, "qmp.sock"),
		PIDFile:     filepath.Join(dir, "qemu.pid"),
	}
}

func TestArgs(t *testing.T) {
	dir := t.TempDir()
	args := Args(testSpec(dir))
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-accel kvm",
		"-cpu host",
		"-machine q35",
		"-smp 2",
		"-m 2048",
		"file=" + filepath.Join(dir, "overlay.qcow2") + ",if=virtio,format=qcow2",
		"file=" + filepath.Join(dir, "seed.iso") + ",if=virtio,format=raw,readonly=on",
		"-netdev user,id=n0",
		"-device virtio-net-pci,netdev=n0",
		"-display none",
		"-serial file:" + filepath.Join(dir, "console.log"),
		"-qmp unix:" + filepath.Join(dir, "qmp.sock") + ",server=on,wait=off",
		"-pidfile " + filepath.Join(dir, "qemu.pid"),
		"-no-reboot",
		"-name ghq-fmt-ab12",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q\nargs: %s", want, joined)
		}
	}
	// -nographic must NOT be used: it would fight -serial file:
	if slices.Contains(args, "-nographic") {
		t.Error("args must not contain -nographic")
	}
}

func TestCreateOverlay(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	out, err := exec.Command("qemu-img", "create", "-f", "qcow2", base, "1G").CombinedOutput()
	if err != nil {
		t.Fatalf("create base: %v: %s", err, out)
	}
	overlay := filepath.Join(dir, "overlay.qcow2")
	if err := CreateOverlay(context.Background(), base, overlay, 10); err != nil {
		t.Fatal(err)
	}
	info, err := exec.Command("qemu-img", "info", overlay).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v: %s", err, info)
	}
	s := string(info)
	if !strings.Contains(s, "backing file: "+base) {
		t.Errorf("no backing file in info:\n%s", s)
	}
	if !strings.Contains(s, "10 GiB") {
		t.Errorf("not resized to 10 GiB:\n%s", s)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/qemu/`
Expected: FAIL — `undefined: Spec`, `undefined: Args`, `undefined: CreateOverlay`.

- [ ] **Step 3: Write the implementation**

`internal/qemu/qemu.go`:

```go
// Package qemu creates copy-on-write disks and supervises
// qemu-system-x86_64 child processes. One VM = one process; guest poweroff
// (with -no-reboot) makes the process exit, which is the job-done signal.
package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

type Spec struct {
	Name        string
	CPUs        int
	MemoryMB    int
	OverlayPath string
	SeedISOPath string
	ConsoleLog  string
	QMPSocket   string
	PIDFile     string
}

// Args builds the qemu-system-x86_64 argv. -display none (not -nographic:
// that would redirect the serial port to stdio and fight -serial file:).
func Args(s Spec) []string {
	return []string{
		"-accel", "kvm",
		"-cpu", "host",
		"-machine", "q35",
		"-smp", strconv.Itoa(s.CPUs),
		"-m", strconv.Itoa(s.MemoryMB),
		"-drive", fmt.Sprintf("file=%s,if=virtio,format=qcow2", s.OverlayPath),
		"-drive", fmt.Sprintf("file=%s,if=virtio,format=raw,readonly=on", s.SeedISOPath),
		"-netdev", "user,id=n0",
		"-device", "virtio-net-pci,netdev=n0",
		"-display", "none",
		"-serial", "file:" + s.ConsoleLog,
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", s.QMPSocket),
		"-pidfile", s.PIDFile,
		"-no-reboot",
		"-name", s.Name,
	}
}

// CreateOverlay makes a qcow2 overlay backed by base (which must be an
// absolute path — qemu resolves relative backing paths against the overlay's
// directory) and grows its virtual size to diskGB. The guest's cloud-init
// growpart expands the root filesystem into the new space on boot.
func CreateOverlay(ctx context.Context, base, dest string, diskGB int) error {
	if out, err := exec.CommandContext(ctx, "qemu-img", "create",
		"-f", "qcow2", "-b", base, "-F", "qcow2", dest).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create %s: %v: %s", dest, err, out)
	}
	if out, err := exec.CommandContext(ctx, "qemu-img", "resize",
		dest, fmt.Sprintf("%dG", diskGB)).CombinedOutput(); err != nil {
		os.Remove(dest)
		return fmt.Errorf("qemu-img resize %s: %v: %s", dest, err, out)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/qemu/`
Expected: PASS (qemu-img exists on this host).

- [ ] **Step 5: Commit**

```bash
git add internal/qemu/
git commit -m "feat: add qcow2 overlay creation and QEMU argv builder"
```

---

### Task 6: qemu package — VM supervision + QMP powerdown

**Files:**
- Create: `internal/qemu/vm.go`
- Create: `internal/qemu/qmp.go`
- Test: `internal/qemu/vm_test.go`
- Test: `internal/qemu/qmp_test.go`

- [ ] **Step 1: Write the failing VM tests** (a fake qemu binary — a shell script — stands in; no real VMs in unit tests)

`internal/qemu/vm_test.go`:

```go
package qemu

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeQEMU(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-qemu")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVMExitsCleanly(t *testing.T) {
	vm, err := Start(context.Background(), fakeQEMU(t, "exit 0"), testSpec(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VM did not exit")
	}
	if vm.Err() != nil {
		t.Errorf("Err() = %v", vm.Err())
	}
}

func TestVMKill(t *testing.T) {
	vm, err := Start(context.Background(), fakeQEMU(t, "sleep 60"), testSpec(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VM did not die after Kill")
	}
	if vm.Err() == nil {
		t.Error("Err() should be non-nil after Kill")
	}
}

func TestVMPowerdownFallsBackToKill(t *testing.T) {
	// No QMP socket exists for the fake binary, so Powerdown must fall back
	// to Kill and still terminate the process.
	vm, err := Start(context.Background(), fakeQEMU(t, "sleep 60"), testSpec(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Powerdown(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VM still running after Powerdown fallback")
	}
}
```

- [ ] **Step 2: Write the failing QMP test**

`internal/qemu/qmp_test.go`:

```go
package qemu

import (
	"bufio"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQMPPowerdown(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	received := make(chan string, 2)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintln(conn, `{"QMP":{"version":{},"capabilities":[]}}`)
		br := bufio.NewReader(conn)
		for range 2 {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			received <- line
			fmt.Fprintln(conn, `{"return":{}}`)
		}
	}()

	if err := qmpPowerdown(sock); err != nil {
		t.Fatal(err)
	}
	want := []string{"qmp_capabilities", "system_powerdown"}
	for _, w := range want {
		select {
		case got := <-received:
			if !strings.Contains(got, w) {
				t.Errorf("got %q, want command %q", got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("server never received %q", w)
		}
	}
}

func TestQMPPowerdownNoSocket(t *testing.T) {
	if err := qmpPowerdown(filepath.Join(t.TempDir(), "absent.sock")); err == nil {
		t.Error("want error for missing socket")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/qemu/`
Expected: FAIL — `undefined: Start`, `undefined: qmpPowerdown`.

- [ ] **Step 4: Write the implementations**

`internal/qemu/vm.go`:

```go
package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// VM supervises one qemu child process.
type VM struct {
	cmd  *exec.Cmd
	spec Spec
	done chan struct{}

	mu  sync.Mutex
	err error
}

// Start launches the qemu binary for spec. The process is deliberately NOT
// tied to ctx: shutdown is orchestrated (drain, QMP powerdown), not by
// context kill. ctx only bounds Start itself.
func Start(_ context.Context, binary string, spec Spec) (*VM, error) {
	logf, err := os.Create(spec.ConsoleLog + ".qemu-stderr")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, Args(spec)...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}
	vm := &VM{cmd: cmd, spec: spec, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		logf.Close()
		vm.mu.Lock()
		vm.err = err
		vm.mu.Unlock()
		close(vm.done)
	}()
	return vm, nil
}

// Done is closed when the qemu process has exited.
func (v *VM) Done() <-chan struct{} { return v.done }

// Err reports the process exit error; only meaningful after Done is closed.
func (v *VM) Err() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}

// Kill terminates the process and waits for it to be reaped.
func (v *VM) Kill() error {
	_ = v.cmd.Process.Kill()
	<-v.done
	return nil
}

// Powerdown asks the guest to shut down via QMP (ACPI power button) and
// waits up to timeout, then falls back to Kill. Always terminates the VM.
func (v *VM) Powerdown(timeout time.Duration) error {
	if err := qmpPowerdown(v.spec.QMPSocket); err != nil {
		return v.Kill()
	}
	select {
	case <-v.done:
		return nil
	case <-time.After(timeout):
		return v.Kill()
	}
}

// ConsoleTail returns the last 2 KiB of the guest serial console, so boot
// wedges and crashes can be surfaced into the journal before the workdir
// (and console.log with it) is deleted.
func (v *VM) ConsoleTail() string {
	b, err := os.ReadFile(v.spec.ConsoleLog)
	if err != nil {
		return ""
	}
	if len(b) > 2048 {
		b = b[len(b)-2048:]
	}
	return string(b)
}
```

`internal/qemu/qmp.go`:

```go
package qemu

import (
	"bufio"
	"fmt"
	"net"
	"time"
)

// qmpPowerdown speaks just enough QMP to press the virtual power button:
// read greeting, negotiate capabilities, send system_powerdown.
func qmpPowerdown(socketPath string) error {
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return fmt.Errorf("dial QMP: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	if _, err := br.ReadString('\n'); err != nil { // greeting
		return fmt.Errorf("read QMP greeting: %w", err)
	}
	for _, cmd := range []string{
		`{"execute":"qmp_capabilities"}`,
		`{"execute":"system_powerdown"}`,
	} {
		if _, err := fmt.Fprintln(conn, cmd); err != nil {
			return err
		}
		if _, err := br.ReadString('\n'); err != nil {
			return fmt.Errorf("read QMP response: %w", err)
		}
	}
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/qemu/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/qemu/
git commit -m "feat: add VM process supervision and QMP powerdown"
```

---

### Task 7: guest scripts + embed package

**Files:**
- Create: `scripts/guest/run-one-job.sh`
- Create: `scripts/guest/bake.sh`
- Create: `scripts/embed.go`
- Test: `scripts/embed_test.go`

- [ ] **Step 1: Create scripts/guest/run-one-job.sh**

```bash
#!/usr/bin/env bash
# Runs as root from cloud-init runcmd inside the ephemeral job VM.
# Runs exactly one GitHub Actions job as the unprivileged "runner" user,
# then powers the guest off NO MATTER WHAT — host-side teardown depends on
# the qemu process exiting.
set -uo pipefail
trap 'poweroff' EXIT

JIT_FILE=/etc/runner-jit.conf
if [ ! -s "$JIT_FILE" ]; then
  echo "run-one-job: $JIT_FILE missing or empty" >/dev/console
  exit 1
fi

cd /opt/actions-runner
# The JIT config registers a pre-created ephemeral runner; run.sh executes
# one job, deregisters, and exits. The blob is single-use, so its visibility
# in guest argv is harmless (the guest is destroyed right after).
runuser -u runner -- ./run.sh --jitconfig "$(cat "$JIT_FILE")"
```

- [ ] **Step 2: Create scripts/guest/bake.sh**

```bash
#!/usr/bin/env bash
# Runs as root via cloud-init runcmd during the ONE-TIME image bake boot.
# Installs Docker + the actions runner, then powers off. The host watches
# the serial console for BAKE-OK; any failure means no sentinel and the
# bake is rejected.
set -euxo pipefail
exec >/dev/console 2>&1

# Written by the bake user-data: VERSION, TARBALL_URL, TARBALL_SHA256.
# shellcheck disable=SC1091
source /run/bake-env

export DEBIAN_FRONTEND=noninteractive

useradd --create-home --shell /bin/bash runner

apt-get update
apt-get install -y --no-install-recommends \
  git curl ca-certificates jq build-essential docker.io
usermod -aG docker runner
systemctl enable docker

mkdir -p /opt/actions-runner
curl -fsSL "$TARBALL_URL" -o /tmp/runner.tar.gz
if [ -n "$TARBALL_SHA256" ]; then
  echo "$TARBALL_SHA256  /tmp/runner.tar.gz" | sha256sum -c -
fi
tar -xzf /tmp/runner.tar.gz -C /opt/actions-runner
rm /tmp/runner.tar.gz
/opt/actions-runner/bin/installdependencies.sh
chown -R runner:runner /opt/actions-runner

install -m 0755 /run/run-one-job /usr/local/bin/run-one-job

# Make the image boot as a fresh instance every time it's cloned.
cloud-init clean --logs --machine-id

echo "BAKE-OK"
poweroff
```

- [ ] **Step 3: Create scripts/embed.go**

```go
// Package scripts embeds the guest-side shell scripts. They are kept as
// real .sh files so shellcheck/shfmt/lefthook cover them.
package scripts

import _ "embed"

//go:embed guest/run-one-job.sh
var RunOneJob string

//go:embed guest/bake.sh
var Bake string
```

- [ ] **Step 4: Write a smoke test for the embeds**

`scripts/embed_test.go`:

```go
package scripts

import (
	"strings"
	"testing"
)

func TestEmbeddedScripts(t *testing.T) {
	if !strings.Contains(RunOneJob, "--jitconfig") || !strings.Contains(RunOneJob, "trap 'poweroff' EXIT") {
		t.Error("RunOneJob missing expected content")
	}
	if !strings.Contains(Bake, "BAKE-OK") || !strings.Contains(Bake, "cloud-init clean") {
		t.Error("Bake missing expected content")
	}
}
```

- [ ] **Step 5: Verify shellcheck, shfmt, and tests pass**

Run: `shellcheck scripts/guest/*.sh && shfmt -d scripts/guest/ && go test ./scripts/`
Expected: all exit 0.

- [ ] **Step 6: Commit**

```bash
git add scripts/
git commit -m "feat: add guest bake and run-one-job scripts with embeds"
```

---

### Task 8: pool package (slot supervisor)

**Files:**
- Create: `internal/pool/pool.go`
- Test: `internal/pool/pool_test.go`

- [ ] **Step 1: Write the failing tests** (white-box, same package; fakes for API/VM/Provisioner)

`internal/pool/pool_test.go`:

```go
package pool

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/github"
)

type fakeAPI struct {
	mu      sync.Mutex
	status  string
	busy    bool
	deleted []int64
	jitErr  error
}

func (f *fakeAPI) GenerateJITConfig(_ context.Context, _ string, req github.JITRequest) (*github.JITResult, error) {
	if f.jitErr != nil {
		return nil, f.jitErr
	}
	return &github.JITResult{Runner: github.Runner{ID: 42, Name: req.Name}, EncodedJITConfig: "blob"}, nil
}

func (f *fakeAPI) GetRunner(context.Context, string, int64) (*github.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &github.Runner{ID: 42, Status: f.status, Busy: f.busy}, nil
}

func (f *fakeAPI) DeleteRunner(_ context.Context, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeAPI) RunnerGroupID(context.Context, string, string) (int64, error) { return 1, nil }

func (f *fakeAPI) deletedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.deleted...)
}

type fakeVM struct {
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	killed  bool
	powered bool
}

func newFakeVM() *fakeVM                  { return &fakeVM{done: make(chan struct{})} }
func (v *fakeVM) exit()                   { v.once.Do(func() { close(v.done) }) }
func (v *fakeVM) Done() <-chan struct{}   { return v.done }
func (v *fakeVM) Err() error              { return nil }
func (v *fakeVM) Kill() error {
	v.mu.Lock()
	v.killed = true
	v.mu.Unlock()
	v.exit()
	return nil
}
func (v *fakeVM) Powerdown(time.Duration) error {
	v.mu.Lock()
	v.powered = true
	v.mu.Unlock()
	v.exit()
	return nil
}
func (v *fakeVM) ConsoleTail() string { return "" }
func (v *fakeVM) wasKilled() bool     { v.mu.Lock(); defer v.mu.Unlock(); return v.killed }
func (v *fakeVM) wasPowered() bool    { v.mu.Lock(); defer v.mu.Unlock(); return v.powered }

type fakeProv struct {
	vm      *fakeVM
	err     error
	mu      sync.Mutex
	cleaned int
}

func (f *fakeProv) Provision(context.Context, string, config.Pool, string) (VM, func(), error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.vm, func() { f.mu.Lock(); f.cleaned++; f.mu.Unlock() }, nil
}

func (f *fakeProv) cleanedCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.cleaned }

func testPool(api *fakeAPI, prov *fakeProv) *Pool {
	return &Pool{
		Cfg: config.Pool{
			Name: "fmt", Scope: "org", Org: "o", Count: 1,
			CPUs: 1, MemoryMB: 512, DiskGB: 10,
			Labels:          []string{"x"},
			RunnerGroup:     "Default",
			LivenessTimeout: config.Duration(200 * time.Millisecond),
			DrainTimeout:    config.Duration(200 * time.Millisecond),
		},
		GH: api, Prov: prov,
		Log:          slog.New(slog.DiscardHandler),
		PollInterval: 10 * time.Millisecond,
	}
}

func TestRunOneHappyPath(t *testing.T) {
	api := &fakeAPI{status: "online"}
	vm := newFakeVM()
	prov := &fakeProv{vm: vm}
	p := testPool(api, prov)
	go func() { time.Sleep(50 * time.Millisecond); vm.exit() }() // job "finishes"
	if err := p.runOne(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if prov.cleanedCount() != 1 {
		t.Error("cleanup not called")
	}
	if got := api.deletedIDs(); len(got) != 1 || got[0] != 42 {
		t.Errorf("deleted = %v, want [42]", got)
	}
}

func TestRunOneLivenessTimeout(t *testing.T) {
	api := &fakeAPI{status: "offline"} // runner never connects
	vm := newFakeVM()
	p := testPool(api, &fakeProv{vm: vm})
	err := p.runOne(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "not online") {
		t.Fatalf("err = %v", err)
	}
	if !vm.wasKilled() {
		t.Error("wedged VM was not killed")
	}
	if len(api.deletedIDs()) != 1 {
		t.Error("runner record not deleted")
	}
}

func TestRunOneProvisionFailureDeletesRecord(t *testing.T) {
	api := &fakeAPI{status: "online"}
	p := testPool(api, &fakeProv{err: errors.New("qemu-img exploded")})
	if err := p.runOne(context.Background(), 0); err == nil {
		t.Fatal("want error")
	}
	if len(api.deletedIDs()) != 1 {
		t.Error("runner record not deleted after provision failure")
	}
}

func TestDrainIdleRunner(t *testing.T) {
	api := &fakeAPI{status: "online", busy: false}
	vm := newFakeVM()
	p := testPool(api, &fakeProv{vm: vm})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.runOne(ctx, 0) }()
	time.Sleep(50 * time.Millisecond) // let it pass the liveness gate
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runOne did not return after cancel")
	}
	if !vm.wasPowered() {
		t.Error("idle VM not powered down")
	}
	if len(api.deletedIDs()) == 0 {
		t.Error("idle runner record not deleted before powerdown")
	}
}

func TestDrainBusyRunnerTimesOut(t *testing.T) {
	api := &fakeAPI{status: "online", busy: true}
	vm := newFakeVM() // never exits on its own
	p := testPool(api, &fakeProv{vm: vm})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.runOne(ctx, 0) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done: // DrainTimeout (200ms) then forced powerdown
	case <-time.After(2 * time.Second):
		t.Fatal("runOne did not return")
	}
	if !vm.wasPowered() {
		t.Error("busy VM not powered down after drain timeout")
	}
}

func TestRunReturnsOnCancel(t *testing.T) {
	api := &fakeAPI{jitErr: errors.New("api down")}
	p := testPool(api, &fakeProv{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished := make(chan struct{})
	go func() { p.Run(ctx); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on cancelled context")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/pool/`
Expected: FAIL — `undefined: Pool`, `undefined: VM`.

- [ ] **Step 3: Write the implementation**

`internal/pool/pool.go`:

```go
// Package pool supervises a fixed number of ephemeral-runner slots. Each
// slot loops: JIT-register -> provision VM -> liveness gate -> wait for the
// VM to power itself off -> teardown -> repeat.
package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/github"
)

// API is the slice of the GitHub client the pool needs.
type API interface {
	GenerateJITConfig(ctx context.Context, prefix string, req github.JITRequest) (*github.JITResult, error)
	GetRunner(ctx context.Context, prefix string, id int64) (*github.Runner, error)
	DeleteRunner(ctx context.Context, prefix string, id int64) error
	RunnerGroupID(ctx context.Context, prefix, name string) (int64, error)
}

// VM is the slice of qemu.VM the pool needs.
type VM interface {
	Done() <-chan struct{}
	Err() error
	Powerdown(timeout time.Duration) error
	Kill() error
	ConsoleTail() string
}

// Provisioner creates a booted VM for a JIT config. The returned cleanup
// removes the VM's working directory and must be called after the VM exits.
type Provisioner interface {
	Provision(ctx context.Context, name string, p config.Pool, jitConfig string) (VM, func(), error)
}

type Pool struct {
	Cfg  config.Pool
	GH   API
	Prov Provisioner
	Log  *slog.Logger

	// PollInterval for the liveness gate; defaults to 10s. Tests shrink it.
	PollInterval time.Duration
}

// Run blocks until ctx is cancelled and all slots have drained. The desired
// count is static in v1; later autoscaling replaces Cfg.Count with a dynamic
// desired value without touching the slot loop.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range p.Cfg.Count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.runSlot(ctx, i)
		}()
	}
	wg.Wait()
}

func (p *Pool) runSlot(ctx context.Context, slot int) {
	backoff := 15 * time.Second
	for ctx.Err() == nil {
		err := p.runOne(ctx, slot)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.Log.Error("slot iteration failed",
				"pool", p.Cfg.Name, "slot", slot, "err", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}
		backoff = 15 * time.Second
	}
}

// runOne is a single slot iteration.
func (p *Pool) runOne(ctx context.Context, slot int) error {
	prefix := p.Cfg.APIPrefix()
	name := fmt.Sprintf("ghq-%s-%s", p.Cfg.Name, shortID())
	log := p.Log.With("pool", p.Cfg.Name, "slot", slot, "vm", name)

	groupID, err := p.GH.RunnerGroupID(ctx, prefix, p.Cfg.RunnerGroup)
	if err != nil {
		return fmt.Errorf("resolve runner group: %w", err)
	}
	jit, err := p.GH.GenerateJITConfig(ctx, prefix, github.JITRequest{
		Name:          name,
		RunnerGroupID: groupID,
		Labels:        p.Cfg.Labels,
		WorkFolder:    "_work",
	})
	if err != nil {
		return fmt.Errorf("generate jitconfig: %w", err)
	}
	// From here the runner record exists on GitHub. Ephemeral runners that
	// complete a job self-deregister (the delete then 404s, harmlessly);
	// every other exit path needs this explicit delete.
	defer p.deleteRecord(prefix, jit.Runner.ID, log)

	vm, cleanup, err := p.Prov.Provision(ctx, name, p.Cfg, jit.EncodedJITConfig)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	defer cleanup()

	log.Info("VM booted, waiting for runner to come online")
	if err := p.awaitOnline(ctx, vm, prefix, jit.Runner.ID); err != nil {
		// Surface the guest console before teardown deletes it.
		log.Warn("liveness gate failed; killing VM", "console_tail", vm.ConsoleTail())
		_ = vm.Kill()
		return err
	}
	log.Info("runner online")

	select {
	case <-vm.Done():
		if vmErr := vm.Err(); vmErr != nil {
			log.Warn("VM exited with error", "err", vmErr, "console_tail", vm.ConsoleTail())
		} else {
			log.Info("VM exited")
		}
		return nil
	case <-ctx.Done():
		p.drain(vm, prefix, jit.Runner.ID, log)
		return nil
	}
}

// awaitOnline is the liveness gate: a guest that wedges before the runner
// connects must not occupy the slot forever.
func (p *Pool) awaitOnline(ctx context.Context, vm VM, prefix string, id int64) error {
	interval := p.PollInterval
	if interval == 0 {
		interval = 10 * time.Second
	}
	deadline := time.After(time.Duration(p.Cfg.LivenessTimeout))
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-vm.Done():
			return fmt.Errorf("VM exited before runner came online: %v", vm.Err())
		case <-deadline:
			return fmt.Errorf("runner not online within %v", time.Duration(p.Cfg.LivenessTimeout))
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			r, err := p.GH.GetRunner(ctx, prefix, id)
			if err != nil {
				continue // transient API failure; keep polling until deadline
			}
			if r.Status == "online" {
				return nil
			}
		}
	}
}

// drain handles shutdown while a VM is up: idle runners are deleted from
// GitHub first (so no job can land mid-shutdown) and powered down; busy
// runners get up to DrainTimeout to finish, then are powered down (the job
// fails — GitHub does not requeue jobs from vanished ephemeral runners).
func (p *Pool) drain(vm VM, prefix string, id int64, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := p.GH.GetRunner(ctx, prefix, id)
	busy := err == nil && r.Busy
	if !busy {
		log.Info("draining idle runner")
		p.deleteRecord(prefix, id, log)
		_ = vm.Powerdown(30 * time.Second)
		return
	}
	log.Info("waiting for busy runner to finish", "timeout", time.Duration(p.Cfg.DrainTimeout))
	select {
	case <-vm.Done():
		log.Info("job finished during drain")
	case <-time.After(time.Duration(p.Cfg.DrainTimeout)):
		log.Warn("drain timeout exceeded; powering down (job will fail)")
		_ = vm.Powerdown(30 * time.Second)
	}
}

func (p *Pool) deleteRecord(prefix string, id int64, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.GH.DeleteRunner(ctx, prefix, id); err != nil && !errors.Is(err, github.ErrNotFound) {
		log.Warn("delete runner record failed", "id", id, "err", err)
	}
}

func shortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return hex.EncodeToString(b)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/pool/`
Expected: PASS, no data races.

- [ ] **Step 5: Commit**

```bash
git add internal/pool/
git commit -m "feat: add pool slot supervisor with liveness gate and drain"
```

---

### Task 9: real Provisioner

**Files:**
- Create: `internal/controller/provision.go`
- Test: `internal/controller/provision_test.go`

- [ ] **Step 1: Write the failing test** (gated on qemu-img + genisoimage; the "qemu" is a fake script, no real VM)

`internal/controller/provision_test.go`:

```go
package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/config"
)

func TestProvision(t *testing.T) {
	for _, bin := range []string{"qemu-img", "genisoimage"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", base, "1G").CombinedOutput(); err != nil {
		t.Fatalf("create base: %v: %s", err, out)
	}
	fake := filepath.Join(dir, "fake-qemu")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "run")

	q := &QEMUProvisioner{RunDir: runDir, BasePath: base, QEMUBin: fake}
	pcfg := config.Pool{Name: "fmt", CPUs: 1, MemoryMB: 512, DiskGB: 10}
	vm, cleanup, err := q.Provision(context.Background(), "ghq-fmt-test", pcfg, "JITBLOB")
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(runDir, "ghq-fmt-test")
	for _, f := range []string{"overlay.qcow2", "seed.iso", "user-data", "meta-data"} {
		if _, err := os.Stat(filepath.Join(workdir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("fake qemu did not exit")
	}
	cleanup()
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Errorf("workdir not removed: %v", err)
	}
}

func TestProvisionFailureCleansUp(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	q := &QEMUProvisioner{
		RunDir:   runDir,
		BasePath: filepath.Join(dir, "missing-base.qcow2"), // overlay creation fails
		QEMUBin:  "/bin/false",
	}
	_, _, err := q.Provision(context.Background(), "ghq-x-y", config.Pool{DiskGB: 10}, "J")
	if err == nil {
		t.Fatal("want error")
	}
	if _, statErr := os.Stat(filepath.Join(runDir, "ghq-x-y")); !os.IsNotExist(statErr) {
		t.Error("failed provision left workdir behind")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/`
Expected: FAIL — `undefined: QEMUProvisioner`.

- [ ] **Step 3: Write the implementation**

`internal/controller/provision.go`:

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
// under RunDir and boots the VM.
type QEMUProvisioner struct {
	RunDir   string // <state>/run
	BasePath string // <state>/images/base.qcow2, absolute
	QEMUBin  string
}

func (q *QEMUProvisioner) Provision(ctx context.Context, name string, p config.Pool, jitConfig string) (pool.VM, func(), error) {
	dir := filepath.Join(q.RunDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	fail := func(err error) (pool.VM, func(), error) {
		cleanup()
		return nil, nil, err
	}

	overlay := filepath.Join(dir, "overlay.qcow2")
	if err := qemu.CreateOverlay(ctx, q.BasePath, overlay, p.DiskGB); err != nil {
		return fail(err)
	}
	ud, err := seed.UserData(jitConfig)
	if err != nil {
		return fail(err)
	}
	iso, err := seed.BuildISO(ctx, dir, ud, seed.MetaData(name, name))
	if err != nil {
		return fail(err)
	}
	vm, err := qemu.Start(ctx, q.QEMUBin, qemu.Spec{
		Name:        name,
		CPUs:        p.CPUs,
		MemoryMB:    p.MemoryMB,
		OverlayPath: overlay,
		SeedISOPath: iso,
		ConsoleLog:  filepath.Join(dir, "console.log"),
		QMPSocket:   filepath.Join(dir, "qmp.sock"),
		PIDFile:     filepath.Join(dir, "qemu.pid"),
	})
	if err != nil {
		return fail(fmt.Errorf("start VM: %w", err))
	}
	return vm, cleanup, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/
git commit -m "feat: add QEMU provisioner composing overlay, seed, and boot"
```

---

### Task 10: orphan reaping + controller wiring

**Files:**
- Create: `internal/controller/reap.go`
- Create: `internal/controller/controller.go`
- Test: `internal/controller/reap_test.go`

- [ ] **Step 1: Write the failing reap tests**

`internal/controller/reap_test.go`:

```go
package controller

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/a1678991/github-qemu-runner/internal/github"
)

type fakeReaperAPI struct {
	mu      sync.Mutex
	runners []github.Runner
	deleted []int64
}

func (f *fakeReaperAPI) ListRunners(context.Context, string) ([]github.Runner, error) {
	return f.runners, nil
}

func (f *fakeReaperAPI) DeleteRunner(_ context.Context, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func TestReapOrphansRemovesWorkdirsAndOfflineRunners(t *testing.T) {
	runDir := t.TempDir()
	stale := filepath.Join(runDir, "ghq-fmt-dead")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	// A pid file pointing at a PID that (a) doesn't exist or (b) isn't a
	// qemu process must not cause a kill, but the dir still goes away.
	if err := os.WriteFile(filepath.Join(stale, "qemu.pid"), []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	api := &fakeReaperAPI{runners: []github.Runner{
		{ID: 1, Name: "ghq-fmt-aaaa", Status: "offline"}, // ours, dead -> delete
		{ID: 2, Name: "ghq-fmt-bbbb", Status: "online"},  // ours, alive -> keep
		{ID: 3, Name: "macmini-tart", Status: "offline"}, // not ours -> keep
	}}

	ReapOrphans(context.Background(), runDir, api, []string{"orgs/o"}, slog.New(slog.DiscardHandler))

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale workdir not removed")
	}
	if len(api.deleted) != 1 || api.deleted[0] != 1 {
		t.Errorf("deleted = %v, want [1]", api.deleted)
	}
}

func TestReapOrphansMissingRunDir(t *testing.T) {
	api := &fakeReaperAPI{}
	// must not panic or error
	ReapOrphans(context.Background(), filepath.Join(t.TempDir(), "absent"), api, nil, slog.New(slog.DiscardHandler))
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/`
Expected: FAIL — `undefined: ReapOrphans`.

- [ ] **Step 3: Write reap.go**

`internal/controller/reap.go`:

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/a1678991/github-qemu-runner/internal/github"
)

// reaperAPI is the slice of the GitHub client reaping needs.
type reaperAPI interface {
	ListRunners(ctx context.Context, prefix string) ([]github.Runner, error)
	DeleteRunner(ctx context.Context, prefix string, id int64) error
}

// ReapOrphans cleans up after a crashed controller: leftover qemu
// processes and workdirs under runDir, plus offline ghq-* runner records
// on GitHub. Best-effort; failures are logged, never fatal.
func ReapOrphans(ctx context.Context, runDir string, gh reaperAPI, prefixes []string, log *slog.Logger) {
	entries, err := os.ReadDir(runDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("read run dir", "err", err)
	}
	for _, e := range entries {
		dir := filepath.Join(runDir, e.Name())
		killStaleQEMU(dir, e.Name(), log)
		if err := os.RemoveAll(dir); err != nil {
			log.Warn("remove stale workdir", "dir", dir, "err", err)
		} else {
			log.Info("removed stale workdir", "dir", dir)
		}
	}
	for _, prefix := range prefixes {
		runners, err := gh.ListRunners(ctx, prefix)
		if err != nil {
			log.Warn("list runners", "prefix", prefix, "err", err)
			continue
		}
		for _, r := range runners {
			if strings.HasPrefix(r.Name, "ghq-") && r.Status == "offline" {
				if err := gh.DeleteRunner(ctx, prefix, r.ID); err != nil {
					log.Warn("delete offline runner", "name", r.Name, "err", err)
				} else {
					log.Info("deleted offline runner record", "name", r.Name)
				}
			}
		}
	}
}

// killStaleQEMU kills the process recorded in dir/qemu.pid, but only after
// /proc/<pid>/cmdline confirms it is a qemu process running this VM — PIDs
// are recycled, especially across host reboots.
func killStaleQEMU(dir, vmName string, log *slog.Logger) {
	b, err := os.ReadFile(filepath.Join(dir, "qemu.pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return // process already gone
	}
	args := string(cmdline)
	if !strings.Contains(args, "qemu-system") || !strings.Contains(args, vmName) {
		log.Warn("pid file does not point at our qemu; not killing", "pid", pid, "vm", vmName)
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
		log.Info("killed orphan qemu", "pid", pid, "vm", vmName)
	}
}
```

- [ ] **Step 4: Write controller.go**

`internal/controller/controller.go`:

```go
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/github"
	"github.com/a1678991/github-qemu-runner/internal/pool"
)

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
	runDir := filepath.Join(cfg.StateDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return err
	}

	for _, w := range cfg.CapacityWarnings(runtime.NumCPU(), HostMemMB()) {
		log.Warn(w)
	}

	ReapOrphans(ctx, runDir, gh, apiPrefixes(cfg), log)

	prov := &QEMUProvisioner{RunDir: runDir, BasePath: basePath, QEMUBin: qemuBin}
	var wg sync.WaitGroup
	for _, pc := range cfg.Pools {
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

// apiPrefixes returns the deduplicated registration scopes of all pools.
func apiPrefixes(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range cfg.Pools {
		pre := p.APIPrefix()
		if !seen[pre] {
			seen[pre] = true
			out = append(out, pre)
		}
	}
	return out
}

// HostMemMB reads MemTotal from /proc/meminfo. Returns 0 on failure, which
// CapacityWarnings treats as "unknown, don't warn".
func HostMemMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(b)) {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			fields := strings.Fields(rest)
			if len(fields) >= 1 {
				if kb, err := strconv.Atoi(fields[0]); err == nil {
					return kb / 1024
				}
			}
		}
	}
	return 0
}
```

Also add this test to `internal/controller/reap_test.go` (same file, bottom):

```go
func TestAPIPrefixesDedup(t *testing.T) {
	cfg := &config.Config{Pools: []config.Pool{
		{Scope: "org", Org: "o"},
		{Scope: "org", Org: "o"},
		{Scope: "repo", Repo: "o/r"},
	}}
	got := apiPrefixes(cfg)
	if len(got) != 2 || got[0] != "orgs/o" || got[1] != "repos/o/r" {
		t.Errorf("apiPrefixes = %v", got)
	}
}
```

(add `"github.com/a1678991/github-qemu-runner/internal/config"` to the test file's imports).

**Consistency note:** `HostMemMB()` returns 0 on failure; `CapacityWarnings` (Task 1) treats `<= 0` host values as "unknown — don't warn", so a failed `/proc/meminfo` read never produces bogus warnings.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./internal/...`
Expected: PASS across all packages.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/ internal/config/
git commit -m "feat: add orphan reaping and controller wiring"
```

---

### Task 11: imagebake package

**Files:**
- Create: `internal/imagebake/bake.go`
- Test: `internal/imagebake/bake_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/imagebake/bake_test.go`:

```go
package imagebake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseSHA256SUMS(t *testing.T) {
	sums := []byte(
		"aaaa1111 *noble-server-cloudimg-amd64.img\n" +
			"bbbb2222  noble-server-cloudimg-arm64.img\n")
	got, err := ParseSHA256SUMS(sums, "noble-server-cloudimg-amd64.img")
	if err != nil || got != "aaaa1111" {
		t.Errorf("got %q, %v", got, err)
	}
	got, err = ParseSHA256SUMS(sums, "noble-server-cloudimg-arm64.img")
	if err != nil || got != "bbbb2222" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := ParseSHA256SUMS(sums, "absent.img"); err == nil {
		t.Error("want error for absent file")
	}
}

func TestDownloadVerified(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("hello"))
	}))
	defer srv.Close()
	// sha256("hello")
	const sha = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

	dest := filepath.Join(t.TempDir(), "f.img")
	ctx := context.Background()
	if err := DownloadVerified(ctx, srv.Client(), srv.URL, dest, sha); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "hello" {
		t.Errorf("content = %q", b)
	}
	// second call: cache hit, no new request
	if err := DownloadVerified(ctx, srv.Client(), srv.URL, dest, sha); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1 (cached)", hits.Load())
	}
	// checksum mismatch must fail and leave no file
	bad := filepath.Join(t.TempDir(), "bad.img")
	if err := DownloadVerified(ctx, srv.Client(), srv.URL, bad, strings.Repeat("0", 64)); err == nil {
		t.Error("want checksum mismatch error")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("mismatched download left a file behind")
	}
}

func TestLatestRunner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/actions/runner/releases/latest" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.335.1",
			"body": "## SHA-256\n" +
				"actions-runner-linux-x64-2.335.1.tar.gz " +
				"<!-- BEGIN SHA --> abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789 <!-- END -->",
			"assets": []map[string]any{
				{"name": "actions-runner-linux-arm64-2.335.1.tar.gz", "browser_download_url": "https://x/arm64.tar.gz"},
				{"name": "actions-runner-linux-x64-2.335.1.tar.gz", "browser_download_url": "https://x/x64.tar.gz"},
			},
		})
	}))
	defer srv.Close()
	rel, err := LatestRunner(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "2.335.1" || rel.TarballURL != "https://x/x64.tar.gz" {
		t.Errorf("rel = %+v", rel)
	}
	if rel.SHA256 != "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Errorf("SHA256 = %q", rel.SHA256)
	}
}

func TestBakeUserData(t *testing.T) {
	ud, err := BakeUserData(Release{Version: "2.335.1", TarballURL: "https://x/t.gz", SHA256: "ff"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatal("missing #cloud-config header")
	}
	var doc struct {
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
		RunCmd [][]string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(ud), &doc); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, wf := range doc.WriteFiles {
		paths[wf.Path] = wf.Content
	}
	for _, p := range []string{"/run/bake-env", "/run/bake.sh", "/run/run-one-job"} {
		if paths[p] == "" {
			t.Errorf("missing write_file %s", p)
		}
	}
	if !strings.Contains(paths["/run/bake-env"], `TARBALL_URL="https://x/t.gz"`) {
		t.Errorf("bake-env = %q", paths["/run/bake-env"])
	}
	if len(doc.RunCmd) != 1 || doc.RunCmd[0][1] != "/run/bake.sh" {
		t.Errorf("runcmd = %v", doc.RunCmd)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/imagebake/`
Expected: FAIL — `undefined: ParseSHA256SUMS` etc.

- [ ] **Step 3: Write the implementation**

`internal/imagebake/bake.go`:

```go
// Package imagebake builds the base VM image: Ubuntu 24.04 cloud image +
// Docker + actions-runner, flattened into <state>/images/base.qcow2 and
// swapped in atomically.
package imagebake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/a1678991/github-qemu-runner/internal/qemu"
	"github.com/a1678991/github-qemu-runner/internal/seed"
	"github.com/a1678991/github-qemu-runner/scripts"
)

const (
	DefaultImageURL = "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
	DefaultSumsURL  = "https://cloud-images.ubuntu.com/noble/current/SHA256SUMS"
)

type Options struct {
	StateDir string
	HTTP     *http.Client
	APIBase  string
	ImageURL string
	SumsURL  string
	QEMUBin  string
	CPUs     int
	MemoryMB int
	Timeout  time.Duration
	Log      *slog.Logger
}

func (o *Options) defaults() {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 15 * time.Minute}
	}
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	if o.ImageURL == "" {
		o.ImageURL = DefaultImageURL
	}
	if o.SumsURL == "" {
		o.SumsURL = DefaultSumsURL
	}
	if o.CPUs == 0 {
		o.CPUs = 4
	}
	if o.MemoryMB == 0 {
		o.MemoryMB = 4096
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
}

// Release identifies an actions/runner build for linux-x64.
type Release struct {
	Version    string
	TarballURL string
	SHA256     string
}

// ParseSHA256SUMS finds filename's hash in sha256sum-format output
// (lines of "<hash> *<name>" or "<hash>  <name>").
func ParseSHA256SUMS(sums []byte, filename string) (string, error) {
	for line := range strings.Lines(string(sums)) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == filename {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s not found in SHA256SUMS", filename)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DownloadVerified fetches url to dest unless dest already matches wantSHA.
// Empty wantSHA skips verification (TLS is then the only integrity check).
func DownloadVerified(ctx context.Context, client *http.Client, url, dest, wantSHA string) error {
	if wantSHA != "" {
		if got, err := fileSHA256(dest); err == nil && got == wantSHA {
			return nil // cached
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if wantSHA != "" {
		got, err := fileSHA256(tmp)
		if err != nil {
			return err
		}
		if got != wantSHA {
			os.Remove(tmp)
			return fmt.Errorf("%s: checksum mismatch: got %s want %s", url, got, wantSHA)
		}
	}
	return os.Rename(tmp, dest)
}

// LatestRunner resolves the newest actions/runner release for linux-x64.
// The tarball SHA is scraped from the release notes; if the notes format
// changes, SHA256 comes back empty and the caller proceeds on TLS alone.
func LatestRunner(ctx context.Context, client *http.Client, apiBase string) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(apiBase, "/")+"/repos/actions/runner/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("releases/latest: %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, err
	}
	version := strings.TrimPrefix(rel.TagName, "v")
	want := fmt.Sprintf("actions-runner-linux-x64-%s.tar.gz", version)
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
	// First standalone 64-hex token after the asset name in the release
	// notes. (?s) lets .*? cross newlines; \b keeps it from grabbing a
	// 64-char slice of a longer hex run.
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(want) + `.*?\b([0-9a-fA-F]{64})\b`)
	if m := re.FindStringSubmatch(rel.Body); m != nil {
		out.SHA256 = strings.ToLower(m[1])
	}
	return out, nil
}

// BakeUserData renders the cloud-config for the one-time bake boot: drop
// the env file and scripts into /run, then execute bake.sh.
func BakeUserData(rel Release) (string, error) {
	env := fmt.Sprintf("VERSION=%q\nTARBALL_URL=%q\nTARBALL_SHA256=%q\n",
		rel.Version, rel.TarballURL, rel.SHA256)
	doc := map[string]any{
		"write_files": []map[string]any{
			{"path": "/run/bake-env", "permissions": "0600", "content": env},
			{"path": "/run/bake.sh", "permissions": "0755", "content": scripts.Bake},
			{"path": "/run/run-one-job", "permissions": "0755", "content": scripts.RunOneJob},
		},
		"runcmd": [][]string{{"bash", "/run/bake.sh"}},
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(b), nil
}

// Bake produces <state>/images/base.qcow2: download + verify the cloud
// image, resolve the runner release, boot a build overlay with the bake
// seed, require the BAKE-OK serial-console sentinel, flatten, swap.
func Bake(ctx context.Context, o Options) error {
	o.defaults()
	imagesDir := filepath.Join(o.StateDir, "images")
	bakeDir := filepath.Join(imagesDir, "bake")
	if err := os.MkdirAll(bakeDir, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(bakeDir)

	sums, err := fetchAll(ctx, o.HTTP, o.SumsURL)
	if err != nil {
		return fmt.Errorf("fetch SHA256SUMS: %w", err)
	}
	wantSHA, err := ParseSHA256SUMS(sums, path.Base(o.ImageURL))
	if err != nil {
		return err
	}
	cloudImg := filepath.Join(imagesDir, path.Base(o.ImageURL))
	o.Log.Info("downloading cloud image (cached if unchanged)", "url", o.ImageURL)
	if err := DownloadVerified(ctx, o.HTTP, o.ImageURL, cloudImg, wantSHA); err != nil {
		return err
	}

	rel, err := LatestRunner(ctx, o.HTTP, o.APIBase)
	if err != nil {
		return fmt.Errorf("resolve runner release: %w", err)
	}
	if rel.SHA256 == "" {
		o.Log.Warn("runner tarball SHA not found in release notes; relying on TLS only")
	}
	o.Log.Info("baking image", "runner_version", rel.Version)

	absCloudImg, err := filepath.Abs(cloudImg)
	if err != nil {
		return err
	}
	overlay := filepath.Join(bakeDir, "bake-overlay.qcow2")
	if err := qemu.CreateOverlay(ctx, absCloudImg, overlay, 20); err != nil {
		return err
	}

	ud, err := BakeUserData(rel)
	if err != nil {
		return err
	}
	iso, err := seed.BuildISO(ctx, bakeDir, ud, seed.MetaData("ghq-bake-"+rel.Version, "ghq-bake"))
	if err != nil {
		return err
	}

	console := filepath.Join(bakeDir, "console.log")
	vm, err := qemu.Start(ctx, o.QEMUBin, qemu.Spec{
		Name: "ghq-bake", CPUs: o.CPUs, MemoryMB: o.MemoryMB,
		OverlayPath: overlay, SeedISOPath: iso, ConsoleLog: console,
		QMPSocket: filepath.Join(bakeDir, "qmp.sock"),
		PIDFile:   filepath.Join(bakeDir, "qemu.pid"),
	})
	if err != nil {
		return err
	}
	select {
	case <-vm.Done():
	case <-time.After(o.Timeout):
		_ = vm.Kill()
		return fmt.Errorf("bake timed out after %v", o.Timeout)
	case <-ctx.Done():
		_ = vm.Kill()
		return ctx.Err()
	}

	consoleOut, err := os.ReadFile(console)
	if err != nil {
		return fmt.Errorf("read console log: %w", err)
	}
	if !bytes.Contains(consoleOut, []byte("BAKE-OK")) {
		tail := consoleOut
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return fmt.Errorf("bake failed (no BAKE-OK sentinel); console tail:\n%s", tail)
	}

	newBase := filepath.Join(imagesDir, "base.qcow2.new")
	if out, err := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2",
		overlay, newBase).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %v: %s", err, out)
	}
	if err := os.Rename(newBase, filepath.Join(imagesDir, "base.qcow2")); err != nil {
		return err
	}

	meta, err := json.MarshalIndent(map[string]string{
		"runner_version":     rel.Version,
		"cloud_image_sha256": wantSHA,
		"baked_at":           time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(imagesDir, "base.json"), append(meta, '\n'), 0o644); err != nil {
		return err
	}
	o.Log.Info("bake complete", "base", filepath.Join(imagesDir, "base.qcow2"))
	return nil
}

func fetchAll(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/imagebake/`
Expected: PASS. (`Bake` itself is exercised by the manual smoke test in Task 14 — it boots a real VM.)

- [ ] **Step 5: Commit**

```bash
git add internal/imagebake/
git commit -m "feat: add image bake pipeline with checksum verification"
```

---

### Task 12: main wiring + setup preflight

**Files:**
- Modify: `cmd/github-qemu-runner/main.go` (replace the stub entirely)

- [ ] **Step 1: Replace main.go**

```go
// Command github-qemu-runner runs ephemeral GitHub Actions runners in
// QEMU/KVM virtual machines: `controller` supervises runner pools,
// `refresh-image` (re)bakes the base VM image, `setup` runs preflight
// checks. See packaging/config.example.yaml for configuration.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/controller"
	"github.com/a1678991/github-qemu-runner/internal/github"
	"github.com/a1678991/github-qemu-runner/internal/imagebake"
)

func main() {
	configPath := flag.String("config", "/etc/github-qemu-runner/config.yaml", "path to config file")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "controller"
	}
	var err error
	switch cmd {
	case "controller":
		err = runController(ctx, *configPath, log)
	case "refresh-image":
		err = runRefreshImage(ctx, *configPath, log)
	case "setup":
		err = runSetup(ctx, *configPath)
	default:
		fmt.Fprintln(os.Stderr, "usage: github-qemu-runner [-config PATH] <controller|refresh-image|setup>")
		os.Exit(2)
	}
	if err != nil {
		log.Error(cmd+" failed", "err", err)
		os.Exit(1)
	}
}

func runController(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	return controller.Run(ctx, cfg, log)
}

func runRefreshImage(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	qemuBin, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		return err
	}
	return imagebake.Bake(ctx, imagebake.Options{
		StateDir: cfg.StateDir,
		APIBase:  cfg.GitHub.APIBaseURL,
		QEMUBin:  qemuBin,
		Log:      log,
	})
}

func runSetup(ctx context.Context, configPath string) error {
	failed := false
	check := func(name string, err error) {
		if err != nil {
			failed = true
			fmt.Printf("FAIL  %s: %v\n", name, err)
		} else {
			fmt.Printf("ok    %s\n", name)
		}
	}

	cfg, err := config.Load(configPath)
	check("config "+configPath, err)
	if err != nil {
		return fmt.Errorf("setup found problems")
	}

	for _, bin := range []string{"qemu-system-x86_64", "qemu-img", "genisoimage"} {
		_, lookErr := exec.LookPath(bin)
		check(bin+" on PATH", lookErr)
	}

	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err == nil {
		kvm.Close()
	}
	check("/dev/kvm read-write access", err)

	keyPEM, err := os.ReadFile(cfg.GitHub.PrivateKeyPath)
	check("private key readable", err)
	if err == nil {
		key, parseErr := github.ParseRSAPrivateKey(keyPEM)
		check("private key parses", parseErr)
		if parseErr == nil {
			gh := github.New(cfg.GitHub.APIBaseURL, cfg.GitHub.AppID, cfg.GitHub.InstallationID, key)
			check("GitHub App auth (installation token)", gh.CheckAuth(ctx))
		}
	}

	base := filepath.Join(cfg.StateDir, "images", "base.qcow2")
	if _, err := os.Stat(base); err != nil {
		fmt.Printf("note  base image missing; run `github-qemu-runner refresh-image`\n")
	} else {
		fmt.Printf("ok    base image %s\n", base)
	}

	for _, w := range cfg.CapacityWarnings(runtime.NumCPU(), controller.HostMemMB()) {
		fmt.Printf("warn  %s\n", w)
	}

	if failed {
		return fmt.Errorf("setup found problems")
	}
	return nil
}
```

- [ ] **Step 2: Verify build, vet, and CLI behavior**

Run: `go build ./... && go vet ./...`
Expected: clean.

Run: `go run ./cmd/github-qemu-runner nonsense; echo "exit=$?"`
Expected: usage line, `exit=2`.

Run: `go run ./cmd/github-qemu-runner -config /nonexistent.yaml setup; echo "exit=$?"`
Expected: `FAIL  config /nonexistent.yaml: ...no such file...`, `exit=1`.

- [ ] **Step 3: Run the full test suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/github-qemu-runner/main.go
git commit -m "feat: wire controller, refresh-image, and setup subcommands"
```

---

### Task 13: packaging (systemd unit, example config) + README

**Files:**
- Create: `packaging/github-qemu-runner.service`
- Create: `packaging/config.example.yaml`
- Create: `README.md` (replaces nothing — repo has no README yet)

**Design note:** the spec names systemd `LoadCredential` for the App key. Default documented layout here is the simpler direct path owned by `gh-runner` (mode 0600) so that `setup`/`refresh-image` work identically inside and outside systemd; the unit ships the `LoadCredential` lines commented out as the hardening option (config then uses `${CREDENTIALS_DIRECTORY}/app-key.pem`, which `config.Load` env-expands).

- [ ] **Step 1: Create packaging/github-qemu-runner.service**

```ini
[Unit]
Description=Ephemeral GitHub Actions runners on QEMU/KVM
Documentation=https://github.com/a1678991/github-qemu-runner
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
User=gh-runner
Group=gh-runner
SupplementaryGroups=kvm
ExecStart=/usr/local/bin/github-qemu-runner -config /etc/github-qemu-runner/config.yaml controller
Restart=on-failure
RestartSec=10
# Must exceed the largest pool drain_timeout (default 30m) plus margin,
# or systemd SIGKILLs mid-drain.
TimeoutStopSec=35m

# Hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
StateDirectory=github-qemu-runner
ReadWritePaths=/var/lib/github-qemu-runner

# Hardening option: keep the App key root-owned and pass it as a systemd
# credential. Set github.private_key_path in config.yaml to
# ${CREDENTIALS_DIRECTORY}/app-key.pem when enabling this.
#LoadCredential=app-key.pem:/etc/github-qemu-runner/app-key.pem

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 2: Create packaging/config.example.yaml**

```yaml
# github-qemu-runner configuration. Copy to
# /etc/github-qemu-runner/config.yaml and edit.

github:
  # Your GitHub App (needs: org "Self-hosted runners: Read & write" for
  # org pools, repo "Administration: Read & write" for repo pools).
  app_id: 123456
  installation_id: 7890123
  private_key_path: /etc/github-qemu-runner/app-key.pem
  # api_base_url: https://api.github.com

# state_dir: /var/lib/github-qemu-runner

pools:
  - name: fmt
    scope: org # org | repo (repo also needs `repo: owner/name`)
    org: my-org
    count: 2
    cpus: 2
    memory_mb: 2048
    disk_gb: 20
    labels: [self-hosted, linux, x64, fmt]
    # runner_group: Default
    # liveness_timeout: 5m
    # drain_timeout: 30m
  - name: build
    scope: org
    org: my-org
    count: 1
    cpus: 8
    memory_mb: 16384
    disk_gb: 60
    labels: [self-hosted, linux, x64, build]
```

- [ ] **Step 3: Create README.md**

````markdown
# github-qemu-runner

Ephemeral GitHub Actions self-hosted runners on Linux. Every job runs in a
disposable QEMU/KVM virtual machine that is destroyed afterwards — the VM is
the isolation boundary. Linux sibling of
[github-tart-runner](https://github.com/a1678991/github-tart-runner) (macOS).

Design: `docs/superpowers/specs/2026-06-10-qemu-runner-design.md`.

## How it works

A Go daemon (`controller`) supervises static pools of runner slots. Per
slot, forever:

1. `POST .../generate-jitconfig` — pre-register an ephemeral runner (GitHub App auth)
2. `qemu-img create` a copy-on-write overlay of the baked base image
3. Build a cloud-init NoCloud seed ISO carrying the JIT config
4. Boot `qemu-system-x86_64` (KVM, user-mode networking, no inbound)
5. The guest runs exactly one job, then powers off; the QEMU process exits
6. Delete the workdir + runner record, loop

`refresh-image` bakes the base image: Ubuntu 24.04 cloud image + Docker +
actions-runner (latest, checksum-verified), flattened to
`/var/lib/github-qemu-runner/images/base.qcow2`.

## Requirements

- Linux host with `/dev/kvm`, systemd
- `qemu-system-x86_64`, `qemu-img`, `genisoimage` on PATH
  (Arch: `pacman -S qemu-base cdrtools`; Debian/Ubuntu: `apt install qemu-system-x86 qemu-utils genisoimage`)
- A GitHub App with **Self-hosted runners: Read & write** (org) and/or
  **Administration: Read & write** (repo), installed on the target org/repos

## Install

```bash
go build -o github-qemu-runner ./cmd/github-qemu-runner
sudo install -m 0755 github-qemu-runner /usr/local/bin/

sudo useradd --system --home-dir /var/lib/github-qemu-runner \
  --shell /usr/sbin/nologin --groups kvm gh-runner
sudo mkdir -p /etc/github-qemu-runner /var/lib/github-qemu-runner
sudo chown gh-runner:gh-runner /var/lib/github-qemu-runner

sudo cp packaging/config.example.yaml /etc/github-qemu-runner/config.yaml
sudoedit /etc/github-qemu-runner/config.yaml   # app_id, installation_id, pools
sudo install -m 0600 -o gh-runner -g gh-runner \
  /path/to/app-private-key.pem /etc/github-qemu-runner/app-key.pem

sudo -u gh-runner github-qemu-runner setup          # preflight: all "ok"?
sudo -u gh-runner github-qemu-runner refresh-image  # bake base image (~10 min)

sudo cp packaging/github-qemu-runner.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now github-qemu-runner
```

Use it from a workflow:

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64, build]
```

## Runbook

| | |
|---|---|
| Logs | `journalctl -u github-qemu-runner -f` |
| Per-VM console | `/var/lib/github-qemu-runner/run/<vm>/console.log` (gone after teardown) |
| Refresh base image | `sudo -u gh-runner github-qemu-runner refresh-image` (monthly, or after runner/Ubuntu releases; running VMs are unaffected, new VMs pick it up) |
| Image provenance | `/var/lib/github-qemu-runner/images/base.json` |
| Stop (drains) | `systemctl stop github-qemu-runner` — idle runners are deregistered immediately; busy ones get `drain_timeout` (default 30 min) to finish |
| Crash recovery | automatic: systemd restarts; startup reaping kills orphan VMs and deletes stale `ghq-*` runner records |

## Security notes

- The VM is the isolation boundary; the guest `runner` user has no sudo
  (Docker group membership is the same documented trade-off as
  GitHub-hosted runners).
- Outbound-only user-mode networking; nothing can connect into a guest.
- JIT configs are single-use and bound to one pre-created runner record;
  they exist on disk only inside a per-VM seed ISO (0600) that is deleted
  on teardown.
- Do not attach these runners to public repositories (fork-PR risk — see
  GitHub's self-hosted runner hardening guide).
- Hardening option: pass the App key via systemd `LoadCredential` (see the
  commented lines in the unit file).
````

- [ ] **Step 4: Verify systemd unit syntax and lint**

Run: `systemd-analyze verify packaging/github-qemu-runner.service 2>&1 | grep -v "Command 'github-qemu-runner' not found" || true`
Expected: no errors other than the not-yet-installed binary path (acceptable pre-install).

- [ ] **Step 5: Commit**

```bash
git add packaging/ README.md
git commit -m "feat: add systemd unit, example config, and README"
```

---

### Task 14: Final verification + manual smoke test

- [ ] **Step 1: Full local check matrix (same as CI)**

Run:
```bash
golangci-lint run && golangci-lint fmt --diff \
  && git ls-files '*.sh' | xargs -r shfmt -d \
  && git ls-files '*.sh' | xargs -r shellcheck \
  && actionlint && zizmor --offline .github/workflows/ \
  && npx secretlint "**/*" \
  && go test -race ./... && go build ./...
```
Expected: everything exits 0.

- [ ] **Step 2: Manual smoke test (real GitHub + real VM — requires App credentials)**

This is the spec's integration test; it needs a scratch repo/org and ~15 minutes.

1. Create a scratch config at `/tmp/ghq-smoke.yaml` with `state_dir: /tmp/ghq-state`, one pool (`count: 1`, `cpus: 2`, `memory_mb: 2048`, `disk_gb: 20`, distinctive label e.g. `smoke-ghq`), pointed at a scratch repo (`scope: repo`) or org.
2. `github-qemu-runner -config /tmp/ghq-smoke.yaml setup` → all `ok`.
3. `github-qemu-runner -config /tmp/ghq-smoke.yaml refresh-image` → ends with `bake complete`; `/tmp/ghq-state/images/base.qcow2` + `base.json` exist.
4. `github-qemu-runner -config /tmp/ghq-smoke.yaml controller` (foreground) → log shows `runner online`; the runner appears as Idle in GitHub repo/org settings → Actions → Runners.
5. Push a workflow with `runs-on: [self-hosted, smoke-ghq]` running `docker run --rm hello-world && uname -a`. It must succeed.
6. Observe: job completes → `VM exited` in the log → a NEW runner (different `ghq-*` suffix) comes online; `/tmp/ghq-state/run/` contains only the new VM's dir.
7. Ctrl-C the controller → idle runner is deleted from GitHub (check the UI), VM powers down, process exits cleanly.
8. `rm -rf /tmp/ghq-state` and delete the scratch config.

- [ ] **Step 3: Commit any remaining fixes; push if a remote exists**

```bash
git status   # should be clean
# No remote is configured yet at the time of writing. Create one first if
# desired, e.g.: gh repo create a1678991/github-qemu-runner --private --source . --push
git remote get-url origin && git push -u origin main || echo "no remote configured; skipping push"
```

---

## Deviations & deferred items (carry into future work)

- **Networking:** slirp only; passt/tap upgrade path deferred (spec "Open items").
- **Runner version pinning:** bake always uses latest release; a config pin is future work (spec "Open items").
- **LoadCredential:** shipped as a commented-out hardening option, not the default (see Task 13 design note).
- **Autoscaling:** `Pool.Run` derives slot count from `Cfg.Count` statically; the webhook-driven desired-count hook is future work per spec.



