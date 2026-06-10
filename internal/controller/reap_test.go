package controller

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/github"
)

type fakeReaperAPI struct {
	mu      sync.Mutex
	runners []github.Runner
	deleted []int64
}

func (f *fakeReaperAPI) ListRunners(context.Context, string) ([]github.Runner, error) {
	return f.runners, nil
}

func (f *fakeReaperAPI) DeleteRunner(_ context.Context, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func TestReapOrphansRemovesWorkdirsAndOfflineRunners(t *testing.T) {
	runDir := t.TempDir()
	stale := filepath.Join(runDir, "ghq-fmt-dead")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	// A pid file pointing at a PID that (a) doesn't exist or (b) isn't a
	// qemu process must not cause a kill, but the dir still goes away.
	if err := os.WriteFile(filepath.Join(stale, "qemu.pid"), []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	api := &fakeReaperAPI{runners: []github.Runner{
		{ID: 1, Name: "ghq-fmt-aaaa", Status: "offline"}, // ours, dead -> delete
		{ID: 2, Name: "ghq-fmt-bbbb", Status: "online"},  // ours, alive -> keep
		{ID: 3, Name: "macmini-tart", Status: "offline"}, // not ours -> keep
	}}

	ReapOrphans(context.Background(), runDir, api, []string{"orgs/o"}, slog.New(slog.DiscardHandler))

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale workdir not removed")
	}
	if len(api.deleted) != 1 || api.deleted[0] != 1 {
		t.Errorf("deleted = %v, want [1]", api.deleted)
	}
}

func TestReapOrphansMissingRunDir(t *testing.T) {
	api := &fakeReaperAPI{}
	// must not panic or error
	ReapOrphans(context.Background(), filepath.Join(t.TempDir(), "absent"), api, nil, slog.New(slog.DiscardHandler))
}

func TestAPIPrefixesDedup(t *testing.T) {
	cfg := &config.Config{Pools: []config.Pool{
		{Scope: "org", Org: "o"},
		{Scope: "org", Org: "o"},
		{Scope: "repo", Repo: "o/r"},
	}}
	got := apiPrefixes(cfg)
	if len(got) != 2 || got[0] != "orgs/o" || got[1] != "repos/o/r" {
		t.Errorf("apiPrefixes = %v", got)
	}
}
