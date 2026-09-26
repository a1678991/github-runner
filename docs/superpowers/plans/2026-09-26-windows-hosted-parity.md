# Windows Hosted-Parity Image Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The baked Windows image behaves like GitHub's `windows-2025` hosted image where CI depends on it: PowerShell 7, `gh`, `jq`, 7-Zip and `bash` on the machine PATH, MSVC Build Tools plus the Windows 11 SDK (`signtool`), Git configured like the hosted image, Windows Update / telemetry / maintenance off, and Defender scanning disabled — with a bake-time assertion that refuses to ship an image missing any of it.

**Architecture:** Three new `windows:` config keys (`packages`, `build_tools`, `disable_defender`) flow through `imageprep` into `imagebake.WindowsOptions`, then into the guest via a typed `bake-env.json`. `bake.ps1` keeps its existing WinGet bootstrap and bounded install helper, and gains: system tuning, Defender preferences (before the installs), Git parity, a configurable package loop with a `--scope machine` fallback, a conditional Build Tools install that explicitly adds the SDK, and a toolchain assertion whose one-line summary the host records in `base-windows.json`.

**Tech Stack:** Go 1.26 (stdlib + `gopkg.in/yaml.v3`), PowerShell 5.1 guest script, WinGet 1.12+, Visual Studio 2022 Build Tools, QEMU/KVM.

**Spec:** `docs/superpowers/specs/2026-09-26-windows-hosted-parity-design.md` (extends `docs/superpowers/specs/2026-09-23-windows-runner-design.md`)

## Global Constraints

- Branch `feat/windows-runner` (PR #14, open). Do not switch branches; do not push (the controller pushes).
- Go 1.26, no new dependencies; `go build ./... && go test ./... && mise exec -- golangci-lint run` clean before every commit.
- Conventional commits (commitlint enforces a lower-case subject start). End every commit message with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.
- `bake.ps1` stays PowerShell 5.1-compatible (no `??`, ternary, `-Parallel`, `ProcessStartInfo.ArgumentList`), LF line endings, pure ASCII.
- Every native command in `bake.ps1` that merges stderr (`2>&1`) runs with `$ErrorActionPreference` relaxed to `'Continue'` (the existing pattern; `TestWindowsAssetsEmbedded` enforces it per call site).
- The existing WinGet bootstrap (`Add-AppxPackage -RegisterByFamilyName`, `Repair-WinGetPackageManager`, version >= 1.12 check), the accepted exit-code set `$wingetOK`, the per-install timeout and hang diagnostics, and the VC++ runtime installs (2015+ first, 2013–2005 last) stay as they are.
- Default `windows.packages`: `Microsoft.PowerShell`, `GitHub.cli`, `jqlang.jq`, `7zip.7zip`. `build_tools` and `disable_defender` default to `true`.
- WinGet package IDs match `^[A-Za-z0-9][A-Za-z0-9.+_-]*$`; duplicates (case-insensitive) are rejected.
- `bake-env.json` always carries `packages` as a JSON array (never `null`), plus `build_tools` and `disable_defender` booleans.
- `-1978335216` is `APPINSTALLER_CLI_ERROR_NO_APPLICABLE_INSTALLER` (0x8A150010): retry that package once without `--scope`.
- Build Tools `--override` value exactly: `--wait --quiet --norestart --nocache --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended --add Microsoft.VisualStudio.Component.Windows11SDK.26100`.
- Bake timeout stays 90 minutes (`WindowsOptions.defaults`).
- No hostnames, IPs or personal paths in committed files.

## Review Focus

1. **`packages: []` with `build_tools: true`** — the toolchain assertion must not demand `pwsh`/`gh`/`jq`/`7z`; only `git` and `bash` are unconditional. Pinned in Task 3 (shape test that command checks are keyed off `$packages`).
2. **A nil package list reaching the guest as JSON `null`** — `ConvertFrom-Json` would yield one `$null` entry and WinGet would be asked to install `""`. Pinned in Task 2 (`bake-env.json` has `"packages": []` when `Packages` is nil) and Task 3 (the loop filters empty entries).
3. **A typo'd or hostile package ID in config** (`jq; rm -rf`, `.foo`, duplicate `GitHub.cli`/`github.cli`) — rejected at config load with the ID in the message, never passed to the guest. Pinned in Task 1.
4. **A package whose manifest has no machine-scoped installer** — installed without `--scope` after a logged retry, not a failed bake. Pinned in Task 3 (shape test for the `-1978335216` retry).
5. **`disable_defender: false`** — Defender left untouched and the assertion does not require real-time protection off. Pinned in Task 3 (both Defender blocks gated on `$env_.disable_defender`).

---

## File structure

| File | Change |
|---|---|
| `internal/config/config.go` | `Windows.Packages`, `Windows.BuildTools`, `Windows.DisableDefender`; `DefaultWindowsPackages`; defaults; validation |
| `internal/config/config_test.go` | Defaults, overrides, empty list, validation cases |
| `internal/imagebake/windows.go` | `WindowsOptions` fields; typed `bakeEnv`; provenance fields; `toolchainSummary` |
| `internal/imagebake/windows_test.go` | `bake-env.json` and provenance assertions; `toolchainSummary` test |
| `internal/imageprep/imageprep.go` | Pass the three options |
| `scripts/guest/windows/bake.ps1` | Helpers, system tuning, Defender, Git parity, WinGet helper split + package loop, conditional Build Tools + SDK, toolchain assertion |
| `scripts/embed_test.go` | Shape assertions for all of the above |
| `README.md`, `packaging/config.example.yaml`, `docs/superpowers/specs/2026-09-23-windows-runner-design.md` | Documentation |

---

### Task 1: Config keys `packages`, `build_tools`, `disable_defender`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Windows.Packages []string` (nil never survives `Load`: absent/null → copy of `DefaultWindowsPackages`, `[]` → empty), `config.Windows.BuildTools *bool` and `config.Windows.DisableDefender *bool` (never nil after `Load`), `var DefaultWindowsPackages []string`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
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
```

Add `"slices"` to the test file's imports if absent. Add these cases to the `cases` slice in `TestWindowsPoolValidation`:

```go
		{"bad package id", func(y string) string { return y + "windows:\n  packages: [\"jq; rm -rf\"]\n" }, `windows.packages: "jq; rm -rf" is not a WinGet package ID`},
		{"leading dot package", func(y string) string { return y + "windows:\n  packages: [.foo]\n" }, `windows.packages: ".foo" is not a WinGet package ID`},
		{"duplicate package", func(y string) string { return y + "windows:\n  packages: [GitHub.cli, github.cli]\n" }, `windows.packages: "github.cli" is listed twice`},
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'WindowsParity|WindowsPackages|WindowsPoolValidation' -v`
Expected: compile errors (`c.Windows.Packages`, `DefaultWindowsPackages` undefined).

- [ ] **Step 3: Implement**

In `internal/config/config.go`, after the `DefaultVirtioWin` constant block add:

```go
// DefaultWindowsPackages are the WinGet packages baked into the Windows
// image when windows.packages is absent: the everyday CLI tools GitHub's
// windows-2025 hosted image puts on PATH.
var DefaultWindowsPackages = []string{
	"Microsoft.PowerShell",
	"GitHub.cli",
	"jqlang.jq",
	"7zip.7zip",
}

// wingetIDRe matches WinGet package identifiers (e.g. "Microsoft.VCRedist.2015+.x64").
var wingetIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]*$`)
```

