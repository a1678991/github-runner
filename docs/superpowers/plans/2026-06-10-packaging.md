# Packaging (Arch + Nix) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Operator-installable packaging: Arch `-git` PKGBUILD (sysusers/tmpfiles included) and a Nix flake (package + NixOS module), both built in CI (`packaging.yml`: makepkg inside `archlinux:base-devel`, `nix flake check`).

**Architecture:** Per `docs/superpowers/specs/2026-06-10-packaging-design.md`. Packaging artifacts only — no Go code changes. Branch: `feat/packaging` (already checked out, contains the spec commit).

**Tech Stack:** makepkg/namcap, sysusers.d/tmpfiles.d, Nix flakes, buildGoModule, NixOS module system, GitHub Actions containers.

**Verified environment facts:**
- Host: Arch with `makepkg`, `namcap` may need install (`pacman -S namcap` — ask user if missing), `nix` 2.34.7 with flakes enabled
- `genisoimage`/`mkisofs` owned by `cdrtools` on Arch; nixpkgs' genisoimage is in `cdrkit`; `qemu-base` provides qemu-system-x86_64 + qemu-img on Arch, `qemu_kvm` on nixpkgs
- `nixfmt` is NOT in the mise registry → **spec deviation:** format via `nix fmt` (flake `formatter` output = nixfmt) in lefthook; CI format check lives in packaging.yml's nix job (`nix fmt` + `git diff --exit-code`), not ci.yml (which has no nix)
- Action versions for pinact: `DeterminateSystems/nix-installer-action@v22`, `actions/upload-artifact@v7`, `actions/checkout@v5`, `jdx/mise-action@v2`
- Lefthook hooks fire on every commit (existing matrix); PKGBUILD is bash but does NOT match the `*.sh` globs — shellcheck/shfmt won't fight it

## File map

| Path | Responsibility |
|---|---|
| `LICENSE` | MIT license (required by PKGBUILD `license=()` and nix `meta.license`) |
| `packaging/arch/PKGBUILD` | `-git` VCS package |
| `packaging/arch/github-qemu-runner.sysusers` | gh-runner user + kvm membership |
| `packaging/arch/github-qemu-runner.tmpfiles` | state dir creation |
| `packaging/arch/.gitignore` | ignore makepkg build artifacts |
| `flake.nix` | inputs, package/module/checks/formatter outputs |
| `flake.lock` | pinned nixpkgs |
| `nix/package.nix` | buildGoModule derivation |
| `nix/module.nix` | NixOS module `services.github-qemu-runner` |
| `.github/workflows/packaging.yml` | arch-package + nix CI jobs |
| `lefthook.yml` (modify) | add nixfmt job |
| `README.md` (modify) | Arch + NixOS install sections |

---

### Task 1: LICENSE

**Files:**
- Create: `LICENSE`

- [ ] **Step 1: Create LICENSE (MIT)**

```
MIT License

Copyright (c) 2026 a1678991

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

- [ ] **Step 2: Commit**

```bash
git add LICENSE
git commit -m "chore: add MIT license"
```

---

### Task 2: Arch packaging files + local makepkg test

**Files:**
- Create: `packaging/arch/PKGBUILD`
- Create: `packaging/arch/github-qemu-runner.sysusers`
- Create: `packaging/arch/github-qemu-runner.tmpfiles`
- Create: `packaging/arch/.gitignore`

- [ ] **Step 1: Create packaging/arch/github-qemu-runner.sysusers**

```
u gh-runner - "GitHub Actions QEMU runner" /var/lib/github-qemu-runner
m gh-runner kvm
```

- [ ] **Step 2: Create packaging/arch/github-qemu-runner.tmpfiles**

```
d /var/lib/github-qemu-runner 0750 gh-runner gh-runner -
```

- [ ] **Step 3: Create packaging/arch/.gitignore** (makepkg litters the build dir)

```gitignore
/github-qemu-runner-git/
/src/
/pkg/
/gopath/
*.pkg.tar.zst
*.log
```

- [ ] **Step 4: Create packaging/arch/PKGBUILD**

```bash
# Maintainer: a1678991
pkgname=github-qemu-runner-git
pkgver=r27.0000000
pkgrel=1
pkgdesc='Ephemeral GitHub Actions self-hosted runners in disposable QEMU/KVM VMs'
arch=('x86_64')
url='https://github.com/a1678991/github-qemu-runner'
license=('MIT')
depends=('qemu-base' 'cdrtools')
makedepends=('git' 'go')
provides=('github-qemu-runner')
conflicts=('github-qemu-runner')
source=("$pkgname::git+https://github.com/a1678991/github-qemu-runner.git")
sha256sums=('SKIP')

