# Repository Setup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Set up the github-qemu-runner repository: mise-pinned toolchain, Go module, lint/format/secret-scan matrix, lefthook commit hooks, conventional commits, and a CI workflow that mirrors local checks.

**Architecture:** Per `docs/superpowers/specs/2026-06-10-repo-setup-design.md`. mise pins all binary tools; npm carries commitlint + secretlint; lefthook wires hooks; CI re-runs the identical matrix.

**Tech Stack:** mise, Go 1.26, golangci-lint v2, shellcheck, shfmt, actionlint, zizmor, pinact, lefthook, commitlint, secretlint, GitHub Actions.

**Verification note:** This host already has mise 2026.6.x and node v24. All commands run from the repo root `/home/a1678991/dev/github-runner`.

---

### Task 1: Go module, .gitignore, .editorconfig, mise toolchain

**Files:**
- Create: `go.mod` (via command)
- Create: `.gitignore`
- Create: `.editorconfig`
- Create: `mise.toml` (via command)

- [ ] **Step 1: Create .gitignore**

```gitignore
# binaries
/github-qemu-runner
/dist/

# npm
node_modules/

# editor / OS
*.swp
.DS_Store
```

- [ ] **Step 2: Create .editorconfig** (shfmt reads this for shell indentation)

```ini
root = true

[*]
charset = utf-8
end_of_line = lf
insert_final_newline = true
trim_trailing_whitespace = true

[*.go]
indent_style = tab

[*.{sh,yml,yaml,json,md}]
indent_style = space
indent_size = 2
```

- [ ] **Step 3: Init Go module**

Run: `go mod init github.com/a1678991/github-qemu-runner`
Expected: `go: creating new go.mod: module github.com/a1678991/github-qemu-runner`

- [ ] **Step 4: Pin toolchain with mise**

Run:
```bash
mise use --pin go@1.26 node@24 golangci-lint@latest shellcheck@latest \
  shfmt@latest actionlint@latest lefthook@latest zizmor@latest pinact@latest
```
Expected: `mise.toml` created with exact pinned versions under `[tools]`. Then run `mise ls --current` and confirm every tool shows an installed version (no `missing`).

- [ ] **Step 5: Verify tools execute**

Run: `golangci-lint version && shellcheck --version && shfmt --version && actionlint --version && lefthook version && zizmor --version && pinact --version`
Expected: each prints a version, no errors.

- [ ] **Step 6: Commit**

```bash
git add .gitignore .editorconfig go.mod mise.toml
git commit -m "chore: init Go module and mise-pinned toolchain"
```

---

### Task 2: npm tooling — commitlint + secretlint

**Files:**
- Create: `package.json` (via commands)
- Create: `package-lock.json` (via commands)
- Create: `commitlint.config.mjs`
- Create: `.secretlintrc.json`
- Create: `.secretlintignore`

- [ ] **Step 1: Create package.json and install dev deps**

Run:
```bash
npm init -y >/dev/null
npm pkg set private=true name=github-qemu-runner-devtools version=0.0.0
npm pkg delete main scripts.test keywords author license description
npm install --save-dev @commitlint/cli @commitlint/config-conventional \
  secretlint @secretlint/secretlint-rule-preset-recommend
```
Expected: `package.json` + `package-lock.json` written, `node_modules/` populated (gitignored).

- [ ] **Step 2: Create commitlint.config.mjs**

```js
export default { extends: ['@commitlint/config-conventional'] };
```

- [ ] **Step 3: Create .secretlintrc.json**

```json
{
  "rules": [
    {
      "id": "@secretlint/secretlint-rule-preset-recommend"
    }
  ]
}
```

- [ ] **Step 4: Create .secretlintignore**

```
node_modules/
package-lock.json
```

- [ ] **Step 5: Verify both tools work (failing and passing cases)**

Run: `echo "bad message" | npx commitlint`
Expected: exit 1 with `subject may not be empty` / `type may not be empty` errors.

Run: `echo "feat: good message" | npx commitlint`
Expected: exit 0, no output.

Run: `npx secretlint "**/*"`
Expected: exit 0 (no secrets in repo).

- [ ] **Step 6: Commit**

```bash
git add package.json package-lock.json commitlint.config.mjs .secretlintrc.json .secretlintignore
git commit -m "chore: add commitlint and secretlint via npm"
```

---

### Task 3: lefthook hooks

**Files:**
- Create: `lefthook.yml`

- [ ] **Step 1: Create lefthook.yml**

```yaml
pre-commit:
  parallel: true
  jobs:
    - name: go-fmt
      glob: "*.go"
      run: golangci-lint fmt {staged_files}
      stage_fixed: true
    - name: go-lint
      glob: "*.go"
      run: golangci-lint run
    - name: shfmt
      glob: "*.sh"
      run: shfmt -w {staged_files}
      stage_fixed: true
    - name: shellcheck
      glob: "*.sh"
      run: shellcheck {staged_files}
    - name: actionlint
      glob: ".github/workflows/*.{yml,yaml}"
      run: actionlint
    - name: secretlint
      run: npx secretlint --maskSecrets {staged_files}

commit-msg:
  jobs:
    - name: commitlint
      run: npx commitlint --edit {1}
```