Add to the `Windows` struct after `OVMFDir`:

```go
	// Packages are WinGet package IDs installed machine-wide in the base
	// image. Absent (or null) means DefaultWindowsPackages; an explicit
	// empty list installs none.
	Packages []string `yaml:"packages"`
	// BuildTools bakes Visual Studio 2022 Build Tools (VCTools workload)
	// and the Windows 11 SDK. Pointer so an absent key defaults to true.
	BuildTools *bool `yaml:"build_tools"`
	// DisableDefender applies the GitHub-hosted image's Defender
	// preferences (real-time and related scanning off, C:\ excluded).
	// Pointer so an absent key defaults to true.
	DisableDefender *bool `yaml:"disable_defender"`
```

In `applyDefaults`, right after `c.Windows.OVMFDir = os.ExpandEnv(c.Windows.OVMFDir)`:

```go
	if c.Windows.Packages == nil {
		c.Windows.Packages = append([]string(nil), DefaultWindowsPackages...)
	}
	if c.Windows.BuildTools == nil {
		on := true
		c.Windows.BuildTools = &on
	}
	if c.Windows.DisableDefender == nil {
		on := true
		c.Windows.DisableDefender = &on
	}
```

In `validate`, right after the loop that calls `validateSource`:

```go
	seenPkg := map[string]bool{}
	for _, id := range c.Windows.Packages {
		if !wingetIDRe.MatchString(id) {
			return fmt.Errorf("windows.packages: %q is not a WinGet package ID", id)
		}
		// WinGet IDs are case-insensitive.
		if seenPkg[strings.ToLower(id)] {
			return fmt.Errorf("windows.packages: %q is listed twice", id)
		}
		seenPkg[strings.ToLower(id)] = true
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS, including all pre-existing tests.

- [ ] **Step 5: Lint and commit**

```bash
mise exec -- golangci-lint run ./internal/config/
git add internal/config
git commit -m "feat(config): add windows.packages, build_tools and disable_defender

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Carry the options into the bake and record them

**Files:**
- Modify: `internal/imagebake/windows.go`, `internal/imageprep/imageprep.go`
- Test: `internal/imagebake/windows_test.go`

**Interfaces:**
- Consumes: `config.Windows.Packages []string`, `*config.Windows.BuildTools`, `*config.Windows.DisableDefender` (Task 1; never nil after `Load`).
- Produces: `imagebake.WindowsOptions.Packages []string`, `.BuildTools bool`, `.DisableDefender bool`; `bake-env.json` keys `packages` (array), `build_tools` (bool), `disable_defender` (bool) alongside the existing six string keys; `base-windows.json` string keys `packages` (comma-joined), `build_tools`, `disable_defender` (`"true"`/`"false"`), `toolchain` (text after the last `toolchain: ` on the console, `""` if none); `func toolchainSummary(console []byte) string`.

- [ ] **Step 1: Write the failing tests**

In `internal/imagebake/windows_test.go`, in `TestBakeSeedContents` replace the block from `var e map[string]string` through the closing brace of the `if e["runner_url"] …` check with:

```go
	var e map[string]any
	if err := json.Unmarshal(env, &e); err != nil {
		t.Fatal(err)
	}
	if e["runner_url"] != "https://x/win.zip" || e["runner_sha256"] != strings.Repeat("e", 64) ||
		e["git_url"] != "https://x/git.exe" || e["git_sha256"] != strings.Repeat("d", 64) {
		t.Errorf("bake-env = %v", e)
	}
	// Packages left nil must still reach the guest as an array: a JSON null
	// becomes one $null entry in bake.ps1's package loop.
	if pk, ok := e["packages"].([]any); !ok || len(pk) != 0 {
		t.Errorf("bake-env packages = %#v, want []", e["packages"])
	}
	if e["build_tools"] != false || e["disable_defender"] != false {
		t.Errorf("bake-env build_tools=%v disable_defender=%v, want false/false (zero options)", e["build_tools"], e["disable_defender"])
	}
```

In `TestBakeWindows`, change the `BakeWindows` call's options to add:

```go
		Packages:        []string{"Microsoft.PowerShell", "jqlang.jq"},
		BuildTools:      true,
		DisableDefender: true,
```

and change `QEMUBin: fakeQEMU(t, dir, "BAKE-OK"),` to