pkgver() {
  cd "$pkgname"
  printf 'r%s.%s' "$(git rev-list --count HEAD)" "$(git rev-parse --short HEAD)"
}

build() {
  cd "$pkgname"
  export GOPATH="$srcdir/gopath"
  export CGO_CPPFLAGS="${CPPFLAGS}"
  export CGO_CFLAGS="${CFLAGS}"
  export CGO_CXXFLAGS="${CXXFLAGS}"
  export CGO_LDFLAGS="${LDFLAGS}"
  export GOFLAGS="-buildmode=pie -trimpath -mod=readonly -modcacherw"
  go build -o github-qemu-runner ./cmd/github-qemu-runner
}

check() {
  cd "$pkgname"
  export GOPATH="$srcdir/gopath"
  export GOFLAGS="-mod=readonly -modcacherw"
  go test ./...
}

package() {
  cd "$pkgname"
  install -Dm755 github-qemu-runner "$pkgdir/usr/bin/github-qemu-runner"
  install -Dm644 packaging/github-qemu-runner.service \
    "$pkgdir/usr/lib/systemd/system/github-qemu-runner.service"
  install -Dm644 packaging/config.example.yaml \
    "$pkgdir/etc/github-qemu-runner/config.example.yaml"
  install -Dm644 packaging/arch/github-qemu-runner.sysusers \
    "$pkgdir/usr/lib/sysusers.d/github-qemu-runner.conf"
  install -Dm644 packaging/arch/github-qemu-runner.tmpfiles \
    "$pkgdir/usr/lib/tmpfiles.d/github-qemu-runner.conf"
  install -Dm644 LICENSE "$pkgdir/usr/share/licenses/$pkgname/LICENSE"
  install -Dm644 README.md "$pkgdir/usr/share/doc/$pkgname/README.md"
}
```

(`pkgver=r27.0000000` is a placeholder makepkg overwrites via `pkgver()`; the real value lands on first build.)

- [ ] **Step 5: Local build test** (builds from GitHub main — the merged code; the LICENSE this PKGBUILD installs comes from the CLONE, so this FAILS until Task 1's LICENSE commit is pushed... it is NOT pushed yet. Therefore: rewrite source to the local repo for the local test, exactly as CI does)

```bash
cd /home/a1678991/dev/github-runner/packaging/arch
# Local test against the working branch (file:// source, like CI):
sed 's|git+https://github.com/a1678991/github-qemu-runner.git|git+file:///home/a1678991/dev/github-runner|' PKGBUILD > PKGBUILD.local
makepkg -p PKGBUILD.local --cleanbuild --syncdeps --noconfirm
```
Expected: build + check (full `go test ./...` incl. qemu-img/genisoimage tests) succeed; a `github-qemu-runner-git-r*.pkg.tar.zst` appears. NOTE: `git+file://` clones the CHECKED-OUT branch's HEAD (feat/packaging) — Task 1's LICENSE commit must exist locally (it does, committed in Task 1). If makepkg errors because of uncommitted files, commit first — every Task commits before this point.

- [ ] **Step 6: Inspect the package**

```bash
namcap *.pkg.tar.zst | tee /tmp/namcap.out || true
grep ' E: ' /tmp/namcap.out && echo "NAMCAP ERRORS" || echo "no namcap errors"
pacman -Qlp *.pkg.tar.zst
```
Expected: no ` E: ` lines (warnings acceptable — report them); file list shows usr/bin/github-qemu-runner, the unit, config example, sysusers.d/tmpfiles.d conf files, license, README.

- [ ] **Step 7: Clean up local test artifacts and commit**