- [ ] **Step 2: Install hooks**

Run: `lefthook install`
Expected: `sync hooks: ✔️ (pre-commit, commit-msg)` (wording may vary; `.git/hooks/pre-commit` and `.git/hooks/commit-msg` now exist).

- [ ] **Step 3: Verify commit-msg hook rejects bad messages**

Run: `git commit --allow-empty -m "bad message"`
Expected: commit REJECTED by commitlint (`type may not be empty`), exit non-zero.

- [ ] **Step 4: Verify a conventional commit passes and commit lefthook.yml**

```bash
git add lefthook.yml
git commit -m "chore: add lefthook commit hooks"
```
Expected: hooks run (secretlint on staged files), commit succeeds.

---

### Task 4: golangci-lint config + binary stub

The lint config needs at least one Go file to lint; create the `main.go` stub the runner plan builds on.

**Files:**
- Create: `.golangci.yml`
- Create: `cmd/github-qemu-runner/main.go`

- [ ] **Step 1: Create .golangci.yml** (v2 schema; `standard` default = govet, staticcheck, errcheck, ineffassign, unused — the spec's floor)

```yaml
version: "2"
linters:
  default: standard
formatters:
  enable:
    - gofumpt
    - goimports
```

- [ ] **Step 2: Create the main.go stub**

```go
// Command github-qemu-runner runs ephemeral GitHub Actions runners in
// QEMU/KVM virtual machines. Subcommands are wired up by the runner
// implementation plan; this stub only reserves the entrypoint.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "github-qemu-runner: not yet implemented")
	os.Exit(2)
}
```

- [ ] **Step 3: Verify lint and build pass**

Run: `golangci-lint run && golangci-lint fmt --diff && go build ./...`
Expected: all exit 0, no diff output from fmt.

- [ ] **Step 4: Commit**

```bash
git add .golangci.yml cmd/github-qemu-runner/main.go
git commit -m "chore: add golangci-lint config and entrypoint stub"
```

---

### Task 5: CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] **Step 1: Create .github/workflows/ci.yml**

Written with version tags first; pinact rewrites them to SHAs in Step 2.

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
        with:
          fetch-depth: 0 # commitlint needs the PR commit range
          persist-credentials: false
      - uses: jdx/mise-action@v2
      - run: npm ci
      - run: golangci-lint run
      - run: golangci-lint fmt --diff
      - run: git ls-files '*.sh' | xargs -r shfmt -d
      - run: git ls-files '*.sh' | xargs -r shellcheck
      - run: actionlint
      - run: zizmor --offline .github/workflows/
      - run: npx secretlint "**/*"
      - if: github.event_name == 'pull_request'
        run: npx commitlint --from "$BASE_SHA" --to HEAD
        env:
          BASE_SHA: ${{ github.event.pull_request.base.sha }}

  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
        with:
          persist-credentials: false
      - uses: jdx/mise-action@v2
      - run: sudo apt-get update && sudo apt-get install -y qemu-utils genisoimage
      - run: go test -race ./...

  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
        with:
          persist-credentials: false
      - uses: jdx/mise-action@v2
      - run: go build ./...
```

(`qemu-utils` + `genisoimage` let the qemu-img/ISO integration-gated tests from the runner plan actually run in CI instead of skipping.)

- [ ] **Step 2: Pin action SHAs**

Run: `pinact run`
Expected: `uses:` lines rewritten to `@<40-char-sha> # vX` form. Verify with `grep 'uses:' .github/workflows/ci.yml` — every ref is a full SHA.

- [ ] **Step 3: Verify workflow lints clean**

Run: `actionlint && zizmor --offline .github/workflows/`
Expected: both exit 0. If zizmor flags `artipacked` or template-injection findings, fix them now (persist-credentials is already false; env-indirection for BASE_SHA is already in place).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: add lint, test, and build workflow"
```

---

### Task 6: Final verification

- [ ] **Step 1: Full local matrix, exactly as CI runs it**

Run:
```bash
golangci-lint run && golangci-lint fmt --diff \
  && git ls-files '*.sh' | xargs -r shfmt -d \
  && git ls-files '*.sh' | xargs -r shellcheck \
  && actionlint && zizmor --offline .github/workflows/ \
  && npx secretlint "**/*" \
  && go test ./... && go build ./...
```
Expected: everything exits 0 (`go test` reports `no test files` — fine at this stage).

- [ ] **Step 2: Verify hook end-to-end one more time**

Run: `git commit --allow-empty -m "not conventional"`
Expected: rejected by commitlint.

Run: `git commit --allow-empty -m "chore: verify hooks" && git reset --hard HEAD~1`
Expected: commit succeeds, then is dropped (it was only a probe).
