# Repository setup — Design

**Date:** 2026-06-10
**Status:** Approved (brainstorming phase)
**Companion to:** [2026-06-10-qemu-runner-design.md](2026-06-10-qemu-runner-design.md)

## What this covers

Development tooling for the `github-qemu-runner` repository itself: linting,
formatting, secret scanning, commit hooks, commit-message convention, and the
CI workflow that mirrors all of it. The principle (carried over from
github-tart-runner): **local hooks and CI run the same checks** — CI is never
the first place a violation is caught.

## Toolchain management: mise

A single `mise.toml` pins every tool version; contributors run `mise install`
once, CI runs the same pins via `jdx/mise-action`. Pinned tools:

| Tool | Purpose |
|---|---|
| `go` | Toolchain for the project itself |
| `golangci-lint` | Go linting + formatting (v2, with formatters enabled) |
| `shellcheck` | Shell script linting |
| `shfmt` | Shell script formatting |
| `actionlint` | GitHub Actions workflow linting |
| `zizmor` | GitHub Actions security auditing (tart-repo parity) |
| `pinact` | Pin action refs to commit SHAs (run manually before workflow edits) |
| `lefthook` | Git hook manager |
| `node` | Runtime for npm-distributed tools below |

npm-distributed tools (`@commitlint/cli` + `@commitlint/config-conventional`,
`secretlint` + `@secretlint/secretlint-rule-preset-recommend`) are declared in
`package.json` with a committed `package-lock.json`, installed via `npm ci`.
They live in npm rather than mise because both resolve config/presets through
`node_modules`.

## Lint / format matrix

| Target | Linter | Formatter |
|---|---|---|
| Go (`**/*.go`) | `golangci-lint run` | `golangci-lint fmt` (gofumpt + goimports) |
| Shell (`**/*.sh`, bake/guest scripts) | `shellcheck` | `shfmt -d` (check) / `shfmt -w` (fix) |
| Actions (`.github/workflows/*.yml`) | `actionlint`, `zizmor` (offline mode in CI) | — |
| All files | `secretlint` (recommend preset) | — |

Notes:

- `golangci-lint` config lives in `.golangci.yml`; enabled linters decided at
  implementation time, but `govet`, `staticcheck`, `errcheck` and the
  formatter set are the floor.
- Guest/bake shell scripts embedded by the Go binary stay as real `.sh` files
  on disk (tart-repo convention) so shellcheck/shfmt cover them.
- `zizmor` requires actions pinned to commit SHAs — `pinact run` before
  committing workflow changes.

## Git hooks: lefthook

`lefthook.yml`:

- **pre-commit** (parallel, staged files only):
  - `golangci-lint fmt` + `golangci-lint run` on staged `*.go`
  - `shfmt -d` + `shellcheck` on staged `*.sh`
  - `actionlint` when `.github/workflows/**` is staged
  - `secretlint` on all staged files
- **commit-msg**:
  - `commitlint --edit` — enforces Conventional Commits
    (`@commitlint/config-conventional`, config in `commitlint.config.mjs`)

`lefthook install` is part of contributor setup (documented in README;
`mise` task or `npm` postinstall hook may automate it — implementation-time
decision).

## Conventional commits

`@commitlint/config-conventional` defaults: `type(scope): subject` with the
standard type set (`feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `ci`,
`build`, `perf`, `style`, `revert`). Enforced in two places:

1. Locally via the lefthook `commit-msg` hook.
2. In CI on pull requests, by running commitlint across the PR's commit range
   — catches commits made with hooks bypassed (`--no-verify`).

## CI (`.github/workflows/ci.yml`)

Triggered on `push` to `main` and `pull_request`. Jobs:

1. **lint** — `mise install` (cached) + `npm ci`, then the full matrix above
   (`golangci-lint run`, `golangci-lint fmt --diff`, `shellcheck`, `shfmt -d`,
   `actionlint`, `zizmor --offline`, `secretlint`), plus commitlint over the
   PR commit range.
2. **test** — `go test ./...` with race detector.
3. **build** — `go build ./...` (cross-compile check is a later concern;
   x86_64 Linux is the only v1 target).

All actions referenced in workflows are SHA-pinned (enforced by zizmor).

## Out of scope

- Release automation (goreleaser, changelog generation from conventional
  commits) — natural later addition enabled by commitlint, not part of v1.
- Integration/KVM smoke tests in CI — covered in the main design's testing
  section as a later step.
