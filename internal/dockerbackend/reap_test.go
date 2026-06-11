package dockerbackend

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reapFakeDocker emits container ids for `ps` and logs all argv.
func reapFakeDocker(t *testing.T, psOutput string) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "docker")
	script := `#!/bin/sh
echo "$@" >> ` + argvLog + `
case "$1" in
  ps) printf '` + psOutput + `' ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

func TestReapContainersRemovesManaged(t *testing.T) {
	bin, argvLog := reapFakeDocker(t, `abc123\ndef456\n`)
	ReapContainers(context.Background(), bin, slog.New(slog.DiscardHandler))
	argv, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "ps --all --quiet --filter label=ghq.managed=true") {
		t.Errorf("ps filter wrong:\n%s", argv)
	}
	if !strings.Contains(string(argv), "rm --force --volumes abc123 def456") {
		t.Errorf("rm argv wrong:\n%s", argv)
	}
}

func TestReapContainersNoneFound(t *testing.T) {
	bin, argvLog := reapFakeDocker(t, ``)
	ReapContainers(context.Background(), bin, slog.New(slog.DiscardHandler))
	argv, _ := os.ReadFile(argvLog)
	if strings.Contains(string(argv), "rm") {
		t.Errorf("rm must not run when nothing matched:\n%s", argv)
	}
}
