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
