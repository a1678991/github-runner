# Windows image: GitHub-hosted parity — Design

**Date:** 2026-09-26
**Status:** Approved (brainstorming phase)
**Extends:** [2026-09-23-windows-runner-design.md](2026-09-23-windows-runner-design.md)

## What this is

The Windows base image baked by `refresh-image` should behave, from a
workflow's point of view, like GitHub's `windows-2025` hosted image where
that matters for CI: the same default shell, the same everyday CLI tools,
a C++/MSVC toolchain and Windows SDK for native and Rust builds, Git
configured the same way, Windows Update and Defender treated the same
way, and winget available for anything else. Differences from the hosted
image stay deliberate and documented.

The gaps this closes were measured on a real workload (a Go and Rust
client build on the first Windows pool): `pwsh` absent, `bash`/`jq`/`gh`
absent, no MSVC linker, no `signtool`, Defender real-time scanning
slowing every toolchain extraction.

Key decisions (settled during brainstorming, from the investigation of
`actions/runner-images`):

| Decision | Choice |
|---|---|
| Package manager | winget (ships with Windows Server 2025; verified working for the `runner` user). The hosted image uses Chocolatey + vendor installers; we do not install Chocolatey |
| Default tool set | PowerShell 7, GitHub CLI, jq, 7-Zip via winget; Git for Windows reconfigured for parity (bash on PATH, LFS, symlinks, `safe.directory *`) |
| MSVC + Windows SDK | **In the default image**: Visual Studio 2022 Build Tools (`Microsoft.VisualStudio.2022.BuildTools` via WinGet, as the branch already does; the manifest tracks the current 17.14 channel) with the `VCTools` workload plus recommended components, and the Windows 11 SDK 10.0.26100 component added explicitly so `signtool` is guaranteed rather than implied |
| Defender | **Disabled by default** exactly as the hosted image does (`Set-MpPreference` set from `Configure-WindowsDefender.ps1`), not uninstalled; opt-out knob |
| System tuning | The subset of the hosted `Configure-System.ps1` that affects CI: Windows Update off by policy and service, telemetry off, SysMain and maintenance/update scheduled tasks off, UAC consent prompt off, `Git\bin` on PATH, execution policy Unrestricted |
| Verification | The bake asserts the toolchain it installed (pwsh, bash, jq, gh, `vswhere` finds `VC.Tools.x86.x64`, `signtool.exe` under `Windows Kits\10`) and fails with `BAKE-FAILED` otherwise |
| Config surface | `windows.packages` (winget IDs, default = the parity list), `windows.build_tools` (bool, default true), `windows.disable_defender` (bool, default true) |

## Starting point

