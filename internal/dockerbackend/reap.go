package dockerbackend

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
)

// ReapContainers force-removes all containers (running or exited) labeled
// as managed by this controller — crash leftovers from a previous run.
// Best-effort; failures are logged, never fatal (mirrors ReapOrphans).
func ReapContainers(ctx context.Context, dockerBin string, log *slog.Logger) {
	out, err := exec.CommandContext(ctx, dockerBin,
		"ps", "--all", "--quiet", "--filter", "label="+managedLabel).Output()
	if err != nil {
		log.Warn("list managed containers", "err", err)
		return
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return
	}
	args := append([]string{"rm", "--force", "--volumes"}, ids...)
	if out, err := exec.CommandContext(ctx, dockerBin, args...).CombinedOutput(); err != nil {
		log.Warn("remove orphan containers", "err", err, "output", string(out))
		return
	}
	log.Info("removed orphan containers", "count", len(ids))
}