```bash
rm -f PKGBUILD.local
git status --short   # only the 4 new tracked files; build litter is gitignored
git add packaging/arch/
git commit -m "feat: add Arch Linux -git PKGBUILD with sysusers and tmpfiles"
```

---

### Task 3: Nix package + flake skeleton

**Files:**
- Create: `nix/package.nix`
- Create: `flake.nix`
- Create: `flake.lock` (generated)

- [ ] **Step 1: Create nix/package.nix**

```nix
{
  lib,
  buildGoModule,
  qemu,
  cdrkit,
  src,
}:
buildGoModule {
  pname = "github-qemu-runner";
  version = "0-unstable-2026-06-10";
  inherit src;
  vendorHash = lib.fakeHash; # replaced in Step 3
  subPackages = [ "cmd/github-qemu-runner" ];

  # qemu-img and genisoimage let the integration-gated tests run in the
  # sandbox instead of skipping.
  nativeCheckInputs = [
    qemu
    cdrkit
  ];

  meta = {
    description = "Ephemeral GitHub Actions self-hosted runners in disposable QEMU/KVM VMs";
    homepage = "https://github.com/a1678991/github-qemu-runner";
    license = lib.licenses.mit;
    platforms = [ "x86_64-linux" ];
    mainProgram = "github-qemu-runner";
  };
}
```

- [ ] **Step 2: Create flake.nix** (module + module-eval check arrive in Task 4 — this skeleton has package + formatter only)

```nix
{
  description = "Ephemeral GitHub Actions self-hosted runners on QEMU/KVM";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      package = pkgs.callPackage ./nix/package.nix { src = self; };
    in
    {
      packages.${system}.default = package;

      formatter.${system} = pkgs.nixfmt-rfc-style;

      checks.${system} = {
        inherit package;
      };
    };
}
```
(If `nixfmt-rfc-style` warns about renaming on current nixos-unstable, use `pkgs.nixfmt` instead and note the change.)

- [ ] **Step 3: Compute vendorHash**

```bash
cd /home/a1678991/dev/github-runner
git add nix/package.nix flake.nix   # flakes only see tracked/staged files
nix build .#default 2>&1 | tee /tmp/nixbuild.out || true
grep 'got:' /tmp/nixbuild.out
```
Expected: hash mismatch error showing `got: sha256-...`. Replace `lib.fakeHash` in nix/package.nix with that exact `sha256-...` string.

- [ ] **Step 4: Build and smoke test**

```bash
git add nix/package.nix
nix build .#default --print-build-logs
./result/bin/github-qemu-runner nonsense; echo "exit=$?"
nix run .#default -- nonsense; echo "exit=$?"
```
Expected: build succeeds (go tests run inside the sandbox — watch the log for the test phase actually executing, not skipping), both invocations print usage and `exit=2`. `flake.lock` is created by the first nix command.

- [ ] **Step 5: Format and commit**

```bash
nix fmt flake.nix nix/package.nix
git add flake.nix flake.lock nix/package.nix
git commit -m "feat: add Nix flake with buildGoModule package"
```

---

### Task 4: NixOS module + flake checks

**Files:**
- Create: `nix/module.nix`
- Modify: `flake.nix` (add nixosModules + module-eval check)

- [ ] **Step 1: Create nix/module.nix**

