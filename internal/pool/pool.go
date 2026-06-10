// Package pool supervises a fixed number of ephemeral-runner slots. Each
// slot loops: JIT-register -> provision VM -> liveness gate -> wait for the
// VM to power itself off -> teardown -> repeat.
package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/github"
)

// API is the slice of the GitHub client the pool needs.
type API interface {
	GenerateJITConfig(ctx context.Context, prefix string, req github.JITRequest) (*github.JITResult, error)
	GetRunner(ctx context.Context, prefix string, id int64) (*github.Runner, error)
	DeleteRunner(ctx context.Context, prefix string, id int64) error
	RunnerGroupID(ctx context.Context, prefix, name string) (int64, error)
}

// VM is the slice of qemu.VM the pool needs.
type VM interface {
	Done() <-chan struct{}
	Err() error
	Powerdown(timeout time.Duration) error
	Kill() error
	ConsoleTail() string
}

// Provisioner creates a booted VM for a JIT config. The returned cleanup
// removes the VM's working directory and must be called after the VM exits.
type Provisioner interface {
	Provision(ctx context.Context, name string, p config.Pool, jitConfig string) (VM, func(), error)
}

type Pool struct {
	Cfg  config.Pool
	GH   API
	Prov Provisioner
	Log  *slog.Logger

	// PollInterval for the liveness gate; defaults to 10s. Tests shrink it.
	PollInterval time.Duration
}

// Run blocks until ctx is cancelled and all slots have drained. The desired
// count is static in v1; later autoscaling replaces Cfg.Count with a dynamic
// desired value without touching the slot loop.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range p.Cfg.Count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.runSlot(ctx, i)
		}()
	}
	wg.Wait()
}

func (p *Pool) runSlot(ctx context.Context, slot int) {
	backoff := 15 * time.Second
	for ctx.Err() == nil {
		err := p.runOne(ctx, slot)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.Log.Error("slot iteration failed",
				"pool", p.Cfg.Name, "slot", slot, "err", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}
		backoff = 15 * time.Second
	}
}

// runOne is a single slot iteration.
func (p *Pool) runOne(ctx context.Context, slot int) error {
	prefix := p.Cfg.APIPrefix()
	name := fmt.Sprintf("ghq-%s-%s", p.Cfg.Name, shortID())
	log := p.Log.With("pool", p.Cfg.Name, "slot", slot, "vm", name)

	groupID, err := p.GH.RunnerGroupID(ctx, prefix, p.Cfg.RunnerGroup)
	if err != nil {
		return fmt.Errorf("resolve runner group: %w", err)
	}
	jit, err := p.GH.GenerateJITConfig(ctx, prefix, github.JITRequest{
		Name:          name,
		RunnerGroupID: groupID,
		Labels:        p.Cfg.Labels,
		WorkFolder:    "_work",
	})
	if err != nil {
		return fmt.Errorf("generate jitconfig: %w", err)
	}
	// From here the runner record exists on GitHub. Ephemeral runners that
	// complete a job self-deregister (the delete then 404s, harmlessly);
	// every other exit path needs this explicit delete.
	defer p.deleteRecord(prefix, jit.Runner.ID, log)

	vm, cleanup, err := p.Prov.Provision(ctx, name, p.Cfg, jit.EncodedJITConfig)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	defer cleanup()

	log.Info("VM booted, waiting for runner to come online")
	if err := p.awaitOnline(ctx, vm, prefix, jit.Runner.ID); err != nil {
		if ctx.Err() != nil {
			// Normal shutdown while still waiting for the runner: drain
			// instead of reporting a liveness failure.
			p.drain(vm, prefix, jit.Runner.ID, log)
			return nil
		}
		// Surface the guest console before teardown deletes it.
		log.Warn("liveness gate failed; killing VM", "console_tail", vm.ConsoleTail())
		_ = vm.Kill()
		return err
	}
	log.Info("runner online")

	select {
	case <-vm.Done():
		if vmErr := vm.Err(); vmErr != nil {
			log.Warn("VM exited with error", "err", vmErr, "console_tail", vm.ConsoleTail())
		} else {
			log.Info("VM exited")
		}
		return nil
	case <-ctx.Done():
		p.drain(vm, prefix, jit.Runner.ID, log)
		return nil
	}
}

// awaitOnline is the liveness gate: a guest that wedges before the runner
// connects must not occupy the slot forever.
func (p *Pool) awaitOnline(ctx context.Context, vm VM, prefix string, id int64) error {
	interval := p.PollInterval
	if interval == 0 {
		interval = 10 * time.Second
	}
	deadline := time.NewTimer(time.Duration(p.Cfg.LivenessTimeout))
	defer deadline.Stop()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-vm.Done():
			return fmt.Errorf("VM exited before runner came online: %v", vm.Err())
		case <-deadline.C:
			return fmt.Errorf("runner not online within %v", time.Duration(p.Cfg.LivenessTimeout))
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			r, err := p.GH.GetRunner(ctx, prefix, id)
			if err != nil {
				continue // transient API failure; keep polling until deadline
			}
			if r.Status == "online" {
				return nil
			}
		}
	}
}

// drain handles shutdown while a VM is up: idle runners are deleted from
// GitHub first (so no job can land mid-shutdown) and powered down; busy
// runners get up to DrainTimeout to finish, then are powered down (the job
// fails — GitHub does not requeue jobs from vanished ephemeral runners).
func (p *Pool) drain(vm VM, prefix string, id int64, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := p.GH.GetRunner(ctx, prefix, id)
	// Unknown state (API error at shutdown) gets the busy-grace treatment:
	// a 30-minute wait on an idle runner is cheaper than killing a job.
	busy := err != nil || r.Busy
	if !busy {
		log.Info("draining idle runner")
		p.deleteRecord(prefix, id, log)
		_ = vm.Powerdown(30 * time.Second)
		return
	}
	log.Info("waiting for busy runner to finish", "timeout", time.Duration(p.Cfg.DrainTimeout))
	select {
	case <-vm.Done():
		log.Info("job finished during drain")
	case <-time.After(time.Duration(p.Cfg.DrainTimeout)):
		log.Warn("drain timeout exceeded; powering down (job will fail)")
		_ = vm.Powerdown(30 * time.Second)
	}
}

func (p *Pool) deleteRecord(prefix string, id int64, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.GH.DeleteRunner(ctx, prefix, id); err != nil && !errors.Is(err, github.ErrNotFound) {
		log.Warn("delete runner record failed", "id", id, "err", err)
	}
}

func shortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return hex.EncodeToString(b)
}
