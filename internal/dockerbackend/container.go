package dockerbackend

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Container supervises one job container via the docker CLI. It satisfies
// pool.VM: Done fires when the container exits (docker wait returns).
type Container struct {
	bin  string
	name string
	done chan struct{}

	mu  sync.Mutex
	err error
}

// newContainer starts watching an already-running container. docker wait
// blocks until the container stops and prints its exit code.
func newContainer(bin, name string) *Container {
	c := &Container{bin: bin, name: name, done: make(chan struct{})}
	go func() {
		out, err := exec.Command(bin, "wait", name).Output()
		switch {
		case err != nil:
			var stderr []byte
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				stderr = ee.Stderr
			}
			c.setErr(fmt.Errorf("docker wait %s: %v: %s", name, err, stderr))
		case strings.TrimSpace(string(out)) != "0":
			c.setErr(fmt.Errorf("container %s exited with status %s",
				name, strings.TrimSpace(string(out))))
		}
		close(c.done)
	}()
	return c
}

func (c *Container) setErr(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

// Done is closed when the container has exited.
func (c *Container) Done() <-chan struct{} { return c.done }

// Err reports the container exit error; only meaningful after Done.
func (c *Container) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Kill force-removes the container (and its anonymous volumes, including
// the inner /var/lib/docker) and waits for the watcher to notice. The
// wait is bounded: a hung docker daemon must not wedge the pool slot
// forever — the watcher goroutine then leaks until the daemon recovers,
// which is the lesser evil.
func (c *Container) Kill() error {
	if out, err := exec.Command(c.bin, "rm", "--force", "--volumes", c.name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm %s: %v: %s", c.name, err, out)
	}
	select {
	case <-c.done:
		return nil
	case <-time.After(2 * time.Minute):
		return fmt.Errorf("container %s not reaped 2m after docker rm", c.name)
	}
}

// Powerdown stops the container gracefully: SIGTERM to the entrypoint,
// SIGKILL after timeout (docker stop's built-in escalation), Kill as the
// last resort. Always terminates the container.
func (c *Container) Powerdown(timeout time.Duration) error {
	secs := max(int(timeout/time.Second), 1)
	if err := exec.Command(c.bin, "stop", "--time", strconv.Itoa(secs), c.name).Run(); err != nil {
		return c.Kill()
	}
	select {
	case <-c.done:
		return nil
	case <-time.After(timeout + 30*time.Second):
		return c.Kill()
	}
}

// ConsoleTail returns the last 2 KiB of the container's logs, so failures
// can be surfaced into the journal before teardown removes the container.
func (c *Container) ConsoleTail() string {
	out, err := exec.Command(c.bin, "logs", "--tail", "50", c.name).CombinedOutput()
	if err != nil {
		return ""
	}
	if len(out) > 2048 {
		out = out[len(out)-2048:]
	}
	return string(out)
}
