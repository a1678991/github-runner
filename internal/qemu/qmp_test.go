package qemu

import (
	"bufio"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQMPPowerdown(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	received := make(chan string, 2)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintln(conn, `{"QMP":{"version":{},"capabilities":[]}}`)
		br := bufio.NewReader(conn)
		for range 2 {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			received <- line
			_, _ = fmt.Fprintln(conn, `{"return":{}}`)
		}
	}()

	if err := qmpPowerdown(sock); err != nil {
		t.Fatal(err)
	}
	want := []string{"qmp_capabilities", "system_powerdown"}
	for _, w := range want {
		select {
		case got := <-received:
			if !strings.Contains(got, w) {
				t.Errorf("got %q, want command %q", got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("server never received %q", w)
		}
	}
}

func TestQMPPowerdownNoSocket(t *testing.T) {
	if err := qmpPowerdown(filepath.Join(t.TempDir(), "absent.sock")); err == nil {
		t.Error("want error for missing socket")
	}
}