The branch already bakes part of the toolchain (commits `7c1de1c`,
`14508f6`, `f73c153`, `a99669f`): it bootstraps a current WinGet client
for the first-logon session (`Repair-WinGetPackageManager`, with the
#4603 workaround), installs through a bounded `Install-WinGetPackage`
helper (per-package timeout, hang diagnostics to the serial console,
accepted exit codes), and installs the VC++ 2015+ runtimes, VS 2022
Build Tools (`VCTools --includeRecommended`), `GitHub.cli`, `jqlang.jq`,
and the 2005–2013 VC++ runtimes; the bake timeout is already 90
minutes. This design keeps all of that and changes only what is listed
below.

## Non-goals

- A tool cache for `actions/setup-*` (Go/Node/Python/Ruby under
  `RUNNER_TOOL_CACHE`). Valuable, separate spec.
- Full Visual Studio Enterprise or the hosted image's 60+ workloads;
  Docker; browsers and WebDrivers; Android SDK; databases; MSYS2.
- Chocolatey.
- Product-key conversion of the evaluation edition (separate follow-up).
- Any change to Linux images or the docker backend.

## Configuration

```yaml
windows:
  # winget package IDs installed machine-wide at bake time. The default
  # list gives GitHub-hosted parity for everyday tools; set an explicit
  # list to trim or extend it (an empty list installs nothing).
  packages:
    - Microsoft.PowerShell
    - GitHub.cli
    - jqlang.jq
    - 7zip.7zip
  build_tools: true        # VS 2022 Build Tools (VCTools workload) + Windows 11 SDK 26100
  disable_defender: true   # hosted-image Defender preferences; false leaves Defender at defaults
```

Rules in `internal/config`:

- `packages` entries must match `^[A-Za-z0-9][A-Za-z0-9.+_-]*$` (winget
  identifier characters) and be unique. `null`/absent means the default
  list; `[]` means none.
- `build_tools` and `disable_defender` are `*bool` so absence defaults to
  true (same pattern as `images.auto_refresh`).

`bake-env.json` carries these to the guest: `packages` (array),
`build_tools` (bool), `disable_defender` (bool), alongside the existing
runner/git fields.

## Bake changes (`scripts/guest/windows/bake.ps1`)

Order after the existing driver/network/user steps, before the runner
install:

1. **Git for Windows**: installer options become
   `/o:PathOption=CmdTools /o:BashTerminalOption=ConHost /o:EnableSymlinks=Enabled /COMPONENTS=gitlfs`;
   add `C:\Program Files\Git\bin` to the machine `PATH`; run
   `git config --system safe.directory *`; set machine env
   `GCM_INTERACTIVE=Never`.
2. **winget packages**: the hardcoded `GitHub.cli` / `jqlang.jq` lines
   are replaced by a loop over `packages`, each installed through the
   existing bounded helper with `--scope machine` (20-minute cap). When
   WinGet answers `-1978335216` (`APPINSTALLER_CLI_ERROR_NO_APPLICABLE_INSTALLER`
   — a manifest with no machine-scoped installer), the package is retried
   once without `--scope`. The helper's existing accepted-exit-code set
   applies; anything else fails the bake with the captured output.
   `--scope machine` keeps portable packages
   (`jq`) out of the Administrator profile and on the machine PATH
   (`%ProgramFiles%\WinGet\Links`). 7-Zip's installer adds no PATH entry
   (the hosted image gets one from Chocolatey shims), so the bake appends
   `C:\Program Files\7-Zip` to the machine `PATH` after the package step.
3. **Build Tools + SDK** (when `build_tools`): the existing WinGet install
   of `Microsoft.VisualStudio.2022.BuildTools` becomes conditional, and its
   `--override` gains `--add Microsoft.VisualStudio.Component.Windows11SDK.26100`
   (`--wait --quiet --norestart --nocache --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended --add Microsoft.VisualStudio.Component.Windows11SDK.26100`).
   The SDK component installs `Windows Kits\10` including `signtool.exe`,
   so no separate SDK installer is needed. The VC++ runtimes stay
   unconditional: they are runtimes jobs load, not build tools.
4. **Defender** (when `disable_defender`; runs *before* the WinGet
   installs so it also speeds up the bake itself): apply the hosted image's
   `Set-MpPreference` set verbatim: `DisableArchiveScanning`,
   `DisableAutoExclusions`, `DisableBehaviorMonitoring`,
   `DisableCatchupFullScan`, `DisableCatchupQuickScan`,
   `DisableIntrusionPreventionSystem`, `DisableIOAVProtection`,
   `DisablePrivacyMode`, `DisableScanningNetworkFiles`,
   `DisableScriptScanning` (all `$true`), `MAPSReporting 0`,
   `PUAProtection 0`, `SignatureDisableUpdateOnStartupWithoutEngine $true`,
   `SubmitSamplesConsent 2`, `ScanAvgCPULoadFactor 5`,
   `ExclusionPath C:\`, `DisableRealtimeMonitoring $true`,
   `ScanScheduleDay 8`, `EnableControlledFolderAccess Disable`,
   `EnableNetworkProtection Disabled`, `DisableBlockAtFirstSeen $true`;
   each in its own `Set-MpPreference` call so one unsupported parameter
   does not abort the rest (log and continue). Defender stays
   installed; `Get-MpComputerStatus` must then report
   `RealTimeProtectionEnabled = False`, asserted by the bake.
5. **System tuning** (always): Windows Update policies
   (`AU\NoAutoUpdate=1`, `AU\AUOptions=1`,
   `DoNotConnectToWindowsUpdateInternetLocations=1`,
   `DisableWindowsUpdateAccess=1`), `WaaSMedicSvc` and `wuauserv`
   disabled (the latter already is), `DiagTrack`, `dmwappushservice`,
   `SysMain` disabled, `AllowTelemetry=0` under both policy paths,
   `MaintenanceDisabled=1`, scheduled tasks under
   `\Microsoft\Windows\{WindowsUpdate,UpdateOrchestrator,Maintenance,Application Experience,Customer Experience Improvement Program,Defrag,DiskCleanup,Windows Error Reporting}` disabled,
   `ConsentPromptBehaviorAdmin=0`, `Set-ExecutionPolicy Unrestricted -Scope LocalMachine`,
   `ServicesPipeTimeout=120000`. `LongPathsEnabled` and
   `DisablePrivacyExperience` remain from the current bake.
6. **Toolchain assertion** (before `BAKE-OK`): fail the bake unless all
   of these hold — `pwsh`, `bash`, `jq`, `gh`, `git`, `7z` resolve via
   `Get-Command` in a *fresh* environment (re-read machine+user PATH from
   the registry first, since the bake session's PATH is stale);
   `vswhere.exe -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath`
   returns a path and `signtool.exe` exists under
   `${env:ProgramFiles(x86)}\Windows Kits\10\bin\*\x64\` (both only when
   `build_tools`); `RealTimeProtectionEnabled` is false (only when
   `disable_defender`). Log each check's result to the serial console.

The bake log line `[bake] toolchain: pwsh=<ver> git=<ver> gh=<ver> jq=<ver> msvc=<path> sdk=<ver>` gives the host a one-line summary that `refresh-image` prints.

## Host-side changes

- `internal/imagebake/windows.go`: `WindowsOptions` gains `Packages []string`,
  `BuildTools bool`, `DisableDefender bool`; they land in
  `bake-env.json` (always a JSON array for `packages`, never `null`); the
  bake timeout stays at the branch's 90 minutes;
  `base-windows.json` records `packages`, `build_tools`,
  `disable_defender` and the toolchain summary line parsed from the
  console.
- `internal/imageprep`: passes the three new options from config.
- Bake VM disk: the VHDX's 64 GiB virtual size is enough (Build Tools +
  SDK ≈ 6 GB). Flattened image grows from ~12 GB to roughly 18–20 GB;
  README footprint numbers updated (≈ 32 GB steady state, ≈ 50 GB peak).
- `packaging/github-qemu-runner-refresh.service` / Nix: `TimeoutStartSec`
  stays 3h.

## Documentation

- README Windows pools section: what the image contains (tool list with
  the note "same tools, versions float with winget"), what it deliberately
  lacks versus the hosted image (VS Enterprise, tool cache, Docker,
  browsers), Defender and Windows Update posture, the three new keys,
  bake time and footprint, and how to add tools (`packages:` plus the
  winget ID lookup command `winget search`).
- Example config gains the three keys, commented.
- Spec amendment table in the parent design's "Configuration" section
  pointing here.

## Testing

Unit (CI): config parsing/validation for the three keys (default list,
empty list, invalid ID, duplicates, `*bool` defaults); `bake-env.json`
contents in the existing `TestBakeSeedContents`; `base-windows.json`
carries the new fields; embed shape tests for the new sentinels
(`winget install`, `vs_BuildTools.exe`, `Set-MpPreference`,
`Workload.VCTools`, `Windows11SDK.26100`, the toolchain assertion
strings).

Manual, recorded in the plan: a bake on the dev host (time, image
size, toolchain summary line); a clone boot; then on the runner host a
rebake, a restart, and a real workload's Windows jobs passing their
toolchain check and completing.

## Risks and mitigations

- **Bake time under load.** The pre-toolchain image baked in
  minutes on a busy runner host; Build Tools and the runtimes add 15–30
  minutes. The 90-minute guest timeout and 3-hour unit timeout cover it;
  the README says so.
- **Defender preferences reverting.** On Windows Server, `Set-MpPreference`
  persists across reboots when tamper protection is off (the default
  without Defender for Endpoint onboarding). The bake asserts the state
  after applying it; the smoke workflow re-checks it in a clone.
- **winget first-run prompts.** Every call passes
  `--disable-interactivity` and the agreement flags with stdin from
  `NUL`; a hang is bounded by the per-package cap.
- **Exit 3010 from Build Tools.** Pending reboot operations complete on
  the clone's first boot; measured cost is seconds. If a clone's first
  boot ever shows the installer resuming, add a reboot inside the bake.
- **Tool versions float.** winget installs the latest at bake time, like
  the hosted image's weekly refresh; `base-windows.json` records what was
  installed.
