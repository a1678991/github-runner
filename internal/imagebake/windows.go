package imagebake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/qemu"
	"github.com/a1678991/github-qemu-runner/internal/seed"
	"github.com/a1678991/github-qemu-runner/scripts"
)

// WindowsOptions configures BakeWindows. Zero values for HTTP, APIBase,
// CPUs, MemoryMB, Timeout, and Log take the same defaults as Options.
type WindowsOptions struct {
	ImageDir string
	HTTP     *http.Client
	APIBase  string
	// Image and VirtioWin are each an http(s) URL (downloaded and cached
	// under ImageDir) or an absolute path to a local file (used in place,
	// never copied). See resolveSource.
	Image           string
	ImageSHA256     string
	VirtioWin       string
	VirtioWinSHA256 string
	OVMFCode        string
	OVMFVars        string
	QEMUBin         string
	CPUs            int
	MemoryMB        int
	Timeout         time.Duration
	Log             *slog.Logger
}

func (o *WindowsOptions) defaults() {
	if o.HTTP == nil {
		// Only the default client gets the downgrade guard; a
		// caller-supplied HTTP client is used exactly as given.
		o.HTTP = &http.Client{
			Timeout:       60 * time.Minute, // 11 GB image
			CheckRedirect: NoDowngradeRedirect,
		}
	}
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	if o.CPUs == 0 {
		o.CPUs = 4
	}
	if o.MemoryMB == 0 {
		o.MemoryMB = 8192
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
}

// Windows image artifact names under ImageDir.
const (
	WindowsVHDX     = "windows-base.vhdx"
	VirtioWinISO    = "virtio-win.iso"
	WindowsBase     = "base-windows.qcow2"
	WindowsBaseMeta = "base-windows.json"
	windowsBakeDir  = "bake-windows"
)

// BakeWindows produces <ImageDir>/base-windows.qcow2: download the
// evaluation VHDX and the virtio-win ISO, resolve the runner and Git
// releases, boot an overlay of the VHDX under OVMF with an Unattend.xml
// seed CD that completes OOBE and runs bake.ps1, require the BAKE-OK
// serial sentinel, flatten, swap.
func BakeWindows(ctx context.Context, o WindowsOptions) error {
	o.defaults()
	if err := os.MkdirAll(o.ImageDir, 0o755); err != nil {
		return err
	}
	vhdx, err := o.resolveSource(ctx, o.Image, WindowsVHDX, o.ImageSHA256, "windows image")
	if err != nil {
		return err
	}
	iso, err := o.resolveSource(ctx, o.VirtioWin, VirtioWinISO, o.VirtioWinSHA256, "virtio-win ISO")
	if err != nil {
		return err
	}

	runner, err := LatestRunner(ctx, o.HTTP, o.APIBase, "win", "x64")
	if err != nil {
		return fmt.Errorf("resolve runner release: %w", err)
	}
	if runner.SHA256 == "" {
		o.Log.Warn("runner zip SHA not found in release notes; relying on TLS only")
	}
	git, err := LatestGitForWindows(ctx, o.HTTP, o.APIBase)
	if err != nil {
		return fmt.Errorf("resolve git-for-windows release: %w", err)
	}
	if git.SHA256 == "" {
		o.Log.Warn("git installer SHA not found in release notes; relying on TLS only")
	}
	o.Log.Info("baking windows image", "runner_version", runner.Version, "git_version", git.Version)

	bakeDir := filepath.Join(o.ImageDir, windowsBakeDir)
	if err := os.RemoveAll(bakeDir); err != nil {
		return err
	}
	if err := os.MkdirAll(bakeDir, 0o700); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(bakeDir) }()

	absVHDX, err := filepath.Abs(vhdx)
	if err != nil {
		return err
	}
	// Downloads are always the evaluation VHDX; a local image may be any
	// format qemu-img can use as a backing file.
	backingFormat := "vhdx"
	if config.IsLocalSource(o.Image) {
		if backingFormat, err = qemu.ImageFormat(ctx, absVHDX); err != nil {
			return err
		}
	}
	overlay := filepath.Join(bakeDir, "bake-overlay.qcow2")
	if err := qemu.CreateOverlay(ctx, absVHDX, backingFormat, overlay, 0); err != nil {
		return err
	}
	dummy := filepath.Join(bakeDir, "dummy.raw")
	if out, err := exec.CommandContext(ctx, "qemu-img", "create", "-f", "raw", dummy, "1G").CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create dummy: %v: %s", err, out)
	}
	vars := filepath.Join(bakeDir, "vars.fd")
	if err := qemu.CopyFile(o.OVMFVars, vars); err != nil {
		return fmt.Errorf("copy OVMF vars: %w", err)
	}

	password, err := RandomPassword(32)
	if err != nil {
		return err
	}
	unattend, err := RenderUnattend(password)
	if err != nil {
		return err
	}
	env, err := json.MarshalIndent(map[string]string{
		"runner_version": runner.Version,
		"runner_url":     runner.TarballURL,
		"runner_sha256":  runner.SHA256,
		"git_version":    git.Version,
		"git_url":        git.TarballURL,
		"git_sha256":     git.SHA256,
	}, "", "  ")
	if err != nil {
		return err
	}
	seedISO, err := seed.BuildISOFiles(ctx, bakeDir, "GHQSEED", map[string]string{
		"Unattend.xml":    unattend,
		"bake.ps1":        scripts.WindowsBake,
		"run-one-job.ps1": scripts.WindowsRunOneJob,
		"bake-env.json":   string(env),
	})
	if err != nil {
		return err
	}

	console := filepath.Join(bakeDir, "console.log")
	vm, err := qemu.Start(ctx, o.QEMUBin, qemu.Spec{
		Name: "ghq-bake-windows", CPUs: o.CPUs, MemoryMB: o.MemoryMB,
		OverlayPath: overlay, SeedISOPath: seedISO, ConsoleLog: console,
		QMPSocket:   filepath.Join(bakeDir, "qmp.sock"),
		PIDFile:     filepath.Join(bakeDir, "qemu.pid"),
		Firmware:    &qemu.Firmware{Code: o.OVMFCode, Vars: vars},
		DiskBus:     "ahci", // the VHDX boots from SATA; viostor is installed during this boot
		SeedBus:     "ahci",
		CDROMs:      []string{iso},
		ExtraDisks:  []qemu.Disk{{Path: dummy, Format: "raw"}}, // makes viostor bind → boot-start
		HyperV:      true,
		AllowReboot: true, // OOBE reboots between specialize and oobeSystem
	})
	if err != nil {
		return err
	}
	select {
	case <-vm.Done():
	case <-time.After(o.Timeout):
		_ = vm.Kill()
		return fmt.Errorf("windows bake timed out after %v; console tail:\n%s", o.Timeout, consoleTail(console))
	case <-ctx.Done():
		_ = vm.Kill()
		return ctx.Err()
	}
	consoleOut, err := os.ReadFile(console)
	if err != nil {
		return fmt.Errorf("read console log: %w", err)
	}
	if !bytes.Contains(consoleOut, []byte("BAKE-OK")) {
		return fmt.Errorf("windows bake failed (no BAKE-OK sentinel); console tail:\n%s", lastBytes(consoleOut, 2000))
	}

	newBase := filepath.Join(o.ImageDir, WindowsBase+".new")
	// Leave no ~12 GB .new debris behind when convert or rename fails;
	// after a successful rename the path is gone and Remove is a no-op.
	defer func() { _ = os.Remove(newBase) }()
	if out, err := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2", overlay, newBase).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %v: %s", err, out)
	}
	if err := os.Rename(newBase, filepath.Join(o.ImageDir, WindowsBase)); err != nil {
		return err
	}
	prov := map[string]string{
		"runner_version": runner.Version,
		"git_version":    git.Version,
		"image":          o.Image,
		"baked_at":       time.Now().UTC().Format(time.RFC3339),
	}
	if config.IsLocalSource(o.Image) {
		// No ETag to record for a file on disk; size+mtime is what tells
		// a later reader whether the image behind this base has changed.
		fi, err := os.Stat(absVHDX)
		if err != nil {
			return err
		}
		prov["image_size"] = strconv.FormatInt(fi.Size(), 10)
		prov["image_mtime"] = fi.ModTime().UTC().Format(time.RFC3339)
	} else {
		var imgMeta downloadMeta
		if mb, err := os.ReadFile(vhdx + ".meta"); err == nil {
			_ = json.Unmarshal(mb, &imgMeta)
		}
		prov["image_etag"] = imgMeta.ETag
	}
	meta, err := json.MarshalIndent(prov, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.ImageDir, WindowsBaseMeta), append(meta, '\n'), 0o644); err != nil {
		return err
	}
	o.Log.Info("windows bake complete", "base", filepath.Join(o.ImageDir, WindowsBase))
	return nil
}