```nix
# NixOS module. Imported via the flake's nixosModules.default, which passes
# `self` so the package option can default to the flake's own build.
self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.github-qemu-runner;
  settingsFormat = pkgs.formats.yaml { };
  configFile = settingsFormat.generate "github-qemu-runner.yaml" cfg.settings;
in
{
  options.services.github-qemu-runner = {
    enable = lib.mkEnableOption "ephemeral GitHub Actions runners on QEMU/KVM";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "github-qemu-runner from this flake";
      description = "github-qemu-runner package to run.";
    };

    settings = lib.mkOption {
      type = settingsFormat.type;
      default = { };
      description = ''
        Configuration rendered to the YAML config file; same schema as
        packaging/config.example.yaml in the repository.
        github.private_key_path defaults to the systemd LoadCredential
        path and normally should not be overridden.
      '';
    };

    privateKeyFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        GitHub App private key, passed to the service via systemd
        LoadCredential. Use a string path (e.g. "/run/secrets/app-key.pem"),
        not a Nix path literal — a literal would copy the key into the
        world-readable store. Note CREDENTIALS_DIRECTORY exists only inside
        the service; run `setup`/`refresh-image` manually via
        `systemd-run -P --wait -p LoadCredential=app-key.pem:<path> ...`.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    services.github-qemu-runner.settings.github.private_key_path =
      lib.mkDefault "\${CREDENTIALS_DIRECTORY}/app-key.pem";

    users.users.gh-runner = {
      isSystemUser = true;
      group = "gh-runner";
      extraGroups = [ "kvm" ];
      home = "/var/lib/github-qemu-runner";
    };
    users.groups.gh-runner = { };

    systemd.services.github-qemu-runner = {
      description = "Ephemeral GitHub Actions runners on QEMU/KVM";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      # qemu-system-x86_64 / qemu-img / genisoimage for the controller
      path = [
        pkgs.qemu_kvm
        pkgs.cdrkit
      ];
      serviceConfig = {
        Type = "exec";
        User = "gh-runner";
        Group = "gh-runner";
        SupplementaryGroups = [ "kvm" ];
        ExecStart = "${lib.getExe cfg.package} -config ${configFile} controller";
        Restart = "on-failure";
        RestartSec = 10;
        # Must exceed the largest pool drain_timeout (default 30m).
        TimeoutStopSec = "35min";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        StateDirectory = "github-qemu-runner";
        ReadWritePaths = [ "/var/lib/github-qemu-runner" ];
        LoadCredential = [ "app-key.pem:${cfg.privateKeyFile}" ];
      };
    };
  };
}
```

- [ ] **Step 2: Extend flake.nix** — full replacement content:

```nix
{
  description = "Ephemeral GitHub Actions self-hosted runners on QEMU/KVM";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      package = pkgs.callPackage ./nix/package.nix { src = self; };
    in
    {
      packages.${system}.default = package;

      nixosModules.default = import ./nix/module.nix self;

      formatter.${system} = pkgs.nixfmt-rfc-style;

      checks.${system} = {
        inherit package;

        # Evaluates the module in a minimal nixosSystem and forces the unit
        # definition (incl. the rendered YAML config) without building a
        # full system closure.
        module-eval =
          let
            eval = nixpkgs.lib.nixosSystem {
              inherit system;
              modules = [
                self.nixosModules.default
                {
                  services.github-qemu-runner = {
                    enable = true;
                    privateKeyFile = "/etc/github-qemu-runner/app-key.pem";
                    settings = {
                      github = {
                        app_id = 1;
                        installation_id = 1;
                      };
                      pools = [
                        {
                          name = "fmt";
                          scope = "org";
                          org = "test";
                          count = 1;
                          cpus = 1;
                          memory_mb = 512;
                          disk_gb = 20;
                          labels = [ "self-hosted" ];
                        }
                      ];
                    };
                  };
                  system.stateVersion = "25.05";
                }
              ];
            };
          in
          pkgs.runCommand "github-qemu-runner-module-eval"
            {
              execStart =
                eval.config.systemd.services.github-qemu-runner.serviceConfig.ExecStart;
              user = eval.config.users.users.gh-runner.name;
            }
            ''
              echo "$user: $execStart" > "$out"
            '';
      };
    };
}
```

- [ ] **Step 3: Run flake check**

```bash
git add nix/module.nix flake.nix
nix flake check --print-build-logs
```
Expected: both checks pass (`package` is cached from Task 3; `module-eval` evaluates the module and renders the config). If `nixosSystem` evaluation fails on missing options, fix the minimal module set — do NOT add `fileSystems`/bootloader config unless evaluation actually demands it (referencing only `systemd.services.*` and `users.users.*` should not).

- [ ] **Step 4: Format and commit**

```bash
nix fmt nix/module.nix flake.nix
git add nix/module.nix flake.nix
git commit -m "feat: add NixOS module and flake checks"
```

---

### Task 5: lefthook nixfmt job

**Files:**
- Modify: `lefthook.yml`

- [ ] **Step 1: Add the nixfmt job to the pre-commit jobs list** (after the secretlint job)

