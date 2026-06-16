// Package imagebake builds the base VM image: Ubuntu 24.04 cloud image +
// Docker + actions-runner, flattened into <ImageDir>/base.qcow2 and
// swapped in atomically.
package imagebake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/a1678991/github-qemu-runner/internal/qemu"
	"github.com/a1678991/github-qemu-runner/internal/seed"
	"github.com/a1678991/github-qemu-runner/scripts"
)

const (
	DefaultImageURL = "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
	DefaultSumsURL  = "https://cloud-images.ubuntu.com/noble/current/SHA256SUMS"
)

type Options struct {
	ImageDir string
	HTTP     *http.Client
	APIBase  string
	ImageURL string
	SumsURL  string
	QEMUBin  string
	CPUs     int
	MemoryMB int
	Timeout  time.Duration
	Log      *slog.Logger
}

func (o *Options) defaults() {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 15 * time.Minute}
	}
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	if o.ImageURL == "" {
		o.ImageURL = DefaultImageURL
	}
	if o.SumsURL == "" {
		o.SumsURL = DefaultSumsURL
	}
	if o.CPUs == 0 {
		o.CPUs = 4
	}
	if o.MemoryMB == 0 {
		o.MemoryMB = 4096
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
}

// Release identifies an actions/runner build for one linux arch.
type Release struct {
	Version    string
	TarballURL string
	SHA256     string
}