```go
		QEMUBin:         fakeQEMU(t, dir, "[bake 00:00:01] toolchain: git=git version 2.55.0 pwsh=7.6.6\n[bake 00:00:02] BAKE-OK"),
```

(the literal newline lands inside the fake's single-quoted `echo`, so the console gets two lines). After the existing `meta[...]` check, add:

```go
	if meta["packages"] != "Microsoft.PowerShell,jqlang.jq" || meta["build_tools"] != "true" ||
		meta["disable_defender"] != "true" || meta["toolchain"] != "git=git version 2.55.0 pwsh=7.6.6" {
		t.Errorf("parity provenance = packages:%q build_tools:%q disable_defender:%q toolchain:%q",
			meta["packages"], meta["build_tools"], meta["disable_defender"], meta["toolchain"])
	}
```

Append a new test:

```go
func TestToolchainSummary(t *testing.T) {
	cases := []struct{ console, want string }{
		{"", ""},
		{"[bake 1] start\r\n[bake 2] BAKE-OK\r\n", ""},
		{"[bake 1] toolchain: git=2.55 bash=5.2\r\n[bake 2] BAKE-OK\r\n", "git=2.55 bash=5.2"},
		{"[bake 1] toolchain: old\n[bake 2] toolchain: git=2.55 msvc=C:\\VS sdk=10.0.26100.0\n", "git=2.55 msvc=C:\\VS sdk=10.0.26100.0"},
		{"[bake 1] toolchain: no-newline-at-end", "no-newline-at-end"},
	}
	for _, tc := range cases {
		if got := toolchainSummary([]byte(tc.console)); got != tc.want {
			t.Errorf("toolchainSummary(%q) = %q, want %q", tc.console, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/imagebake/ -run 'TestBakeWindows$|TestBakeSeedContents|TestToolchainSummary' -v`
Expected: compile errors (`Packages`, `BuildTools`, `DisableDefender`, `toolchainSummary` undefined).

- [ ] **Step 3: Implement**

In `internal/imagebake/windows.go`, add `"strings"` to the imports. Add to `WindowsOptions` after `VirtioWinSHA256`:

```go
	// Packages are WinGet package IDs installed machine-wide by bake.ps1.
	// nil installs none (config supplies the default list).
	Packages []string
	// BuildTools installs VS 2022 Build Tools (VCTools) and the Windows 11 SDK.
	BuildTools bool
	// DisableDefender applies the GitHub-hosted image's Defender preferences.
	DisableDefender bool
```

Add below the artifact-name constants:

```go
// bakeEnv is bake-env.json, read by bake.ps1 through ConvertFrom-Json.
type bakeEnv struct {
	RunnerVersion   string   `json:"runner_version"`
	RunnerURL       string   `json:"runner_url"`
	RunnerSHA256    string   `json:"runner_sha256"`
	GitVersion      string   `json:"git_version"`
	GitURL          string   `json:"git_url"`
	GitSHA256       string   `json:"git_sha256"`
	Packages        []string `json:"packages"`
	BuildTools      bool     `json:"build_tools"`
	DisableDefender bool     `json:"disable_defender"`
}

// toolchainSummary returns what bake.ps1 logged after "toolchain: " on
// the last such console line, or "" when there is none.
func toolchainSummary(console []byte) string {
	const marker = "toolchain: "
	i := bytes.LastIndex(console, []byte(marker))
	if i < 0 {
		return ""
	}
	rest := console[i+len(marker):]
	if j := bytes.IndexByte(rest, '\n'); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(string(rest))
}
```

Replace the `env, err := json.MarshalIndent(map[string]string{ … }, "", "  ")` statement with:

```go
	pkgs := o.Packages
	if pkgs == nil {
		// bake.ps1 iterates this; JSON null would arrive as one $null entry.
		pkgs = []string{}
	}
	env, err := json.MarshalIndent(bakeEnv{
		RunnerVersion:   runner.Version,
		RunnerURL:       runner.TarballURL,
		RunnerSHA256:    runner.SHA256,
		GitVersion:      git.Version,
		GitURL:          git.TarballURL,
		GitSHA256:       git.SHA256,
		Packages:        pkgs,
		BuildTools:      o.BuildTools,
		DisableDefender: o.DisableDefender,
	}, "", "  ")
```

In the provenance map literal `prov := map[string]string{ … }` add four entries:

```go
		"packages":         strings.Join(o.Packages, ","),
		"build_tools":      strconv.FormatBool(o.BuildTools),
		"disable_defender": strconv.FormatBool(o.DisableDefender),
		"toolchain":        toolchainSummary(consoleOut),
```

and change the final log line to:

```go
	o.Log.Info("windows bake complete", "base", filepath.Join(o.ImageDir, WindowsBase), "toolchain", prov["toolchain"])
```

In `internal/imageprep/imageprep.go`, add to the `imagebake.WindowsOptions{…}` literal:

```go
			Packages:        cfg.Windows.Packages,
			BuildTools:      *cfg.Windows.BuildTools,
			DisableDefender: *cfg.Windows.DisableDefender,
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go build ./... && go test ./internal/imagebake/ ./internal/imageprep/ ./internal/config/ -v`
Expected: PASS (qemu-img and genisoimage are installed on this host, so the bake tests run rather than skip).

- [ ] **Step 5: Lint and commit**

```bash
mise exec -- golangci-lint run
git add internal/imagebake internal/imageprep
git commit -m "feat(imagebake): pass package, build-tools and defender options to the windows bake

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Hosted-parity bake script

**Files:**
- Modify: `scripts/guest/windows/bake.ps1`
- Test: `scripts/embed_test.go`

**Interfaces:**
- Consumes: `bake-env.json` keys `packages` (array of strings), `build_tools` (bool), `disable_defender` (bool) from Task 2.
- Produces: serial-console line `[bake HH:mm:ss] toolchain: <name>=<version> … [msvc=<path> sdk=<ver>] [defender_rt=off]` immediately before `BAKE-OK` (Task 2's `toolchainSummary` reads it).

- [ ] **Step 1: Write the failing shape tests**

In `scripts/embed_test.go`, inside `TestWindowsAssetsEmbedded`, extend the `for _, call := range []string{"pnputil.exe /add-driver", "& $gitExe --version"}` list to `[]string{"pnputil.exe /add-driver", "& $gitExe --version", "& $exe @argv"}`, then append at the end of the function:

```go
	// Hosted-image parity (docs/superpowers/specs/2026-09-26-windows-hosted-parity-design.md).
	for _, want := range []string{
		// config reaches the guest
		"$env_.packages", "$env_.build_tools", "$env_.disable_defender",
		// Git configured like the hosted image
		"/COMPONENTS=gitlfs", "/o:EnableSymlinks=Enabled", "/o:BashTerminalOption=ConHost",
		"Add-MachinePath 'C:\\Program Files\\Git\\bin'", "safe.directory", "GCM_INTERACTIVE",
		// system tuning
		"NoAutoUpdate", "DisableWindowsUpdateAccess", "WaaSMedicSvc", "AllowTelemetry",
		"MaintenanceDisabled", "ConsentPromptBehaviorAdmin", "SysMain", "ServicesPipeTimeout",
		// Defender
		"Set-MpPreference", "DisableRealtimeMonitoring", "DisableBehaviorMonitoring",
		"DisableIOAVProtection", "ExclusionPath", "RealTimeProtectionEnabled",
		// packages: machine scope with a retry on "no applicable installer"
		"'--scope', 'machine'", "-1978335216", "Assert-WinGetResult",
		// Build Tools + SDK
		"Microsoft.VisualStudio.Component.Windows11SDK.26100",
		// toolchain assertion and summary
		"signtool.exe", "Log \"toolchain: ",
		// empty or null package entries never reach winget
		"$env_.packages | Where-Object { $_ }",
		// a function that returns a value must not leak WaitForExit's bool
		"$null = $p.WaitForExit()",
	} {
		if !strings.Contains(WindowsBake, want) {
			t.Errorf("bake.ps1 missing %q", want)
		}
	}
	// gh and jq come from the configurable package list, not hardcoded installs.
	for _, gone := range []string{"Install-WinGetPackage 'GitHub.cli'", "Install-WinGetPackage 'jqlang.jq'"} {
		if strings.Contains(WindowsBake, gone) {
			t.Errorf("bake.ps1 still hardcodes %q; it belongs to windows.packages", gone)
		}
	}
	// Tool checks for packaged commands are keyed off the configured list,
	// so `packages: []` does not demand pwsh/gh/jq/7z.
	if !strings.Contains(WindowsBake, "foreach ($id in $packages) { if ($pkgCommands.ContainsKey($id))") {
		t.Error("bake.ps1: packaged-command checks must be derived from $packages")
	}
	// Both Defender blocks (apply, then assert) are gated on the option.
	if n := strings.Count(WindowsBake, "if ($env_.disable_defender)"); n < 2 {
		t.Errorf("bake.ps1: want the Defender apply and check both gated on $env_.disable_defender, found %d", n)
	}
	// Build Tools install and its checks are gated on the option.
	if n := strings.Count(WindowsBake, "if ($env_.build_tools)"); n < 2 {
		t.Errorf("bake.ps1: want the Build Tools install and check both gated on $env_.build_tools, found %d", n)
	}
	// Order: Defender off before the first WinGet install (it speeds the
	// installs up); the toolchain summary right before the sentinel.
	defender := strings.Index(WindowsBake, "Set-MpPreference @pref")
	firstInstall := strings.Index(WindowsBake, `Install-WinGetPackage "Microsoft.VCRedist.2015+.$arch"`)
	summary := strings.Index(WindowsBake, "Log \"toolchain: ")
	ok := strings.Index(WindowsBake, "Log 'BAKE-OK'")
	if defender < 0 || firstInstall < 0 || defender > firstInstall {
		t.Errorf("bake.ps1: Defender preferences (offset %d) must be applied before the first WinGet install (offset %d)", defender, firstInstall)
	}
	if summary < 0 || ok < 0 || summary > ok {
		t.Errorf("bake.ps1: the toolchain summary (offset %d) must be logged before BAKE-OK (offset %d)", summary, ok)
	}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/ -run TestWindowsAssetsEmbedded -v`
Expected: FAIL listing the missing strings.

- [ ] **Step 3: Update the header comment**

Replace the first five lines of `scripts/guest/windows/bake.ps1` (the comment block ending `# rejected quickly instead of hanging until the host timeout.`) with:

```powershell
# Runs ONCE as Administrator (unattend FirstLogonCommands) during the
# image bake boot. Installs virtio drivers, the runner user, and a toolchain
# matching GitHub's windows-2025 hosted image where CI depends on it (Git,
# WinGet packages, VS Build Tools + Windows SDK), applies the hosted image's
# Windows Update / telemetry / Defender posture, installs the actions runner,
# checks the toolchain, then powers off. The host watches the serial console
# for BAKE-OK; any failure prints BAKE-FAILED and powers off so the bake is
# rejected quickly instead of hanging until the host timeout.
```

- [ ] **Step 4: Add the helpers**

Immediately after the closing brace of `function Get-Verified`, add:

```powershell
# Appends $dir to the machine PATH (idempotent) and to this session's.
function Add-MachinePath([string]$dir) {
    $cur = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if (($cur -split ';') -notcontains $dir) {
        [Environment]::SetEnvironmentVariable('Path', "$cur;$dir", 'Machine')
    }
    if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }
}

# Creates the key when needed, then sets one value.
function Set-Reg([string]$path, [string]$name, $value, [string]$type = 'DWord') {
    if (-not (Test-Path $path)) { New-Item -Path $path -Force | Out-Null }
    Set-ItemProperty -Path $path -Name $name -Value $value -Type $type
}

# Runs a native command and returns its trimmed output, throwing on a
# non-zero exit. Under $ErrorActionPreference = 'Stop' the stderr that 2>&1
# merges arrives as ErrorRecords and would abort before the rc check.
function Invoke-Native([string]$exe, [string[]]$argv) {
    $saved = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = (& $exe @argv 2>&1 | Out-String).Trim()
    } finally {
        $ErrorActionPreference = $saved
    }
    if ($LASTEXITCODE -ne 0) { throw "$exe $($argv -join ' ') failed rc=$LASTEXITCODE : $out" }
    return $out
}
```

- [ ] **Step 5: Replace the policies block with system tuning plus Defender**

Replace the whole section from `        # --- policies and services -------------------------------------------` through `        Log 'policies applied'` (inclusive) with:

```powershell
        # --- policies and services -------------------------------------------
        # GitHub-hosted parity (actions/runner-images Configure-System.ps1 and
        # Configure-BaseImage.ps1): nothing in a job VM updates, reports or
        # maintains itself in the background, and admins get no UAC prompt.
        Set-Service wuauserv -StartupType Disabled
        Stop-Service wuauserv -Force -ErrorAction SilentlyContinue
        $wu = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate'
        Set-Reg "$wu\AU" NoAutoUpdate 1
        Set-Reg "$wu\AU" AUOptions 1
        Set-Reg $wu DoNotConnectToWindowsUpdateInternetLocations 1
        Set-Reg $wu DisableWindowsUpdateAccess 1
        # WaaSMedicSvc re-enables Windows Update and refuses Set-Service even
        # for administrators; the hosted image flips its Start value instead.
        try { Set-Reg 'HKLM:\SYSTEM\CurrentControlSet\Services\WaaSMedicSvc' Start 4 }
        catch { Log "WaaSMedicSvc: $($_.Exception.Message)" }
        foreach ($svc in 'DiagTrack', 'dmwappushservice', 'SysMain') {
            Set-Service $svc -StartupType Disabled -ErrorAction SilentlyContinue
            Stop-Service $svc -Force -ErrorAction SilentlyContinue
        }
        Set-Reg 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\DataCollection' AllowTelemetry 0
        Set-Reg 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\DataCollection' AllowTelemetry 0
        Set-Reg 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Schedule\Maintenance' MaintenanceDisabled 1
        # Some of these tasks belong to SYSTEM and refuse an administrator;
        # count those instead of failing the bake over them.
        $taskSkips = 0
        foreach ($tp in '\Microsoft\Windows\WindowsUpdate\', '\Microsoft\Windows\UpdateOrchestrator\',
            '\Microsoft\Windows\Maintenance\', '\Microsoft\Windows\Application Experience\',
            '\Microsoft\Windows\Customer Experience Improvement Program\', '\Microsoft\Windows\Defrag\',
            '\Microsoft\Windows\DiskCleanup\', '\Microsoft\Windows\Windows Error Reporting\') {
            foreach ($task in @(Get-ScheduledTask -TaskPath $tp -ErrorAction SilentlyContinue)) {
                try { $task | Disable-ScheduledTask -ErrorAction Stop | Out-Null } catch { $taskSkips++ }
            }
        }
        Get-ScheduledTask -TaskName ServerManager -ErrorAction SilentlyContinue | Disable-ScheduledTask | Out-Null
        Set-Reg 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System' ConsentPromptBehaviorAdmin 0
        Set-Reg 'HKLM:\SYSTEM\CurrentControlSet\Control' ServicesPipeTimeout 120000
        # What Set-ExecutionPolicy -Scope LocalMachine writes, without its
        # "overridden by a more specific scope" error under -ExecutionPolicy Bypass.
        Set-Reg 'HKLM:\SOFTWARE\Microsoft\PowerShell\1\ShellIds\Microsoft.PowerShell' ExecutionPolicy 'Unrestricted' 'String'
        New-Item -Path 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\OOBE' -Force | Out-Null
        Set-ItemProperty 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\OOBE' DisablePrivacyExperience -Value 1 -Type DWord
        Set-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\TimeZoneInformation' RealTimeIsUniversal -Value 1 -Type DWord
        Set-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' LongPathsEnabled -Value 1 -Type DWord
        powercfg /change monitor-timeout-ac 0 | Out-Null
        powercfg /change standby-timeout-ac 0 | Out-Null
        Log "policies applied ($taskSkips protected scheduled tasks left as they were)"

        # --- Defender ----------------------------------------------------------
        # The hosted image's preferences (actions/runner-images
        # Configure-WindowsDefender.ps1): Defender stays installed, scanning
        # is off and C:\ is excluded. Applied before the WinGet installs so
        # they are not scanned either. One call per preference, so a
        # parameter this build rejects is logged rather than fatal.
        if ($env_.disable_defender) {
            $avPrefs = @(
                @{DisableArchiveScanning = $true}
                @{DisableAutoExclusions = $true}
                @{DisableBehaviorMonitoring = $true}
                @{DisableCatchupFullScan = $true}
                @{DisableCatchupQuickScan = $true}
                @{DisableIntrusionPreventionSystem = $true}
                @{DisableIOAVProtection = $true}
                @{DisablePrivacyMode = $true}
                @{DisableScanningNetworkFiles = $true}
                @{DisableScriptScanning = $true}
                @{MAPSReporting = 0}
                @{PUAProtection = 0}
                @{SignatureDisableUpdateOnStartupWithoutEngine = $true}
                @{SubmitSamplesConsent = 2}
                @{ScanAvgCPULoadFactor = 5; ExclusionPath = @('C:\')}
                @{DisableRealtimeMonitoring = $true}
                @{ScanScheduleDay = 8}
                @{EnableControlledFolderAccess = 'Disabled'}
                @{EnableNetworkProtection = 'Disabled'}
                @{DisableBlockAtFirstSeen = $true}
            )
            foreach ($pref in $avPrefs) {
                try { Set-MpPreference @pref -ErrorAction Stop }
                catch { Log "Set-MpPreference $($pref.Keys -join ','): $($_.Exception.Message)" }
            }
            $rt = $true
            for ($i = 0; $i -lt 12 -and $rt; $i++) {
                $rt = (Get-MpComputerStatus).RealTimeProtectionEnabled
                if ($rt) { Start-Sleep -Seconds 5 }
            }
            if ($rt) { throw 'Defender real-time protection is still on after Set-MpPreference' }
            Log 'defender: real-time, behaviour, IOAV and script scanning off; C:\ excluded'
        } else {
            Log 'disable_defender is false; Defender left at its defaults'
        }
```

- [ ] **Step 6: Git parity**

In the Git for Windows section, replace the `Start-Process 'C:\git-installer.exe' …` line with:

```powershell
        # Options as on the hosted image: Git LFS, symlinks, ConHost bash.
        $p = Start-Process 'C:\git-installer.exe' -ArgumentList '/VERYSILENT', '/NORESTART', '/NOCANCEL', '/SP-',
            '/COMPONENTS=gitlfs', '/o:PathOption=CmdTools', '/o:BashTerminalOption=ConHost', '/o:EnableSymlinks=Enabled' -Wait -PassThru
```

After the existing `Log "git: $gitVer"` line, add:

```powershell
        # Hosted-image Git setup: bash/sh on PATH (Git\bin), every checkout
        # directory trusted, no credential-manager prompts.
        Add-MachinePath 'C:\Program Files\Git\bin'
        $null = Invoke-Native $gitExe @('config', '--system', 'safe.directory', '*')
        [Environment]::SetEnvironmentVariable('GCM_INTERACTIVE', 'Never', 'Machine')
```

- [ ] **Step 7: Split the WinGet helper**

Replace the whole `function Install-WinGetPackage([string]$id, [int]$timeoutMin = 15, [string[]]$extra = @()) { … }` definition (including its leading comment block starting `# One winget install, bounded.`) with:

```powershell
        # One winget install, bounded; returns WinGet's exit code. A hung
        # installer (the first toolchain bake sat on VCRedist.2005.x64 until
        # the host's 90-minute timeout, and the overlay was gone before it
        # could be inspected) is killed after $timeoutMin, with what it was
        # stuck on sent to the serial console: the live installer processes,
        # winget's diagnostic log and the package's own installer log.
        function Invoke-WinGetInstall([string]$id, [int]$timeoutMin = 15, [string[]]$extra = @()) {
            $safe = $id -replace '[^A-Za-z0-9.+-]', '_'
            $instLog = "C:\ghq\winget\$safe.installer.log"
            $outFile = "C:\ghq\winget\$safe.out.txt"
            $wgArgs = @('install', '--id', $id, '--exact', '--source', 'winget', '--silent',
                '--accept-package-agreements', '--accept-source-agreements', '--disable-interactivity',
                '--log', $instLog) + $extra
            # Windows PowerShell 5.1 has no ProcessStartInfo.ArgumentList, so
            # quote each argument for the MSVCRT command-line parser.
            $argLine = ($wgArgs | ForEach-Object {
                if ($_ -match '[\s"]') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ }
            }) -join ' '
            $started = Get-Date
            $p = Start-Process -FilePath $winget -ArgumentList $argLine -PassThru -NoNewWindow -RedirectStandardOutput $outFile -RedirectStandardError "$outFile.err"
            # Reading Handle while the process lives makes .NET keep it, or
            # ExitCode comes back $null after WaitForExit (the third bake
            # failed on 'rc=' with winget mid-install).
            $null = $p.Handle
            if (-not $p.WaitForExit($timeoutMin * 60 * 1000)) {
                Log "TIMEOUT: winget install $id still running after $timeoutMin min; diagnostics follow"
                Get-CimInstance Win32_Process | Where-Object { $_.CreationDate -ge $started -and $_.ProcessId -ne $PID } |
                    ForEach-Object { Log "  proc $($_.ProcessId) $($_.Name): $($_.CommandLine)" }
                $diag = Get-ChildItem $wingetDiag -Filter *.log -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending | Select-Object -First 1
                if ($diag) { Get-Content $diag.FullName -Tail 25 | ForEach-Object { Log "  winget-diag: $_" } }
                Get-ChildItem 'C:\ghq\winget' -Filter "$safe.installer*" -ErrorAction SilentlyContinue |
                    ForEach-Object { Get-Content $_.FullName -Tail 25 | ForEach-Object { Log "  installer-log: $_" } }
                & taskkill.exe /T /F /PID $p.Id 2>&1 | Out-Null
                throw "winget install $id timed out after $timeoutMin min"
            }
            # Completes async output handling after the timed wait. Its bool
            # must not join this function's return value.
            $null = $p.WaitForExit()
            $rc = $p.ExitCode
            if ($null -eq $rc) { throw "winget install ${id}: exit code unavailable" }
            $script:wingetMinutes = [math]::Round(((Get-Date) - $started).TotalMinutes, 1)
            return $rc
        }

        # Throws with the tail of WinGet's output unless $rc means installed.
        function Assert-WinGetResult([string]$id, [int]$rc) {
            if ($wingetOK -notcontains $rc) {
                $safe = $id -replace '[^A-Za-z0-9.+-]', '_'
                $outFile = "C:\ghq\winget\$safe.out.txt"
                $out = (Get-Content $outFile, "$outFile.err" -ErrorAction SilentlyContinue | Select-Object -Last 30) -join "`n"
                throw "winget install $id failed rc=$rc : $out"
            }
            Log "installed $id (rc=$rc, $script:wingetMinutes min)"
        }

        function Install-WinGetPackage([string]$id, [int]$timeoutMin = 15, [string[]]$extra = @()) {
            Assert-WinGetResult $id (Invoke-WinGetInstall $id $timeoutMin $extra)
        }
