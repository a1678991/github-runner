# Linux desktop builds and Chromium E2E

The default QEMU base image is Ubuntu 24.04 x86-64. Its bake includes native
dependencies for Tauri v2 and headless Chromium E2E. These packages are installed
once per image refresh instead of in every disposable job VM. Docker-backed
images and custom non-Noble cloud images do not include this package set.

## What is included

The package list lives in `scripts/guest/bake.sh`, embedded in the runner binary.

| Purpose | Packages |
| --- | --- |
| Native compiler and discovery | `build-essential`, `pkg-config`, `clang`, `curl`, `wget`, `file` |
| Tauri v2 | `libwebkit2gtk-4.1-dev`, `libxdo-dev`, `libssl-dev`, `libayatana-appindicator3-dev`, `librsvg2-dev` (including their GTK development dependencies) |
| Image-processing E2E stack | `libvips-dev` (includes the libvips runtime), `libatomic1` |
| Chromium | Playwright 1.61.1's Ubuntu 24.04 Chromium libraries, fonts and Xvfb; the full list is in the bake script |

Sources: [Tauri Linux prerequisites](https://v2.tauri.app/start/prerequisites/#linux)
and [Playwright native dependencies](https://github.com/microsoft/playwright/blob/v1.61.1/packages/playwright-core/src/server/registry/nativeDeps.ts).
The bake verifies native dependency discovery using `pkg-config` before emitting
`BAKE-OK`. OS package updates arrive through the weekly image refresh.

Node, pnpm, Rust, Go, protoc, the Wild linker and Playwright browser binaries are
not baked. Select their versions in the consuming repository; keep lockfiles and
normal toolchain/browser caches. Building the Tauri binary does not require a
display server. Running its GUI needs a display; Chromium E2E does not test the
Tauri WebKit UI.

## Build a Tauri v2 app

Install the project's Node/pnpm and Rust versions first (for example using its
`mise.toml` or Actions setup steps), then run from the Tauri frontend directory:

```sh
pkg-config --modversion webkit2gtk-4.1 gtk+-3.0 ayatana-appindicator3-0.1 openssl
rustup target add x86_64-unknown-linux-gnu
pnpm install --frozen-lockfile
pnpm tauri build --target x86_64-unknown-linux-gnu --no-bundle -- --locked
```

Projects using `--ld-path=wild` also need `cargo install --locked wild-linker`,
with `$HOME/.cargo/bin` on `PATH`. Projects generating protobuf code must install
their chosen protoc version. Install Go and build sidecars if the app uses them.
`--no-bundle` validates the executable; it does not validate AppImage/deb packaging,
signing, Windows/macOS builds, or desktop interaction.

## Use the image in CI

Check actual installed packages rather than trusting an image version marker.
For Playwright 1.61.1, `playwright install-deps --dry-run chromium` prints
`All system dependencies are installed.` when no packages are missing. Then run
`playwright install --only-shell chromium`. Otherwise use
`playwright install --with-deps --only-shell chromium`. Requiring that message
also handles older CLIs whose dry run only prints an install command.

Do not disable Playwright's native library validation. Browser binaries must
match the project's Playwright version. If an upgrade adds native dependencies,
the job fallback installs them; update the bake list to recover the fast path.

## Apply a changed bake script

Editing `bake.sh` alone does not change an installed controller: Go embeds it at
build time. Build/install the updated runner package or binary, then run:

```sh
sudo systemctl start github-qemu-runner-refresh.service
sudo journalctl -u github-qemu-runner-refresh.service -n 30 --no-pager
sudo systemctl enable --now github-qemu-runner-refresh.timer
```

Ensure the refresh service's `ExecStart` points to the newly built binary and
that `gh-runner` owns its configured images directory and metadata. A successful
refresh atomically replaces `base.qcow2`; existing VMs keep their open backing
image, and newly created VMs use the new image. Already-idle old VMs may still
accept a job until they are naturally replaced. Do not restart the controller
or interrupt active jobs just to roll out native packages.
