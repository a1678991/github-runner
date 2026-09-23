package config

import (
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveOVMFExplicitDir(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "OVMF_CODE_4M.fd"))
	touch(t, filepath.Join(dir, "OVMF_VARS_4M.fd"))
	touch(t, filepath.Join(dir, "OVMF_CODE.secboot.4m.fd")) // must be ignored
	got, err := ResolveOVMF(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != filepath.Join(dir, "OVMF_CODE_4M.fd") || got.Vars != filepath.Join(dir, "OVMF_VARS_4M.fd") {
		t.Errorf("got %+v", got)
	}
}

func TestResolveOVMFPrefersArchNames(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "OVMF_CODE.4m.fd"))
	touch(t, filepath.Join(dir, "OVMF_CODE.fd"))
	touch(t, filepath.Join(dir, "OVMF_VARS.4m.fd"))
	got, err := ResolveOVMF(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got.Code) != "OVMF_CODE.4m.fd" {
		t.Errorf("Code = %q", got.Code)
	}
}

func TestResolveOVMFMissing(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "OVMF_CODE.fd")) // no VARS
	if _, err := ResolveOVMF(dir); err == nil {
		t.Error("want error when VARS missing")
	}
	if _, err := ResolveOVMF(filepath.Join(dir, "nope")); err == nil {
		t.Error("want error for missing dir")
	}
}

func TestResolveOVMFAutoDetect(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "OVMF_CODE.fd"))
	touch(t, filepath.Join(dir, "OVMF_VARS.fd"))
	old := ovmfSearchDirs
	ovmfSearchDirs = []string{filepath.Join(dir, "absent"), dir}
	t.Cleanup(func() { ovmfSearchDirs = old })
	got, err := ResolveOVMF("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Vars != filepath.Join(dir, "OVMF_VARS.fd") {
		t.Errorf("got %+v", got)
	}
	ovmfSearchDirs = []string{filepath.Join(dir, "absent")}
	if _, err := ResolveOVMF(""); err == nil {
		t.Error("want error when no search dir has firmware")
	}
}