```

- [ ] **Step 8: Conditional Build Tools with the SDK, and the package loop**

Replace the block from the comment `        # MSBuild + MSVC v143 x64/x86 + Windows 11 SDK (signtool): the VCTools` through the line `        Install-WinGetPackage 'jqlang.jq' 15 @('--scope', 'machine')` (inclusive) with:

```powershell
        # MSBuild + MSVC v143 x64/x86 + the Windows 11 SDK (signtool). The
        # VCTools workload requires MSBuild and recommends the compiler and an
        # SDK; the 26100 SDK is added explicitly so signtool is guaranteed.
        # --override replaces winget's default installer switches, so the
        # silent/wait flags are repeated here.
        # https://learn.microsoft.com/visualstudio/install/workload-component-id-vs-build-tools
        $vswhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
        if ($env_.build_tools) {
            Install-WinGetPackage 'Microsoft.VisualStudio.2022.BuildTools' 60 @('--override',
                '--wait --quiet --norestart --nocache --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended --add Microsoft.VisualStudio.Component.Windows11SDK.26100')
            $vsPath = if (Test-Path $vswhere) { & $vswhere -latest -products * -requires Microsoft.Component.MSBuild Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath }
            if (-not $vsPath) { throw 'VS Build Tools installed but MSBuild/MSVC not found by vswhere' }
            Log "VS Build Tools at $vsPath"
        } else {
            Log 'build_tools is false; skipping VS Build Tools and the Windows SDK'
        }

        # windows.packages, machine-wide so the `runner` user sees them (the
        # default user scope would land in this Administrator's profile).
        # A manifest with no machine-scoped installer answers
        # APPINSTALLER_CLI_ERROR_NO_APPLICABLE_INSTALLER; install it unscoped.
        $noApplicable = -1978335216  # 0x8A150010
        $packages = @($env_.packages | Where-Object { $_ })
        foreach ($id in $packages) {
            $rc = Invoke-WinGetInstall $id 20 @('--scope', 'machine')
            if ($rc -eq $noApplicable) {
                Log "$id has no machine-scoped installer; retrying without --scope"
                $rc = Invoke-WinGetInstall $id 20
            }
            Assert-WinGetResult $id $rc
        }
        # 7-Zip's installer adds no PATH entry (the hosted image gets one
        # from a Chocolatey shim).
        if (Test-Path 'C:\Program Files\7-Zip\7z.exe') { Add-MachinePath 'C:\Program Files\7-Zip' }
```