```yaml
    - name: nixfmt
      glob: "*.nix"
      run: nix fmt {staged_files}
      stage_fixed: true
```
(`nix` is a system binary on this host, not mise-managed — no `mise exec` prefix.)

- [ ] **Step 2: Verify the hook fires**

```bash
mise exec -- lefthook validate
git add lefthook.yml
git commit -m "chore: format nix files via lefthook"
```
Expected: validate "All good"; the commit itself runs the new job against... no staged .nix files (skip) — that's fine; Task 4's files are already formatted.

---

### Task 6: packaging.yml workflow

**Files:**
- Create: `.github/workflows/packaging.yml`

- [ ] **Step 1: Create .github/workflows/packaging.yml** (tags first; pinact pins in Step 2)

```yaml
name: Packaging

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  arch-package:
    runs-on: ubuntu-latest
    container: archlinux:base-devel
    steps:
      # git must exist before checkout; base-devel does not include it
      - run: pacman -Syu --noconfirm git go qemu-base cdrtools namcap
      - uses: actions/checkout@v5
        with:
          persist-credentials: false
      - name: Prepare non-root builder copy
        run: |
          useradd -m builder
          git config --system --add safe.directory '*'
          # makepkg clones from this copy; give it a real branch (PR
          # checkouts are detached HEADs, which git clone cannot follow).
          git switch -C ci-build
          cp -r "$PWD" /home/builder/src
          chown -R builder:builder /home/builder/src
      - name: Point PKGBUILD at this checkout
        run: |
          sed -i 's|git+https://github.com/a1678991/github-qemu-runner.git|git+file:///home/builder/src|' \
            /home/builder/src/packaging/arch/PKGBUILD
      - name: Build package
        run: su builder -c 'cd /home/builder/src/packaging/arch && makepkg --noconfirm'
      - name: namcap (fail on errors only)
        run: |
          cd /home/builder/src/packaging/arch
          namcap ./*.pkg.tar.zst | tee /tmp/namcap.out || true
          if grep ' E: ' /tmp/namcap.out; then
            echo '::error::namcap reported errors'
            exit 1
          fi
      - name: Install and smoke test
        run: |
          cd /home/builder/src/packaging/arch
          pacman -U --noconfirm ./*.pkg.tar.zst
          rc=0; github-qemu-runner nonsense || rc=$?
          test "$rc" -eq 2
          pacman -Ql github-qemu-runner-git | grep sysusers.d
          pacman -Ql github-qemu-runner-git | grep tmpfiles.d
      - uses: actions/upload-artifact@v7
        with:
          name: arch-package
          path: /home/builder/src/packaging/arch/*.pkg.tar.zst

  nix:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
        with:
          persist-credentials: false
      - uses: DeterminateSystems/nix-installer-action@v22
      - run: nix flake check --print-build-logs
      - name: Format check
        run: |
          nix fmt
          git diff --exit-code
      - name: Smoke test
        run: |
          rc=0; nix run .#default -- nonsense || rc=$?
          test "$rc" -eq 2
```

- [ ] **Step 2: Pin action SHAs**

```bash
mise exec -- pinact run
grep 'uses:' .github/workflows/packaging.yml
```
Expected: every `uses:` is a 40-char SHA with a version comment. If a tag doesn't resolve (e.g. upload-artifact@v7), check the latest with `gh api repos/<owner>/<repo>/releases/latest --jq .tag_name` and adjust, noting the deviation.

- [ ] **Step 3: Lint the workflow**

```bash
mise exec -- actionlint && mise exec -- zizmor --offline .github/workflows/
```
Expected: both clean. If zizmor flags the container job or sed/grep steps, fix properly (no inline `${{ }}` in run blocks is already satisfied; `persist-credentials: false` is set).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/packaging.yml
git commit -m "ci: build Arch package and Nix flake in packaging workflow"
```

---

### Task 7: README install sections

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add Arch and NixOS sections** — insert immediately AFTER the existing `## Install` section's command block (which documents the manual go-build path) and retitle that existing section `## Install (manual)`. New content:

````markdown
## Install (Arch Linux)

```bash
cd packaging/arch
makepkg --cleanbuild --syncdeps
sudo pacman -U github-qemu-runner-git-*.pkg.tar.zst
```

