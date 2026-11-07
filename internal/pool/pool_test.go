package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/github"
)

type fakeAPI struct {
	mu      sync.Mutex
	status  string
	busy    bool
	deleted []int64
	jitErr  error
	getErr  error
}

func (f *fakeAPI) GenerateJITConfig(_ context.Context, _ string, req github.JITRequest) (*github.JITResult, error) {
	if f.jitErr != nil {
		return nil, f.jitErr
	}
	return &github.JITResult{Runner: github.Runner{ID: 42, Name: req.Name}, EncodedJITConfig: "blob"}, nil
}

func (f *fakeAPI) setGetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

func (f *fakeAPI) setRunner(status string, busy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.busy = status, busy
}

func (f *fakeAPI) GetRunner(context.Context, string, int64) (*github.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &github.Runner{ID: 42, Status: f.status, Busy: f.busy}, nil
}

func (f *fakeAPI) DeleteRunner(_ context.Context, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeAPI) RunnerGroupID(context.Context, string, string) (int64, error) { return 1, nil }

func (f *fakeAPI) deletedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.deleted...)
}

type fakeVM struct {
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	killed  bool
	powered bool
}

func newFakeVM() *fakeVM                { return &fakeVM{done: make(chan struct{})} }
func (v *fakeVM) exit()                 { v.once.Do(func() { close(v.done) }) }
func (v *fakeVM) Done() <-chan struct{} { return v.done }
func (v *fakeVM) Err() error            { return nil }
func (v *fakeVM) Kill() error {
	v.mu.Lock()
	v.killed = true
	v.mu.Unlock()
	v.exit()
	return nil
}

func (v *fakeVM) Powerdown(time.Duration) error {
	v.mu.Lock()
	v.powered = true
	v.mu.Unlock()
	v.exit()
	return nil
}
func (v *fakeVM) ConsoleTail() string { return "" }
func (v *fakeVM) wasKilled() bool     { v.mu.Lock(); defer v.mu.Unlock(); return v.killed }
func (v *fakeVM) wasPowered() bool    { v.mu.Lock(); defer v.mu.Unlock(); return v.powered }

type fakeProv struct {
	vm      *fakeVM
	err     error
	mu      sync.Mutex
	cleaned int
}

func (f *fakeProv) Provision(context.Context, string, config.Pool, string) (VM, func(), error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.vm, func() { f.mu.Lock(); f.cleaned++; f.mu.Unlock() }, nil
}

func (f *fakeProv) cleanedCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.cleaned }

func testPool(api *fakeAPI, prov *fakeProv) *Pool {
	return &Pool{
		Cfg: config.Pool{
			Name: "fmt", Scope: "org", Org: "o", Count: 1,
			CPUs: 1, MemoryMB: 512, DiskGB: 10,
			Labels:          []string{"x"},
			RunnerGroup:     "Default",
			LivenessTimeout: config.Duration(200 * time.Millisecond),
			DrainTimeout:    config.Duration(200 * time.Millisecond),
		},
		GH: api, Prov: prov,
		Log:            slog.New(slog.DiscardHandler),
		PollInterval:   10 * time.Millisecond,
		HealthInterval: 10 * time.Millisecond,
		HealthGrace:    100 * time.Millisecond,
	}
}

// startPastGate runs runOne and returns once it has passed the liveness gate.
func startPastGate(ctx context.Context, p *Pool) <-chan error {
	done := make(chan error, 1)
	go func() { done <- p.runOne(ctx, 0) }()
	time.Sleep(50 * time.Millisecond)
	return done
}

func TestRunOneHappyPath(t *testing.T) {
	api := &fakeAPI{status: "online"}
	vm := newFakeVM()
	prov := &fakeProv{vm: vm}
	p := testPool(api, prov)
	go func() { time.Sleep(50 * time.Millisecond); vm.exit() }() // job "finishes"
	if err := p.runOne(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if prov.cleanedCount() != 1 {
		t.Error("cleanup not called")
	}
	if got := api.deletedIDs(); len(got) != 1 || got[0] != 42 {
		t.Errorf("deleted = %v, want [42]", got)
	}
}

func TestRunOneLivenessTimeout(t *testing.T) {
	api := &fakeAPI{status: "offline"} // runner never connects
	vm := newFakeVM()
	p := testPool(api, &fakeProv{vm: vm})
	err := p.runOne(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "not online") {
		t.Fatalf("err = %v", err)
	}
	if !vm.wasKilled() {
		t.Error("wedged VM was not killed")
	}
	if len(api.deletedIDs()) != 1 {
		t.Error("runner record not deleted")
	}
}

func TestLostRunnerKillsVM(t *testing.T) {
	notFound := fmt.Errorf("GET runner: %w", github.ErrNotFound)
	for name, lose := range map[string]func(*fakeAPI){
		"record deleted": func(a *fakeAPI) { a.setGetErr(notFound) },
		"offline idle":   func(a *fakeAPI) { a.setRunner("offline", false) },
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{status: "online"}
			vm := newFakeVM() // alive with no runner: never exits on its own
			done := startPastGate(context.Background(), testPool(api, &fakeProv{vm: vm}))
			lose(api)
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "runner lost") {
					t.Fatalf("err = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("slot stayed held by a VM with no runner")
			}
			if !vm.wasKilled() {
				t.Error("VM was not killed")
			}
			if len(api.deletedIDs()) != 1 {
				t.Error("runner record not deleted")
			}
		})
	}
}

