package imagebake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeQEMU writes sentinel to the file named by "-serial file:<path>"
// and exits, standing in for the bake VM. It also dumps its argv (one
// argument per line) to <dir>/argv so tests can assert the VM topology;
// dir is the test's own directory, not the bake dir BakeWindows deletes.
func fakeQEMU(t *testing.T, dir, sentinel string) string {
	t.Helper()
	p := filepath.Join(dir, "fake-qemu")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + filepath.Join(dir, "argv") + "\"\n" +
		"prev=\nfor a in \"$@\"; do\n" +
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
		ImageDir:  images,
		HTTP:      srv.Client(),
		APIBase:   srv.URL,
		Image:     srv.URL + "/image.vhdx",
		VirtioWin: srv.URL + "/virtio-win.iso",
		OVMFCode:  filepath.Join(fw, "OVMF_CODE.fd"),
		OVMFVars:  filepath.Join(fw, "OVMF_VARS.fd"),
		QEMUBin:   fakeQEMU(t, dir, "BAKE-OK"),
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
	if meta["runner_version"] != "2.337.0" || meta["git_version"] != "2.55.0.windows.5" ||
		meta["image"] != srv.URL+"/image.vhdx" || meta["image_etag"] != `"img-1"` || meta["baked_at"] == "" {
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

	// The bake VM's argv: the disk/CD topology Windows needs before
	// viostor exists, OVMF, Hyper-V enlightenments, and reboots allowed.
	argvBytes, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	argv := string(argvBytes)
	for _, want := range []string{
		"ide-hd,drive=boot,bus=ide.0,bootindex=0", // VHDX boots from SATA
		"virtio-blk-pci,drive=extra0",             // dummy disk binds viostor
		"ide-cd,drive=cd0,bus=ide.2",              // virtio-win ISO
		"ide-cd,drive=seed,bus=ide.1",             // Unattend.xml CD
		"if=pflash,format=raw,readonly=on,file=" + filepath.Join(fw, "OVMF_CODE.fd"),
		"hv_relaxed", // Hyper-V enlightenments
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "-no-reboot") {
		t.Errorf("bake VM must allow reboots (OOBE reboots):\n%s", argv)
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
		Image: srv.URL + "/image.vhdx", VirtioWin: srv.URL + "/virtio-win.iso",
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
		Image: srv.URL + "/image.vhdx", VirtioWin: srv.URL + "/virtio-win.iso",
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

// localSources creates a VHDX and a stand-in virtio-win ISO outside
// ImageDir, plus the OVMF pair, and returns their paths.
func localSources(t *testing.T, dir string) (vhdx, iso, ovmfCode, ovmfVars string) {
	t.Helper()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	vhdx = filepath.Join(src, "my-windows.vhdx")
	if out, err := exec.Command("qemu-img", "create", "-f", "vhdx", vhdx, "64M").CombinedOutput(); err != nil {
		t.Fatalf("create vhdx: %v: %s", err, out)
	}
	iso = filepath.Join(src, "my-virtio-win.iso")
	if err := os.WriteFile(iso, []byte("fake-iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"OVMF_CODE.fd", "OVMF_VARS.fd"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return vhdx, iso, filepath.Join(dir, "OVMF_CODE.fd"), filepath.Join(dir, "OVMF_VARS.fd")
}

// A local windows.image / windows.virtio_win is used where it lies: no
// copy into ImageDir, no validator sidecar, and the provenance records
// size+mtime instead of an ETag.
func TestBakeWindowsLocalImage(t *testing.T) {
	requireBakeTools(t)
	srv := windowsBakeServer(t)
	defer srv.Close()
	dir := t.TempDir()
	vhdx, iso, code, vars := localSources(t, dir)
	images := filepath.Join(dir, "images")
	err := BakeWindows(context.Background(), WindowsOptions{
		ImageDir: images, HTTP: srv.Client(), APIBase: srv.URL,
		Image: vhdx, VirtioWin: iso,
		OVMFCode: code, OVMFVars: vars,
		QEMUBin: fakeQEMU(t, dir, "BAKE-OK"),
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
	for _, f := range []string{"windows-base.vhdx", "windows-base.vhdx.meta", "virtio-win.iso"} {
		if _, err := os.Stat(filepath.Join(images, f)); !os.IsNotExist(err) {
			t.Errorf("%s: local sources must not be copied into ImageDir (err = %v)", f, err)
		}
	}
	mb, err := os.ReadFile(filepath.Join(images, "base-windows.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]string
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["image"] != vhdx || meta["image_size"] == "" || meta["image_mtime"] == "" {
		t.Errorf("meta = %v", meta)
	}
	if _, ok := meta["image_etag"]; ok {
		t.Errorf("local source must not record an ETag: meta = %v", meta)
	}
	fi, err := os.Stat(vhdx)
	if err != nil {
		t.Fatal(err)
	}
	if meta["image_size"] != strconv.FormatInt(fi.Size(), 10) {
		t.Errorf("image_size = %q, want %d", meta["image_size"], fi.Size())
	}
	if _, err := time.Parse(time.RFC3339, meta["image_mtime"]); err != nil {
		t.Errorf("image_mtime = %q: %v", meta["image_mtime"], err)
	}
	// The bake VM reads the ISO from where it lies.
	argvBytes, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	if argv := string(argvBytes); !strings.Contains(argv, "file="+iso+",if=none,id=cd0") {
		t.Errorf("argv must reference the local ISO %s:\n%s", iso, argv)
	}
}

func TestBakeWindowsLocalImageSHAMismatch(t *testing.T) {
	requireBakeTools(t)
	srv := windowsBakeServer(t)
	defer srv.Close()
	dir := t.TempDir()
	vhdx, iso, code, vars := localSources(t, dir)
	images := filepath.Join(dir, "images")
	err := BakeWindows(context.Background(), WindowsOptions{
		ImageDir: images, HTTP: srv.Client(), APIBase: srv.URL,
		Image: vhdx, ImageSHA256: strings.Repeat("0", 64), VirtioWin: iso,
		OVMFCode: code, OVMFVars: vars,
		QEMUBin: fakeQEMU(t, dir, "BAKE-OK"),
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("err = %v, want a checksum mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(images, "base-windows.qcow2")); !os.IsNotExist(err) {
		t.Error("a mismatched local image must not publish a base image")
	}
}

func TestBakeWindowsLocalImageMissing(t *testing.T) {
	requireBakeTools(t)
	srv := windowsBakeServer(t)
	defer srv.Close()
	dir := t.TempDir()
	_, iso, code, vars := localSources(t, dir)
	missing := filepath.Join(dir, "src", "absent.vhdx")
	err := BakeWindows(context.Background(), WindowsOptions{
		ImageDir: filepath.Join(dir, "images"), HTTP: srv.Client(), APIBase: srv.URL,
		Image: missing, VirtioWin: iso,
		OVMFCode: code, OVMFVars: vars,
		QEMUBin: fakeQEMU(t, dir, "BAKE-OK"),
	})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %v, want one naming %s", err, missing)
	}
}
