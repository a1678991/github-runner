package imagebake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeQEMU writes sentinel to the file named by "-serial file:<path>"
// and exits, standing in for the bake VM.
func fakeQEMU(t *testing.T, dir, sentinel string) string {
	t.Helper()
	p := filepath.Join(dir, "fake-qemu")
	script := "#!/bin/sh\nprev=\nfor a in \"$@\"; do\n" +
		"  if [ \"$prev\" = -serial ]; then echo '" + sentinel + "' > \"${a#file:}\"; fi\n" +
		"  prev=$a\ndone\nexit 0\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func windowsBakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	vhdx := filepath.Join(t.TempDir(), "src.vhdx")
	if out, err := exec.Command("qemu-img", "create", "-f", "vhdx", vhdx, "64M").CombinedOutput(); err != nil {
		t.Fatalf("create vhdx: %v: %s", err, out)
	}
	vhdxBytes, err := os.ReadFile(vhdx)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/image.vhdx", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"img-1"`)
		_, _ = w.Write(vhdxBytes)
	})
	mux.HandleFunc("/virtio-win.iso", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake-iso"))
	})
	mux.HandleFunc("/repos/actions/runner/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.337.0",
			"body":     "actions-runner-win-x64-2.337.0.zip <!-- BEGIN SHA win-x64 -->" + strings.Repeat("e", 64) + "<!-- END SHA win-x64 -->",
			"assets":   []map[string]any{{"name": "actions-runner-win-x64-2.337.0.zip", "browser_download_url": "https://x/win.zip"}},
		})
	})
	mux.HandleFunc("/repos/git-for-windows/git/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.55.0.windows.5",
			"body":     "Git-2.55.0.5-64-bit.exe | " + strings.Repeat("d", 64),
			"assets":   []map[string]any{{"name": "Git-2.55.0.5-64-bit.exe", "browser_download_url": "https://x/git.exe"}},
		})
	})
	return httptest.NewServer(mux)
}

func requireBakeTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"qemu-img", "genisoimage"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

func TestBakeWindows(t *testing.T) {
	requireBakeTools(t)
	srv := windowsBakeServer(t)
	defer srv.Close()
	dir := t.TempDir()
	fw := filepath.Join(dir, "fw")
	if err := os.MkdirAll(fw, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"OVMF_CODE.fd", "OVMF_VARS.fd"} {
		if err := os.WriteFile(filepath.Join(fw, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	images := filepath.Join(dir, "images")
	err := BakeWindows(context.Background(), WindowsOptions{
		ImageDir:     images,
		HTTP:         srv.Client(),
		APIBase:      srv.URL,
		ImageURL:     srv.URL + "/image.vhdx",
		VirtioWinURL: srv.URL + "/virtio-win.iso",
		OVMFCode:     filepath.Join(fw, "OVMF_CODE.fd"),
		OVMFVars:     filepath.Join(fw, "OVMF_VARS.fd"),
		QEMUBin:      fakeQEMU(t, dir, "BAKE-OK"),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(images, "base-windows.qcow2")
	info, err := exec.Command("qemu-img", "info", base).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v: %s", err, info)
	}
	if strings.Contains(string(info), "backing file") {
		t.Errorf("base must be flattened:\n%s", info)
	}
	mb, err := os.ReadFile(filepath.Join(images, "base-windows.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]string
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["runner_version"] != "2.337.0" || meta["git_version"] != "2.55.0.windows.5" || meta["image_etag"] != `"img-1"` || meta["baked_at"] == "" {
		t.Errorf("meta = %v", meta)
	}
	if _, err := os.Stat(filepath.Join(images, "bake-windows")); !os.IsNotExist(err) {
		t.Error("bake dir not cleaned up")
	}
	for _, f := range []string{"windows-base.vhdx", "windows-base.vhdx.meta", "virtio-win.iso"} {
		if _, err := os.Stat(filepath.Join(images, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestBakeWindowsNoSentinel(t *testing.T) {
	requireBakeTools(t)
	srv := windowsBakeServer(t)
	defer srv.Close()
	dir := t.TempDir()
	for _, f := range []string{"OVMF_CODE.fd", "OVMF_VARS.fd"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	images := filepath.Join(dir, "images")
	err := BakeWindows(context.Background(), WindowsOptions{
		ImageDir: images, HTTP: srv.Client(), APIBase: srv.URL,
		ImageURL: srv.URL + "/image.vhdx", VirtioWinURL: srv.URL + "/virtio-win.iso",
		OVMFCode: filepath.Join(dir, "OVMF_CODE.fd"), OVMFVars: filepath.Join(dir, "OVMF_VARS.fd"),
		QEMUBin: fakeQEMU(t, dir, "BAKE-FAILED: boom"),
	})
	if err == nil || !strings.Contains(err.Error(), "BAKE-FAILED: boom") {
		t.Errorf("err = %v, want console tail with BAKE-FAILED", err)
	}
	if _, err := os.Stat(filepath.Join(images, "base-windows.qcow2")); !os.IsNotExist(err) {
		t.Error("failed bake must not publish a base image")
	}
}

func TestBakeSeedContents(t *testing.T) {
	requireBakeTools(t)
	srv := windowsBakeServer(t)
	defer srv.Close()
	dir := t.TempDir()
	for _, f := range []string{"OVMF_CODE.fd", "OVMF_VARS.fd"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A fake qemu that copies the bake dir aside before exiting so the
	// test can inspect the seed files (BakeWindows removes bake-windows/).
	keep := filepath.Join(dir, "keep")
	fake := filepath.Join(dir, "fake-qemu")
	script := "#!/bin/sh\nprev=\nfor a in \"$@\"; do\n" +
		"  if [ \"$prev\" = -serial ]; then f=\"${a#file:}\"; echo BAKE-OK > \"$f\"; cp -r \"$(dirname \"$f\")\" " + keep + "; fi\n" +
		"  prev=$a\ndone\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := BakeWindows(context.Background(), WindowsOptions{
		ImageDir: filepath.Join(dir, "images"), HTTP: srv.Client(), APIBase: srv.URL,
		ImageURL: srv.URL + "/image.vhdx", VirtioWinURL: srv.URL + "/virtio-win.iso",
		OVMFCode: filepath.Join(dir, "OVMF_CODE.fd"), OVMFVars: filepath.Join(dir, "OVMF_VARS.fd"),
		QEMUBin: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"Unattend.xml", "bake.ps1", "run-one-job.ps1", "bake-env.json", "seed.iso", "vars.fd", "dummy.raw", "bake-overlay.qcow2"} {
		if _, err := os.Stat(filepath.Join(keep, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	env, err := os.ReadFile(filepath.Join(keep, "bake-env.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e map[string]string
	if err := json.Unmarshal(env, &e); err != nil {
		t.Fatal(err)
	}
	if e["runner_url"] != "https://x/win.zip" || e["runner_sha256"] != strings.Repeat("e", 64) ||
		e["git_url"] != "https://x/git.exe" || e["git_sha256"] != strings.Repeat("d", 64) {
		t.Errorf("bake-env = %v", e)
	}
	if vars, _ := os.ReadFile(filepath.Join(keep, "vars.fd")); string(vars) != "OVMF_VARS.fd" {
		t.Error("vars.fd must be a copy of the pristine OVMF_VARS")
	}
}