// Every case here must leave the VM running well past HealthGrace.
func TestHealthCheckKeepsVM(t *testing.T) {
	for name, tc := range map[string]struct {
		during func(*fakeAPI)
		wait   time.Duration
		after  func(*fakeAPI)
		grace  time.Duration
	}{
		// A wedged busy runner is still caught: GitHub eventually clears
		// busy or removes the record.
		"offline busy": {
			during: func(a *fakeAPI) { a.setRunner("offline", true) },
			wait:   400 * time.Millisecond,
		},
		// Says nothing about the runner.
		"API errors": {
			during: func(a *fakeAPI) { a.setGetErr(errors.New("github is down")) },
			wait:   400 * time.Millisecond,
		},
		// Coming back online restarts the grace period.
		"brief outage": {
			during: func(a *fakeAPI) { a.setRunner("offline", false) },
			wait:   20 * time.Millisecond,
			after:  func(a *fakeAPI) { a.setRunner("online", false) },
			grace:  time.Second, // outlasts a slow CI scheduler
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{status: "online"}
			vm := newFakeVM()
			p := testPool(api, &fakeProv{vm: vm})
			if tc.grace != 0 {
				p.HealthGrace = tc.grace
			}
			done := startPastGate(context.Background(), p)
			tc.during(api)
			time.Sleep(tc.wait)
			if tc.after != nil {
				tc.after(api)
				time.Sleep(p.HealthGrace + 100*time.Millisecond)
			}
			if vm.wasKilled() {
				t.Fatal("healthy VM was killed")
			}
			vm.exit()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunOneProvisionFailureDeletesRecord(t *testing.T) {
	api := &fakeAPI{status: "online"}
	p := testPool(api, &fakeProv{err: errors.New("qemu-img exploded")})
	if err := p.runOne(context.Background(), 0); err == nil {
		t.Fatal("want error")
	}
	if len(api.deletedIDs()) != 1 {
		t.Error("runner record not deleted after provision failure")
	}
}

func TestDrainIdleRunner(t *testing.T) {
	api := &fakeAPI{status: "online", busy: false}
	vm := newFakeVM()
	p := testPool(api, &fakeProv{vm: vm})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.runOne(ctx, 0) }()
	time.Sleep(50 * time.Millisecond) // let it pass the liveness gate
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runOne did not return after cancel")
	}
	if !vm.wasPowered() {
		t.Error("idle VM not powered down")
	}
	if len(api.deletedIDs()) == 0 {
		t.Error("idle runner record not deleted before powerdown")
	}
}

func TestDrainBusyRunnerTimesOut(t *testing.T) {
	api := &fakeAPI{status: "online", busy: true}
	vm := newFakeVM() // never exits on its own
	p := testPool(api, &fakeProv{vm: vm})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.runOne(ctx, 0) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done: // DrainTimeout (200ms) then forced powerdown
	case <-time.After(2 * time.Second):
		t.Fatal("runOne did not return")
	}
	if !vm.wasPowered() {
		t.Error("busy VM not powered down after drain timeout")
	}
}

func TestRunReturnsOnCancel(t *testing.T) {
	api := &fakeAPI{jitErr: errors.New("api down")}
	p := testPool(api, &fakeProv{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished := make(chan struct{})
	go func() { p.Run(ctx); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on cancelled context")
	}
}

func TestDrainTreatsAPIErrorAsBusy(t *testing.T) {
	api := &fakeAPI{status: "online", busy: false}
	vm := newFakeVM()
	p := testPool(api, &fakeProv{vm: vm})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.runOne(ctx, 0) }()
	time.Sleep(50 * time.Millisecond) // pass the liveness gate
	api.setGetErr(errors.New("github is down"))
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runOne did not return")
	}
	if !vm.wasPowered() {
		t.Error("VM not powered down after drain timeout")
	}
	// Busy-grace path: only the deferred record delete runs (1), not the
	// idle path's pre-powerdown delete (which would make it 2).
	if got := api.deletedIDs(); len(got) != 1 {
		t.Errorf("deleted %d times, want 1 (busy path)", len(got))
	}
}

func TestDrainSkipsWaitWhenRunnerMissing(t *testing.T) {
	api := &fakeAPI{status: "online"}
	vm := newFakeVM()
	p := testPool(api, &fakeProv{vm: vm})
	p.Cfg.DrainTimeout = config.Duration(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := startPastGate(ctx, p)
	api.setGetErr(fmt.Errorf("GET runner: %w", github.ErrNotFound))
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain waited for a runner that no longer exists")
	}
	if !vm.wasPowered() {
		t.Error("VM not powered down")
	}
}

func TestCancelDuringLivenessGateDrains(t *testing.T) {
	api := &fakeAPI{status: "offline"} // never comes online
	vm := newFakeVM()
	p := testPool(api, &fakeProv{vm: vm})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.runOne(ctx, 0) }()
	time.Sleep(30 * time.Millisecond) // inside the liveness gate
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown during liveness gate must not be an error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runOne did not return")
	}
	if !vm.wasPowered() {
		t.Error("VM not powered down via drain")
	}
	if vm.wasKilled() {
		t.Error("VM was hard-killed; expected graceful drain")
	}
}