- [ ] **Step 9: Toolchain assertion**

Immediately before `        # --- actions-runner ----------------------------------------------------`, add:

```powershell
        # --- toolchain check -----------------------------------------------------
        # What a job's fresh logon session will see: re-read PATH from the
        # registry, since this session's copy predates the installs.
        $env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
        $checks = [ordered]@{ git = '--version'; bash = '--version' }
        $pkgCommands = @{
            'Microsoft.PowerShell' = @('pwsh', '--version')
            'GitHub.cli'           = @('gh', '--version')
            'jqlang.jq'            = @('jq', '--version')
            '7zip.7zip'            = @('7z', 'i')
        }
        foreach ($id in $packages) { if ($pkgCommands.ContainsKey($id)) { $checks[$pkgCommands[$id][0]] = $pkgCommands[$id][1] } }
        $summary = @()
        foreach ($name in @($checks.Keys)) {
            $cmd = Get-Command $name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
            if (-not $cmd) { throw "toolchain check: $name is not on the machine PATH" }
            $ver = ((Invoke-Native $cmd.Source @($checks[$name])) -split "`n")[0].Trim()
            $summary += "$name=$ver"
        }
        if ($env_.build_tools) {
            $vc = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
            if (-not $vc) { throw 'toolchain check: vswhere finds no VC.Tools.x86.x64 installation' }
            $signtool = Get-ChildItem "${env:ProgramFiles(x86)}\Windows Kits\10\bin" -Filter signtool.exe -Recurse -ErrorAction SilentlyContinue |
                Where-Object { $_.FullName -match '\\x64\\signtool\.exe$' } | Select-Object -First 1
            if (-not $signtool) { throw 'toolchain check: signtool.exe (Windows SDK, x64) not found' }
            $summary += "msvc=$vc"
            $summary += "sdk=$(Split-Path (Split-Path (Split-Path $signtool.FullName)) -Leaf)"
        }
        if ($env_.disable_defender) {
            if ((Get-MpComputerStatus).RealTimeProtectionEnabled) { throw 'toolchain check: Defender real-time protection is on' }
            $summary += 'defender_rt=off'
        }
        Log "toolchain: $($summary -join ' ')"