// resolveSource returns the local file to use for a windows.image /
// windows.virtio_win value: a download cached under ImageDir for http(s)
// sources, or the path itself for local ones (used in place; never copied).
//
// A download is checksum-verified when sha is set, otherwise conditional
// (ETag/Last-Modified); a network failure on the conditional path keeps
// an existing file, so an upstream outage does not block a rebake. A
// local file must exist and be regular, and is hashed when sha is set.
func (o *WindowsOptions) resolveSource(ctx context.Context, src, cachedName, sha, what string) (string, error) {
	if config.IsLocalSource(src) {
		fi, err := os.Stat(src)
		if err != nil {
			return "", fmt.Errorf("%s: %w", what, err)
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("%s: %s: not a regular file", what, src)
		}
		if sha != "" {
			got, err := fileSHA256(src)
			if err != nil {
				return "", fmt.Errorf("%s: %w", what, err)
			}
			if got != sha {
				return "", fmt.Errorf("%s: checksum mismatch: got %s want %s", src, got, sha)
			}
		}
		o.Log.Info("using local "+what, "path", src)
		return src, nil
	}

	dest := filepath.Join(o.ImageDir, cachedName)
	o.Log.Info("downloading "+what+" (cached if unchanged)", "url", src)
	if sha != "" {
		if err := DownloadVerified(ctx, o.HTTP, src, dest, sha); err != nil {
			return "", err
		}
		return dest, nil
	}
	cached, err := DownloadConditional(ctx, o.HTTP, src, dest)
	if err != nil {
		if ctx.Err() != nil {
			return "", err // a cancelled bake is not an upstream outage
		}
		if _, statErr := os.Stat(dest); statErr != nil {
			return "", fmt.Errorf("download %s: %w", what, err)
		}
		o.Log.Warn("conditional download failed; using the cached file", "what", what, "err", err)
		return dest, nil
	}
	if cached {
		o.Log.Info(what + " unchanged upstream; using cached file")
	}
	return dest, nil
}
