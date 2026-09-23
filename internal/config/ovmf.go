package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// OVMF is a UEFI firmware pair: the read-only code image and the
// pristine variable store that each VM copies before boot.
type OVMF struct {
	Code string
	Vars string
}

// ovmfSearchDirs are tried in order when windows.ovmf_dir is empty.
// Overridden by tests.
var ovmfSearchDirs = []string{
	"/usr/share/edk2/x64",      // Arch (edk2-ovmf)
	"/usr/share/OVMF",          // Debian/Ubuntu (ovmf)
	"/usr/share/edk2-ovmf/x64", // older Arch layout
}

// Secure-Boot variants (*.secboot.*, *.ms.fd) are deliberately absent:
// Windows Server does not need Secure Boot and the .ms variants require
// a matching enrolled VARS.
var (
	ovmfCodeNames = []string{"OVMF_CODE.4m.fd", "OVMF_CODE_4M.fd", "OVMF_CODE.fd"}
	ovmfVarsNames = []string{"OVMF_VARS.4m.fd", "OVMF_VARS_4M.fd", "OVMF_VARS.fd"}
)

// ResolveOVMF finds the firmware pair in dir, or in the distro search
// paths when dir is empty.
func ResolveOVMF(dir string) (OVMF, error) {
	if dir != "" {
		return resolveOVMFIn(dir)
	}
	for _, d := range ovmfSearchDirs {
		if fw, err := resolveOVMFIn(d); err == nil {
			return fw, nil
		}
	}
	return OVMF{}, fmt.Errorf("OVMF firmware not found in %v; install edk2-ovmf/ovmf or set windows.ovmf_dir", ovmfSearchDirs)
}

func resolveOVMFIn(dir string) (OVMF, error) {
	code, err := firstExisting(dir, ovmfCodeNames)
	if err != nil {
		return OVMF{}, err
	}
	vars, err := firstExisting(dir, ovmfVarsNames)
	if err != nil {
		return OVMF{}, err
	}
	return OVMF{Code: code, Vars: vars}, nil
}

func firstExisting(dir string, names []string) (string, error) {
	for _, n := range names {
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("none of %v found in %s", names, dir)
}