```

- [ ] **Step 10: Run the tests**

Run: `go test ./scripts/ -run TestWindowsAssetsEmbedded -v`
Expected: PASS.

Then verify encoding and the whole suite:

```bash
grep -c $'\r' scripts/guest/windows/bake.ps1           # expect 0
LC_ALL=C grep -c '[^ -~	]' scripts/guest/windows/bake.ps1  # expect 0 (ASCII only)
go test ./... && mise exec -- golangci-lint run
```

- [ ] **Step 11: Commit**

```bash
git add scripts/guest/windows/bake.ps1 scripts/embed_test.go
git commit -m "feat(windows): bake a GitHub-hosted-parity toolchain and posture

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Documentation

**Files:**
- Modify: `README.md`, `packaging/config.example.yaml`, `docs/superpowers/specs/2026-09-23-windows-runner-design.md`

**Interfaces:**
- Consumes: key names and defaults from Task 1; behaviour from Task 3.

- [ ] **Step 1: README — Windows pools YAML block**

In the `## Windows pools` YAML example, append inside the `windows:` block after the `ovmf_dir` line:

```yaml
  # packages: [Microsoft.PowerShell, GitHub.cli, jqlang.jq, 7zip.7zip]   # WinGet IDs; [] installs none
  # build_tools: true         # VS 2022 Build Tools (C++) + Windows 11 SDK (signtool)
  # disable_defender: true    # hosted-image Defender posture; false leaves Defender on
```