// ParseSHA256SUMS finds filename's hash in sha256sum-format output
// (lines of "<hash> *<name>" or "<hash>  <name>").
func ParseSHA256SUMS(sums []byte, filename string) (string, error) {
	for line := range strings.Lines(string(sums)) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == filename {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s not found in SHA256SUMS", filename)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DownloadVerified fetches url to dest unless dest already matches wantSHA.
// Empty wantSHA skips verification (TLS is then the only integrity check).
func DownloadVerified(ctx context.Context, client *http.Client, url, dest, wantSHA string) error {
	if wantSHA != "" {
		if got, err := fileSHA256(dest); err == nil && got == wantSHA {
			return nil // cached
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if wantSHA != "" {
		got, err := fileSHA256(tmp)
		if err != nil {
			return err
		}
		if got != wantSHA {
			_ = os.Remove(tmp)
			return fmt.Errorf("%s: checksum mismatch: got %s want %s", url, got, wantSHA)
		}
	}
	return os.Rename(tmp, dest)
}

// LatestRunner resolves the newest actions/runner release for the given
// arch ("x64" or "arm64"). The tarball SHA is scraped from the release
// notes; if the notes format changes, SHA256 comes back empty and the
// caller proceeds on TLS alone.
func LatestRunner(ctx context.Context, client *http.Client, apiBase, arch string) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(apiBase, "/")+"/repos/actions/runner/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("releases/latest: %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, err
	}
	version := strings.TrimPrefix(rel.TagName, "v")
	want := fmt.Sprintf("actions-runner-linux-%s-%s.tar.gz", arch, version)
	out := Release{Version: version}
	for _, a := range rel.Assets {
		if a.Name == want {
			out.TarballURL = a.BrowserDownloadURL
			break
		}
	}
	if out.TarballURL == "" {
		return Release{}, fmt.Errorf("asset %s not found in release %s", want, rel.TagName)
	}
	// SHA comes from the "<!-- BEGIN SHA linux-<arch> -->" markers GitHub
	// embeds in the release notes' checksum table; the asset name alone is
	// ambiguous (it also appears in the install instructions, and the first
	// hex token after that is a different platform's SHA). If the markers
	// vanish in a future format change, SHA256 stays empty and the caller
	// proceeds on TLS alone.
	re := regexp.MustCompile(`<!-- BEGIN SHA linux-` + regexp.QuoteMeta(arch) + ` -->\s*([0-9a-fA-F]{64})\s*<!-- END SHA linux-` + regexp.QuoteMeta(arch) + ` -->`)
	if m := re.FindStringSubmatch(rel.Body); m != nil {
		out.SHA256 = strings.ToLower(m[1])
	}
	return out, nil
}

// shQuote single-quotes s for POSIX shell: no expansions survive inside
// single quotes, unlike the double quotes %q would produce.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// BakeUserData renders the cloud-config for the one-time bake boot: drop
// the env file and scripts into /run, then execute bake.sh.
func BakeUserData(rel Release) (string, error) {
	env := fmt.Sprintf("VERSION=%s\nTARBALL_URL=%s\nTARBALL_SHA256=%s\n",
		shQuote(rel.Version), shQuote(rel.TarballURL), shQuote(rel.SHA256))
	doc := map[string]any{
		"write_files": []map[string]any{
			{"path": "/run/bake-env", "permissions": "0600", "content": env},
			{"path": "/run/bake.sh", "permissions": "0755", "content": scripts.Bake},
			{"path": "/run/run-one-job", "permissions": "0755", "content": scripts.RunOneJob},
		},
		"runcmd": [][]string{{"bash", "/run/bake.sh"}},
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(b), nil
}

// Bake produces <ImageDir>/base.qcow2: download + verify the cloud
// image, resolve the runner release, boot a build overlay with the bake
// seed, require the BAKE-OK serial-console sentinel, flatten, swap.
func Bake(ctx context.Context, o Options) error {
	o.defaults()
	imagesDir := o.ImageDir
	bakeDir := filepath.Join(imagesDir, "bake")
	if err := os.MkdirAll(bakeDir, 0o755); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(bakeDir) }()

	sums, err := fetchAll(ctx, o.HTTP, o.SumsURL)
	if err != nil {
		return fmt.Errorf("fetch SHA256SUMS: %w", err)
	}
	wantSHA, err := ParseSHA256SUMS(sums, path.Base(o.ImageURL))
	if err != nil {
		return err
	}
	cloudImg := filepath.Join(imagesDir, path.Base(o.ImageURL))
	o.Log.Info("downloading cloud image (cached if unchanged)", "url", o.ImageURL)
	if err := DownloadVerified(ctx, o.HTTP, o.ImageURL, cloudImg, wantSHA); err != nil {
		return err
	}

	rel, err := LatestRunner(ctx, o.HTTP, o.APIBase, "x64")
	if err != nil {
		return fmt.Errorf("resolve runner release: %w", err)
	}
	if rel.SHA256 == "" {
		o.Log.Warn("runner tarball SHA not found in release notes; relying on TLS only")
	}
	o.Log.Info("baking image", "runner_version", rel.Version)

	absCloudImg, err := filepath.Abs(cloudImg)
	if err != nil {
		return err
	}
	overlay := filepath.Join(bakeDir, "bake-overlay.qcow2")
	if err := qemu.CreateOverlay(ctx, absCloudImg, overlay, 20); err != nil {
		return err
	}

	ud, err := BakeUserData(rel)
	if err != nil {
		return err
	}
	iso, err := seed.BuildISO(ctx, bakeDir, ud, seed.MetaData("ghq-bake-"+rel.Version, "ghq-bake"))
	if err != nil {
		return err
	}

	console := filepath.Join(bakeDir, "console.log")
	vm, err := qemu.Start(ctx, o.QEMUBin, qemu.Spec{
		Name: "ghq-bake", CPUs: o.CPUs, MemoryMB: o.MemoryMB,
		OverlayPath: overlay, SeedISOPath: iso, ConsoleLog: console,
		QMPSocket: filepath.Join(bakeDir, "qmp.sock"),
		PIDFile:   filepath.Join(bakeDir, "qemu.pid"),
	})
	if err != nil {
		return err
	}
	select {
	case <-vm.Done():
	case <-time.After(o.Timeout):
		_ = vm.Kill()
		return fmt.Errorf("bake timed out after %v; console tail:\n%s", o.Timeout, consoleTail(console))
	case <-ctx.Done():
		_ = vm.Kill()
		return ctx.Err()
	}

	consoleOut, err := os.ReadFile(console)
	if err != nil {
		return fmt.Errorf("read console log: %w", err)
	}
	if !bytes.Contains(consoleOut, []byte("BAKE-OK")) {
		return fmt.Errorf("bake failed (no BAKE-OK sentinel); console tail:\n%s", lastBytes(consoleOut, 2000))
	}

	newBase := filepath.Join(imagesDir, "base.qcow2.new")
	if out, err := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2",
		overlay, newBase).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %v: %s", err, out)
	}
	if err := os.Rename(newBase, filepath.Join(imagesDir, "base.qcow2")); err != nil {
		return err
	}

	meta, err := json.MarshalIndent(map[string]string{
		"runner_version":     rel.Version,
		"cloud_image_sha256": wantSHA,
		"baked_at":           time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(imagesDir, "base.json"), append(meta, '\n'), 0o644); err != nil {
		return err
	}
	o.Log.Info("bake complete", "base", filepath.Join(imagesDir, "base.qcow2"))
	return nil
}

// consoleTail returns the last 2 KiB of the bake VM's serial console for
// embedding in an error, or a placeholder if the log can't be read. Used on
// the timeout path, where there is no consoleOut already in hand.
func consoleTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(console log unavailable: %v)", err)
	}
	return string(lastBytes(b, 2000))
}

// lastBytes returns the final n bytes of b, or all of b if it is shorter.
func lastBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

func fetchAll(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
