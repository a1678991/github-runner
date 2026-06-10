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

## Install (manual)

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
  commented lines in the unit file). Note this applies only to the
  controller service: `setup` and `refresh-image` run outside systemd where
  `${CREDENTIALS_DIRECTORY}` is unset, so keep a root-readable key path for
  manual commands or wrap them with `systemd-run -p LoadCredential=...`.
- Run exactly ONE controller instance per org/repo scope. Startup reaping
  deletes offline `ghq-*` runner records in its scopes and would tear down a
  second instance's records.
