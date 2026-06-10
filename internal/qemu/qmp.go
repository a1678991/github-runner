package qemu

import (
	"bufio"
	"fmt"
	"net"
	"time"
)

// qmpPowerdown speaks just enough QMP to press the virtual power button:
// read greeting, negotiate capabilities, send system_powerdown.
func qmpPowerdown(socketPath string) error {
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return fmt.Errorf("dial QMP: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	if _, err := br.ReadString('\n'); err != nil { // greeting
		return fmt.Errorf("read QMP greeting: %w", err)
	}
	for _, cmd := range []string{
		`{"execute":"qmp_capabilities"}`,
		`{"execute":"system_powerdown"}`,
	} {
		if _, err := fmt.Fprintln(conn, cmd); err != nil {
			return err
		}
		if _, err := br.ReadString('\n'); err != nil {
			return fmt.Errorf("read QMP response: %w", err)
		}
	}
	return nil
}
