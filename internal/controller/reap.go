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
// NOTE: deletion is scoped only by the ghq- name prefix, so this assumes a
// single controller instance per org/repo scope. A second controller on the
// same scope would have its offline (e.g. mid-boot) runner records deleted
// by this instance's startup reap.
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