- [ ] **Step 2: README — replace the "Inside the guest" paragraph**

Replace the paragraph that starts `Inside the guest, jobs run as the local administrator` with:

```markdown
Inside the guest, jobs run as the local administrator `runner` in an
interactive session, on an image built to behave like GitHub's
`windows-2025` hosted image where CI depends on it:

- **On the machine PATH:** Git (with Git LFS, symlinks enabled,
  `safe.directory *`) and its `bash`, PowerShell 7 (`pwsh`, so
  `shell: pwsh` and the default Windows shell work), `gh`, `jq`, and
  `7z` — the `windows.packages` default list, installed with WinGet at
  bake time. Add any WinGet package ID (`winget search <name>` finds
  them) or trim the list; `[]` installs none.
- **C++ toolchain:** Visual Studio 2022 Build Tools with the C++ workload
  (MSVC, MSBuild) and the Windows 11 SDK, so `signtool`, `link.exe` and
  Rust's `x86_64-pc-windows-msvc` target work
  (`windows.build_tools: false` skips them), plus the VC++ 2005–2015+
  runtimes.
- **Posture like the hosted image:** Windows Update, telemetry, SysMain
  and background maintenance off; no UAC prompt; long paths on;
  Microsoft Defender installed but with real-time, behaviour, script and
  download scanning off and `C:\` excluded
  (`windows.disable_defender: false` keeps Defender's defaults).

Tool versions float: each bake installs the current WinGet release, as the
hosted image's weekly refresh does, and `base-windows.json` records the
bake's toolchain summary. What the hosted image has and this one
deliberately lacks: Visual Studio Enterprise, the pre-populated tool
cache for `actions/setup-*`, Docker, browsers and WebDrivers, Android
SDK, databases. The bake checks its own toolchain before publishing and
fails rather than ship an image missing a configured tool.

A bake with the default options takes 25–45 minutes (Build Tools
dominates) and needs outbound HTTPS to the WinGet source and Microsoft's
Visual Studio CDN.
```

