# Windows job VMs on the qemu backend — Design

**Date:** 2026-09-23
**Status:** Approved (brainstorming phase; validated by spike on 2026-09-23)
**Extends:** [2026-06-10-qemu-runner-design.md](2026-06-10-qemu-runner-design.md)

## What this is

`backend: qemu` pools gain `os: windows`. A second baked image,
`base-windows.qcow2`, is built from the Windows Server evaluation VHDX by
booting it once under QEMU and running a built-in PowerShell bake script.
Windows job VMs then follow the Linux lifecycle exactly: clone the base,
hand the JIT config over on a seed ISO, run one job, power off, tear
down. The pool loop, liveness gate, drain, and orphan reaping are
untouched.

Every mechanism below was exercised on a KVM host against the real
Server 2025 evaluation image before this spec was written (see
[Spike findings](#spike-findings)); the design records what worked, not
what the documentation says should work.

Key decisions (settled during brainstorming):

| Decision | Choice |
|---|---|
| Base image | Windows Server 2025 evaluation VHDX by default; `windows.image_url` overrides (e.g. Server 2022) |
| Bake customisation | Built-in `bake.ps1` only, mirroring Linux `bake.sh`; no operator hook script |
| Runner session | Interactive: local admin `runner` autologs on, a logon-triggered scheduled task runs the job (GitHub-hosted parity) |
| Virtual hardware | Full virtio for job VMs (virtio-blk boot disk, virtio-net); drivers staged at bake |
| Firmware | UEFI via OVMF (the VHDX is GPT / Hyper-V Gen 2); fresh per-VM NVRAM copy |
| OOBE automation | `Unattend.xml` on the seed CD-ROM (bake boot only) |
| Docker inside Windows jobs | Not supported |
| Windows Update at bake | Not run; `wuauserv` disabled in the image so jobs are never interrupted |
| Guest tooling installed | virtio drivers, Git for Windows, actions-runner (win-x64) |

## Non-goals

- Docker or Windows containers inside Windows jobs.
- arm64 Windows.
- Windows Update, Chocolatey, Visual Studio, or any tooling beyond Git
  at bake time. Operators who need more bake their own image later; the
  hook-script question was considered and declined for this version.
- The docker backend is unchanged.
- Evaluation licensing terms are the operator's concern. The README says
  so plainly: the eval expires after 180 days and is not a production
  licence; a weekly rebake starts from the pristine VHDX each time.

## Configuration

```yaml
windows:                       # optional block; every key optional
  image_url: https://go.microsoft.com/fwlink/?linkid=2345826   # Server 2025 eval VHDX
  image_sha256: ""             # verified when set; Microsoft publishes no checksum file
  virtio_win_url: https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.302-1/virtio-win-0.1.302.iso
  virtio_win_sha256: ""
  ovmf_dir: ""                 # directory holding OVMF_CODE*.fd + OVMF_VARS*.fd; auto-detected when empty

pools:
  - name: win
    os: windows                # linux (default) | windows; qemu backend only
    scope: org
    org: my-org
    count: 1
    cpus: 4
    memory_mb: 8192            # windows pools: >= 2048
    disk_gb: 80                # floor is the VHDX virtual size (64 GiB for Server 2025)
    labels: [self-hosted, windows, x64]
```

Validation rules added to `internal/config`:

- `os` must be `linux` or `windows`; empty defaults to `linux`.
- `os: windows` with `backend: docker` is rejected.
- Windows pools require `memory_mb >= 2048`.
- `windows.ovmf_dir`, when set, must be absolute.
- `windows.*_sha256`, when set, must be 64 hex characters.

Helpers: `Config.HasQEMUOS(os string) bool`, alongside `HasBackend` and
`HasDockerIsolation`, so imageprep, controller, and setup gate on it.

The `virtio_win_url` default pins a versioned **https** archive URL. The
unversioned `stable-virtio/virtio-win.iso` path redirects through plain
`http://`, which would silently drop TLS for an unverified download.

### OVMF discovery

When `windows.ovmf_dir` is empty, the first existing directory wins:

| Distro | Directory | Files |
|---|---|---|
| Arch | `/usr/share/edk2/x64` | `OVMF_CODE.4m.fd`, `OVMF_VARS.4m.fd` |
| Debian/Ubuntu | `/usr/share/OVMF` | `OVMF_CODE_4M.fd`, `OVMF_VARS_4M.fd` |
| Arch (older layout) | `/usr/share/edk2-ovmf/x64` | as above |
| NixOS | set by the module from `pkgs.OVMF.fd` (`/share/edk2/x64` or `FV`) | |

Within the directory, the code file is the first match of
`OVMF_CODE.4m.fd`, `OVMF_CODE_4M.fd`, `OVMF_CODE.fd`, and the vars file
likewise with `VARS`. Secure-Boot variants (`*.secboot.*`, `*.ms.fd`) are
never picked automatically: Windows Server does not need Secure Boot and
the `.ms` variants require a matching enrolled VARS.

## Host prerequisites (Windows pools)

Everything the Linux qemu backend needs, plus OVMF firmware. `qemu-img`
reads VHDX natively and `genisoimage` builds the seed ISO as today, so no
new tooling beyond the firmware package:

- Arch: `edk2-ovmf` (added to the PKGBUILD `optdepends`)
- Debian/Ubuntu: `ovmf` (documented; the deb keeps its no-Depends policy)
- NixOS: the module adds `pkgs.OVMF.fd` and sets `windows.ovmf_dir`

`setup` gains, when a Windows pool exists: OVMF code and vars files found
(`FAIL` otherwise), and a `note` when `base-windows.qcow2` is missing.

## Images directory layout

| File | Purpose |
|---|---|
| `windows-base.vhdx` | Downloaded evaluation image; cached across bakes |
| `windows-base.vhdx.meta` | ETag / Last-Modified / Content-Length sidecar for the conditional GET |
| `virtio-win.iso` | Downloaded driver ISO |
| `bake-windows/` | Bake working directory, removed after the bake |
| `base-windows.qcow2` | The baked base image (flattened, no backing file) |
| `base-windows.json` | Provenance: runner version, git version, image ETag, bake time |

### Conditional download

Microsoft ships no checksum file for the evaluation image, and the file
is 11 GB. `DownloadConditional` sends `If-None-Match` / `If-Modified-Since`
from the sidecar and treats `304 Not Modified` as a cache hit; any `200`
replaces the file and rewrites the sidecar. When `image_sha256` is set,
the existing `DownloadVerified` semantics apply on top (hash match skips
the request entirely; mismatch after download is an error). The virtio
ISO uses the same helper.

## Bake flow (`internal/imagebake/windows.go`)

1. **Download** the VHDX and the virtio-win ISO as above.
2. **Resolve releases:** actions-runner `win-x64` zip via the existing
   `LatestRunner`, which gains a `platform` parameter (`linux` | `win`);
   the release-notes marker is `<!-- BEGIN SHA win-x64 -->`. Git for
   Windows via a new `LatestGitForWindows` that picks
   `Git-<ver>-64-bit.exe` from `git-for-windows/git` and scrapes its
   SHA-256 from the release notes table; empty SHA falls back to TLS-only
   with a warning, as for the runner tarball today.
3. **Overlay** `qemu-img create -f qcow2 -b windows-base.vhdx -F vhdx`
   (`CreateOverlay` gains a backing-format parameter). No 11 GB
   conversion step.
4. **Seed ISO** with volume label `GHQSEED` carrying `Unattend.xml`,
   `bake.ps1`, `run-one-job.ps1`, and `bake-env.json` (resolved URLs,
   hashes, versions). Built by the new `seed.BuildISOFiles(ctx, dir,
   volid, files)`; the cloud-init `BuildISO` becomes a thin wrapper.
5. **Answer file** (`scripts/guest/windows/Unattend.xml`, a Go
   `text/template`): specialize pass sets `ComputerName` `ghq-bake` and
   `TimeZone` `UTC`; oobeSystem pass sets en-US locale, hides EULA,
   privacy, local-account, OEM and wireless pages, sets a per-bake random
   Administrator password (32 chars from `crypto/rand`), autologs on
   Administrator once, and its single `FirstLogonCommands` entry locates
   the `GHQSEED` volume by label and runs `bake.ps1` with
   `-ExecutionPolicy Bypass`. The password is generated per bake, exists
   only in the seed ISO and the bake VM, and is never logged.
6. **Bake VM hardware** (`qemu.Spec` with the new fields):
   - OVMF code (read-only pflash) and a fresh copy of OVMF vars in the
     bake dir;
   - boot overlay on **AHCI** (`ide-hd`, `bootindex=0`): the controller
     the VHDX already boots from;
   - a 1 GiB raw **dummy virtio-blk** disk so `viostor` binds to real
     hardware and is registered boot-start;
   - **virtio-net**;
   - virtio-win ISO and seed ISO as SATA CD-ROMs (`ide-cd`);
   - Hyper-V enlightenments `hv_relaxed,hv_vapic,hv_spinlocks=0x1fff,hv_time`;
   - serial to `console.log`, QMP socket, PID file;
   - **reboots allowed** (no `-no-reboot`): OOBE reboots once between
     the specialize and oobeSystem passes.
7. **`bake.ps1`** (runs as Administrator; transcript mirrored to COM1;
   any error prints `BAKE-FAILED: <message>` and shuts down):
   1. Locate the virtio-win volume; pick the driver folder `2k25`, falling
      back to `2k22`; `pnputil /add-driver <inf> /install` for `viostor`,
      `NetKVM`, `vioscsi`, `Balloon`, `viorng`, `vioserial`, `pvpanic`,
      `qemufwcfg`. Assert `viostor` start mode is `Boot` and a
      `10.0.2.0/24` address appears within 3 minutes (fails the bake
      otherwise).
   2. Create local admin `runner` with a random password generated inside
      the guest (never leaves the VM); set Winlogon autologon to `runner`;
      remove `AutoLogonCount`.
   3. Policies and services: `wuauserv` disabled; Server Manager logon
      task disabled; `HKLM\SOFTWARE\Policies\Microsoft\Windows\OOBE
      DisablePrivacyExperience=1` (suppresses the per-user
      "Send diagnostic data" page); `RealTimeIsUniversal=1`; long paths
      enabled; monitor/standby timeouts off.
   4. Git for Windows: download, verify SHA when known, install with
      `/VERYSILENT /NORESTART /NOCANCEL /SP- /COMPONENTS=""` and
      `/o:PathOption=CmdTools`; assert `git --version` works.
   5. actions-runner: download the zip, verify SHA when known, extract to
      `C:\actions-runner`.
   6. Copy `run-one-job.ps1` to `C:\ghq\`; register scheduled task
      `ghq-run-one-job`: trigger at logon of `runner`, principal `runner`
      interactive with `RunLevel Highest`, execution time limit 3 days,
      battery settings irrelevant but set permissive.
   7. `slmgr /ato` best-effort (logged, not fatal), log licence status.
   8. Print `BAKE-OK`, `Stop-Computer -Force`.
8. **Host side:** wait for QEMU to exit (30 minute timeout, as today);
   require `BAKE-OK` in `console.log`, else fail with the console tail;
   `qemu-img convert -O qcow2` the overlay to `base-windows.qcow2.new`;
   atomic rename; write `base-windows.json`.

## Job VM flow

`controller.QEMUProvisioner` gains `WindowsBasePath` and `OVMF` and
branches on `pool.OS`:

- overlay on `base-windows.qcow2` (`-F qcow2`), grown to `disk_gb`;
- fresh copy of the pristine OVMF vars file into the workdir;
- seed ISO (label `GHQSEED`) holding only `runner-jit.conf` (mode 0600,
  as the Linux seed);
- `qemu.Spec`: virtio-blk boot disk (`bootindex=0`), virtio-net, seed as
  SATA CD-ROM, OVMF, Hyper-V enlightenments, `-no-reboot`, serial,
  QMP, PID file.

The Linux argv is byte-for-byte unchanged; the existing golden tests in
`internal/qemu` guard that.

**`run-one-job.ps1`** (scheduled task at `runner` logon; transcript to
COM1; `Stop-Computer -Force` in `finally`):

1. Extend `C:` to `Get-PartitionSupportedSize` when more than 100 MiB is
   unused (Windows does not auto-grow).
2. Locate the `GHQSEED` volume; read `runner-jit.conf`. Missing file:
   log `run-one-job: jit config missing` and shut down (slot recycles
   through the liveness gate, as on Linux).
3. `& C:\actions-runner\run.cmd --jitconfig <blob>`; wait for exit.

Pool behaviour is unchanged: the liveness gate polls GitHub until the
runner is `online` (Windows reaches the task in roughly 16 seconds after
power-on, well inside the 5 minute default), `drain` uses QMP ACPI
power-off, which Windows honours from the interactive session, and
`ReapOrphans` treats Windows workdirs like any other.

## Code changes by package

| Package | Change |
|---|---|
| `internal/config` | `Pool.OS`, `Windows` block, validation, `HasQEMUOS`, OVMF discovery (`ResolveOVMF`) |
| `internal/qemu` | `Spec` gains `Firmware{Code,Vars}`, `DiskBus` (`virtio` \| `ahci`), `NIC` (`virtio` \| `e1000`), `HyperV`, `AllowReboot`, `ExtraDisks []Disk`, `CDROMs []string`; `Args` renders them; `CreateOverlay(ctx, base, backingFormat, dest, diskGB)`; `CopyFile` for vars |
| `internal/seed` | `BuildISOFiles`; `BuildISO` wraps it |
| `internal/imagebake` | `LatestRunner(…, platform, arch)`, `LatestGitForWindows`, `DownloadConditional`, `WindowsOptions`, `BakeWindows`, `RenderUnattend` |
| `internal/imageprep` | plan gains artifact `windows` (`base-windows.qcow2`); `Ensure` calls `BakeWindows` when selected; OVMF resolved once and passed through |
| `internal/controller` | provisioner branch; startup check for `base-windows.qcow2` and OVMF when a Windows pool exists |
| `cmd/github-qemu-runner` | `setup`: OVMF check, Windows base image note; usage text |
| `scripts` | embed `guest/windows/Unattend.xml`, `bake.ps1`, `run-one-job.ps1` |
| `packaging` | `config.example.yaml` Windows pool + `windows:` block; PKGBUILD `optdepends+=('edk2-ovmf: Windows pools')`; README sections |
| `nix` | module option `windows.enable` (adds `pkgs.OVMF.fd` to the closure and sets `settings.windows.ovmf_dir`); package unchanged |

Lint coverage: PowerShell files are embedded like the shell scripts.
PSScriptAnalyzer needs `pwsh`, which is not in the mise toolchain and is
not added; the guest scripts get shape tests only (embedded, non-empty,
contain their sentinels, LF line endings; `.editorconfig` already pins
`end_of_line = lf` for every file, which PowerShell 5.1 accepts).

## Error handling

- **Missing OVMF:** `setup` fails; the controller refuses to start a
  Windows pool with a clear message naming `windows.ovmf_dir`.
- **Bake without `BAKE-OK`:** error with the last 2 KiB of the serial
  console, same as Linux; `BAKE-FAILED` lines carry the PowerShell error
  and position.
- **Bake OOBE never reaching the script:** the 30 minute timeout kills
  the VM and reports the console tail (which will show only firmware
  lines, the tell-tale for an answer-file problem).
- **Download failures:** as today; a stale cached VHDX is still usable
  when the conditional request fails with a network error (logged), so a
  Microsoft outage does not block a rebake.
- **Job VM without a JIT file / runner never online:** liveness gate
  tears the slot down; console tail is logged.

## Testing

Unit (all run in CI without KVM):

- config: `os` defaults, docker+windows rejected, memory floor,
  `HasQEMUOS`, OVMF resolution against a temp dir layout, sha256 format.
- qemu: Windows job argv golden; Windows bake argv golden (AHCI boot,
  dummy virtio disk, two CD-ROMs, no `-no-reboot`); Linux argv unchanged;
  `CreateOverlay` backing-format plumbing with a fake `qemu-img` on PATH
  (existing pattern).
- seed: `BuildISOFiles` produces an ISO containing the given files with
  the given label (skips when `genisoimage` is absent, existing pattern).
- imagebake: `LatestRunner("win")` and `LatestGitForWindows` against
  httptest fixtures; `DownloadConditional` 200-then-304 sequence and
  sidecar contents; `RenderUnattend` yields well-formed XML with the
  password substituted and no template residue; `BakeWindows` fails fast
  on a console without `BAKE-OK` using a stub `qemu-system-x86_64`.
- imageprep: plan selects `windows` only for Windows pools and only when
  missing (or forced).
- controller: provisioner picks the Windows base and seed for Windows
  pools with a stub QEMU binary.
- scripts: embedded PowerShell files non-empty, LF endings, sentinels
  present.

Manual checkpoints recorded in the implementation plan:

1. `refresh-image` on a KVM host with a Windows pool configured produces
   `base-windows.qcow2` in under 15 minutes.
2. A real job (`runs-on: [self-hosted, windows, x64]`) with
   `actions/checkout` and a `git --version` step succeeds, and the VM
   exits afterwards.
3. `systemctl stop` while that job runs drains correctly.

## Spike findings

Run on 2026-09-23 on an Arch host (QEMU 11.1, edk2-ovmf 202608) against
build 26100.1742 of the Server 2025 evaluation image and virtio-win
0.1.302. Retained because several points contradict or go beyond
Microsoft's documentation.

- The evaluation "VHD" download is a **.vhdx** (10.9 GB, 64 GiB
  virtual) with a GPT layout: EFI system partition, MSR, NTFS. It is a
  Hyper-V Generation 2 image and needs UEFI. OVMF boots it via the
  fallback `\EFI\Boot\bootx64.efi` path with pristine NVRAM; Windows
  then writes its own `Windows Boot Manager` entry.
- The image ships **no** `C:\Windows\Panther` directory, so nothing
  cached outranks removable media.
- An answer file on removable media is found at both the specialize and
  oobeSystem passes only when named **`Unattend.xml`**. A file named
  `Autounattend.xml` (the name Microsoft's search-order table gives for
  removable media) is ignored at these passes. `setupact.log` records
  `Found usable unattend file for pass [specialize] at [E:\unattend.xml]`
  for a SATA CD-ROM; a FAT USB stick works too and takes precedence, but
  is unnecessary.
- With the answer file in place the full chain ran unattended: computer
  name and time zone applied, all OOBE pages skipped, Administrator
  autologon, first-logon command executed the script from the seed
  volume, `Stop-Computer` ended the QEMU process. About 3 minutes.
- `pnputil /add-driver … /install` from the ISO's `2k25\amd64` folders
  installs cleanly. `viostor` shows `StartMode=Boot` when a virtio-blk
  device was present at install time; the clone then booted from
  virtio-blk. `NetKVM` bound to the virtio NIC and DHCP from QEMU's
  user-mode network came up within seconds.
- The logon scheduled task ran as `runner` in session 1
  (`UserInteractive=True`) 16 seconds after power-on; `Resize-Partition`
  grew `C:` from 64 to 80 GiB; the seed CD was readable by label.
- `slmgr`-style licence query reported `LicenseStatus=1` with a 180 day
  grace period immediately after first boot with network.
- Server 2025 shows a "Send diagnostic data" page at each user's first
  interactive logon; it does not block the scheduled task but is
  suppressed anyway via `DisablePrivacyExperience`.
- The runner zip's SHA-256 matched the `<!-- BEGIN SHA win-x64 -->`
  marker in the release notes, so the existing scraper generalises.
- The `stable-virtio` alias on fedorapeople.org redirects to
  `http://` before landing on the versioned ISO; the default URL pins
  the versioned https path.

## Amendment 2026-09-24 — `windows.image` / `windows.virtio_win`

The two source keys above were renamed before the branch merged and now
accept a local file as well as a URL. Everything else in this spec stands;
`image_url` / `virtio_win_url` never shipped.

- `windows.image_url` → **`windows.image`**, `windows.virtio_win_url` →
  **`windows.virtio_win`** (constants `DefaultWindowsImage` /
  `DefaultVirtioWin`; the values are unchanged). Both are passed through
  `os.ExpandEnv` like `windows.ovmf_dir`.
- Each value is either an `http(s)://` URL — downloaded and cached under
  `paths.images` exactly as before — or an **absolute path** to a file on
  the host. Anything else is rejected at load: a relative path with
  `windows.image must be an http(s) URL or an absolute path`, a `file://`
  URL with `windows.image: use a plain absolute path, not a file:// URL`.
  `config.IsLocalSource` (true when the value is absolute) is the single
  definition the bake, the controller and `setup` share.
- A local file is used **in place** and never copied into `paths.images`:
  no `windows-base.vhdx`, no `.meta` validator sidecar, no
  `virtio-win.iso`. It must exist and be a regular file; when the matching
  `*_sha256` is set it is hashed on every bake and a mismatch fails the
  bake.
- The bake's `fetch` helper became `WindowsOptions.resolveSource`, which
  returns the path to use for either kind of source; the http(s) path
  keeps the checksum-verified / conditional-GET behaviour including the
  cached-file fallback on an upstream failure.
- The overlay's backing format follows a local file instead of assuming
  `vhdx`: `qemu.ImageFormat` maps `.vhdx`→`vhdx`, `.vhd`→`vpc`,
  `.qcow2`→`qcow2`, `.img`/`.raw`→`raw`, and otherwise reads `format`
  from `qemu-img info --output=json`. An http(s) source stays `vhdx`.
- `base-windows.json` records `image` (the configured value) in place of
  `image_url`. For an http(s) source it still records `image_etag`; for a
  local one it records `image_size` (decimal bytes) and `image_mtime`
  (RFC3339) instead, which is what a later reader can compare.
- `config.CheckLocalSource(path) (warning, err)` is the shared preflight:
  it errors when the file is missing or not regular, and returns a warning
  for a path under `/home`, which the units cannot read with
  `ProtectHome=yes`. The controller runs it for each local source at
  startup (warning at WARN, error fatal); `setup` prints
  `ok    windows image <path>` / `ok    virtio-win ISO <path>` and
  `warn  <warning>`.