The package ships sysusers.d/tmpfiles.d fragments, so the `gh-runner` user
(in the `kvm` group) and `/var/lib/github-qemu-runner` are created by
pacman's hooks — no manual `useradd`. Then:

```bash
sudo cp /etc/github-qemu-runner/config.example.yaml /etc/github-qemu-runner/config.yaml
sudoedit /etc/github-qemu-runner/config.yaml
sudo install -m 0600 -o gh-runner -g gh-runner /path/to/app-private-key.pem /etc/github-qemu-runner/app-key.pem
sudo -u gh-runner github-qemu-runner setup
sudo -u gh-runner github-qemu-runner refresh-image
sudo systemctl enable --now github-qemu-runner
```

## Install (NixOS)

```nix
{
  inputs.github-qemu-runner.url = "github:a1678991/github-qemu-runner";

  # In your nixosSystem modules:
  imports = [ inputs.github-qemu-runner.nixosModules.default ];

  services.github-qemu-runner = {
    enable = true;
    # String path, NOT a Nix path literal (a literal copies the key into
    # the world-readable store).
    privateKeyFile = "/run/secrets/app-key.pem";
    settings = {
      github = {
        app_id = 123456;
        installation_id = 7890123;
      };
      pools = [
        {
          name = "build";
          scope = "org";
          org = "my-org";
          count = 1;
          cpus = 8;
          memory_mb = 16384;
          disk_gb = 60;
          labels = [ "self-hosted" "linux" "x64" "build" ];
        }
      ];
    };
  };
}
```

The module wires the key via systemd `LoadCredential`; for manual
`setup`/`refresh-image` runs use
`systemd-run -P --wait -p LoadCredential=app-key.pem:/run/secrets/app-key.pem github-qemu-runner ... setup`.
````

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: add Arch and NixOS install instructions"
```

---

### Task 8: Final verification

- [ ] **Step 1: Full existing matrix** (must stay green with the new files)

```bash
cd /home/a1678991/dev/github-runner
mise exec -- golangci-lint run && mise exec -- golangci-lint fmt --diff \
  && git ls-files '*.sh' | xargs -r mise exec -- shfmt -d \
  && git ls-files '*.sh' | xargs -r mise exec -- shellcheck \
  && mise exec -- actionlint && mise exec -- zizmor --offline .github/workflows/ \
  && mise exec -- npx secretlint "**/*" \
  && go test -race ./... && go build ./...
```
Expected: all green (secretlint also scans the new files; PKGBUILD/nix contain no secrets).

- [ ] **Step 2: Nix re-verification**

```bash
nix flake check --print-build-logs
nix fmt && git diff --exit-code
```
Expected: checks pass, no formatting drift.

- [ ] **Step 3: Arch re-verification (fresh, from the committed state)**

```bash
cd packaging/arch
sed 's|git+https://github.com/a1678991/github-qemu-runner.git|git+file:///home/a1678991/dev/github-runner|' PKGBUILD > PKGBUILD.local
makepkg -p PKGBUILD.local --cleanbuild --syncdeps --noconfirm
namcap ./*.pkg.tar.zst | tee /tmp/namcap.out || true
! grep ' E: ' /tmp/namcap.out
rm -f PKGBUILD.local
git status --short   # must be clean (artifacts gitignored)
```

- [ ] **Step 4: Working tree clean, ready for PR**

```bash
git status --short && git log --oneline main..HEAD
```
Expected: clean tree; commits: spec, LICENSE, PKGBUILD, flake package, module+checks, lefthook, workflow, README.

---

## Deviations from spec (decided during planning)

- **nixfmt via `nix fmt`, not mise**: nixfmt is absent from the mise registry. The flake's `formatter` output provides it; lefthook runs `nix fmt {staged_files}`; the CI format check (`nix fmt` + `git diff --exit-code`) lives in packaging.yml's nix job because ci.yml has no Nix installation. ci.yml is NOT modified.
- **Local makepkg tests use a `git+file://` source rewrite** (same mechanism as CI) because the canonical GitHub source would build merged main, which doesn't yet contain the packaging files themselves.