- [ ] **Step 3: README — footprint numbers**

In the "Disk footprint in `paths.images`" paragraph, change `about 24 GB steady state` to `about 30 GB steady state`, `~12 GB baked` to `~18 GB baked`, `peaking near 40 GB` to `peaking near 50 GB`, `Budget 40 GB` to `Budget 50 GB`, `roughly 12 GB steady state and 24 GB during a bake` to `roughly 18 GB steady state and 36 GB during a bake`.

- [ ] **Step 4: README — Top level configuration table**

Add after the `windows.ovmf_dir` row:

```markdown
| `windows.packages` | no | `[Microsoft.PowerShell, GitHub.cli, jqlang.jq, 7zip.7zip]` | WinGet package IDs installed machine-wide in the Windows image; `[]` installs none — see "Windows pools" |
| `windows.build_tools` | no | `true` | Bake VS 2022 Build Tools (C++ workload) and the Windows 11 SDK |
| `windows.disable_defender` | no | `true` | Apply the GitHub-hosted image's Defender posture (scanning off, `C:\` excluded); `false` leaves Defender's defaults |
```

- [ ] **Step 5: Example config**

In `packaging/config.example.yaml`, inside the commented `#windows:` block, add after its `ovmf_dir` line:

```yaml
#  packages: [Microsoft.PowerShell, GitHub.cli, jqlang.jq, 7zip.7zip]  # WinGet IDs baked in; [] for none
#  build_tools: true         # VS 2022 Build Tools (C++) + Windows 11 SDK
#  disable_defender: true    # GitHub-hosted-image Defender posture
```

- [ ] **Step 6: Parent spec pointer**

Append to the end of `docs/superpowers/specs/2026-09-23-windows-runner-design.md`:

```markdown

## Amendment 2026-09-26: hosted-image parity

The baked toolchain, the `windows.packages` / `build_tools` /
`disable_defender` keys, and the Defender and Windows Update posture are
specified in
[2026-09-26-windows-hosted-parity-design.md](2026-09-26-windows-hosted-parity-design.md).
```

- [ ] **Step 7: Verify and commit**

```bash
grep -n 'windows.packages\|build_tools\|disable_defender' README.md packaging/config.example.yaml | head
go test ./...
git add README.md packaging/config.example.yaml docs/superpowers/specs/2026-09-23-windows-runner-design.md
git commit -m "docs: document the hosted-parity Windows image and its options

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Manual verification (controller)

**Files:** none unless a fix is needed (then a `fix(windows): …` commit with its cause in the body).

- [ ] **Step 1: Bake on the dev host.** Build the binary, point a Windows-only config at a state directory whose `images/` already holds the VHDX and ISO with their sidecars, run `refresh-image`, and watch `images/bake-windows/console.log` for the `[bake …]` lines. Expect `BAKE-OK` within 60 minutes, a `toolchain:` line listing `git`, `bash`, `pwsh`, `gh`, `jq`, `7z`, `msvc=…`, `sdk=10.0.26100.0`, `defender_rt=off`, and `base-windows.json` carrying `packages`, `build_tools`, `disable_defender`, `toolchain`.
- [ ] **Step 2: Clone boot.** Boot a clone of the new base from virtio-blk with a fake JIT seed (as in the first Windows plan's Task 11) and confirm `run-one-job` still starts as `runner` and powers off.
- [ ] **Step 3: Runner host.** Push the branch, install the branch build on the runner host alongside the packaged binary, run `refresh-image` with the Windows-only bake config, restart the service (drains busy Linux runners), and confirm the `win` pool's runner comes online.
- [ ] **Step 4: Real workloads.** Re-run a real workflow whose Windows jobs failed on a missing toolchain and confirm they now pass and complete; run a parity smoke workflow and confirm `pwsh on PATH: True` and `RealTimeProtectionEnabled : False`.
- [ ] **Step 5: Record** bake time, image size, and the toolchain line in the PR description.
