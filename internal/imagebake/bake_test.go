package imagebake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseSHA256SUMS(t *testing.T) {
	sums := []byte(
		"aaaa1111 *noble-server-cloudimg-amd64.img\n" +
			"bbbb2222  noble-server-cloudimg-arm64.img\n")
	got, err := ParseSHA256SUMS(sums, "noble-server-cloudimg-amd64.img")
	if err != nil || got != "aaaa1111" {
		t.Errorf("got %q, %v", got, err)
	}
	got, err = ParseSHA256SUMS(sums, "noble-server-cloudimg-arm64.img")
	if err != nil || got != "bbbb2222" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := ParseSHA256SUMS(sums, "absent.img"); err == nil {
		t.Error("want error for absent file")
	}
}

func TestDownloadVerified(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()
	// sha256("hello")
	const sha = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

	dest := filepath.Join(t.TempDir(), "f.img")
	ctx := context.Background()
	if err := DownloadVerified(ctx, srv.Client(), srv.URL, dest, sha); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "hello" {
		t.Errorf("content = %q", b)
	}
	// second call: cache hit, no new request
	if err := DownloadVerified(ctx, srv.Client(), srv.URL, dest, sha); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1 (cached)", hits.Load())
	}
	// checksum mismatch must fail and leave no file
	bad := filepath.Join(t.TempDir(), "bad.img")
	if err := DownloadVerified(ctx, srv.Client(), srv.URL, bad, strings.Repeat("0", 64)); err == nil {
		t.Error("want checksum mismatch error")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("mismatched download left a file behind")
	}
}

func TestLatestRunner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/actions/runner/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.335.1",
			"body": "## SHA-256\n" +
				"- actions-runner-linux-x64-2.335.1.tar.gz " +
				"<!-- BEGIN SHA linux-x64 -->abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789<!-- END SHA linux-x64 -->",
			"assets": []map[string]any{
				{"name": "actions-runner-linux-arm64-2.335.1.tar.gz", "browser_download_url": "https://x/arm64.tar.gz"},
				{"name": "actions-runner-linux-x64-2.335.1.tar.gz", "browser_download_url": "https://x/x64.tar.gz"},
			},
		})
	}))
	defer srv.Close()
	rel, err := LatestRunner(context.Background(), srv.Client(), srv.URL, "x64")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "2.335.1" || rel.TarballURL != "https://x/x64.tar.gz" {
		t.Errorf("rel = %+v", rel)
	}
	if rel.SHA256 != "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Errorf("SHA256 = %q", rel.SHA256)
	}
}

// TestLatestRunnerArm64NoMarkers exercises the SHA-not-found fallback:
// when the release body lacks the BEGIN/END SHA markers (format drift,
// or a release that hasn't been published with them yet), SHA256 must
// come back empty so the caller falls through to the TLS-only path.
func TestLatestRunnerArm64NoMarkers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"tag_name": "v2.335.1",
			"body": "release notes with no checksum markers at all",
			"assets": [
				{"name": "actions-runner-linux-x64-2.335.1.tar.gz", "browser_download_url": "https://example.com/x64.tar.gz"},
				{"name": "actions-runner-linux-arm64-2.335.1.tar.gz", "browser_download_url": "https://example.com/arm64.tar.gz"}
			]
		}`))
	}))
	defer srv.Close()
	rel, err := LatestRunner(context.Background(), srv.Client(), srv.URL, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if rel.TarballURL != "https://example.com/arm64.tar.gz" {
		t.Errorf("TarballURL = %q, want arm64 asset", rel.TarballURL)
	}
	if rel.SHA256 != "" {
		t.Errorf("SHA256 = %q, want empty (no markers → TLS-only fallback)", rel.SHA256)
	}
}

func TestLatestRunnerSHAFromMarkers(t *testing.T) {
	// Realistic release body: the asset filename appears FIRST in a
	// per-platform install instructions block (curl + tar) long before
	// the checksum table. The naive "first 64-hex after asset name"
	// regex would grab the win-x64 SHA for every arch.
	body := "## Windows x64\n```\n" +
		"curl -O -L https://github.com/actions/runner/releases/download/v2.335.1/actions-runner-win-x64-2.335.1.zip\n" +
		"```\n" +
		"## Linux arm64\n```bash\n" +
		"curl -O -L https://github.com/actions/runner/releases/download/v2.335.1/actions-runner-linux-arm64-2.335.1.tar.gz\n" +
		"tar xzf ./actions-runner-linux-arm64-2.335.1.tar.gz\n" +
		"```\n" +
		"## Linux x64\n```bash\n" +
		"curl -O -L https://github.com/actions/runner/releases/download/v2.335.1/actions-runner-linux-x64-2.335.1.tar.gz\n" +
		"tar xzf ./actions-runner-linux-x64-2.335.1.tar.gz\n" +
		"```\n" +
		"## SHA-256 Checksums\n" +
		"- actions-runner-win-x64-2.335.1.zip <!-- BEGIN SHA win-x64 -->" + strings.Repeat("b", 64) + "<!-- END SHA win-x64 -->\n" +
		"- actions-runner-linux-x64-2.335.1.tar.gz <!-- BEGIN SHA linux-x64 -->" + strings.Repeat("c", 64) + "<!-- END SHA linux-x64 -->\n" +
		"- actions-runner-linux-arm64-2.335.1.tar.gz <!-- BEGIN SHA linux-arm64 -->" + strings.Repeat("d", 64) + "<!-- END SHA linux-arm64 -->\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.335.1",
			"body":     body,
			"assets": []map[string]any{
				{"name": "actions-runner-linux-x64-2.335.1.tar.gz", "browser_download_url": "https://example.com/x64.tar.gz"},
				{"name": "actions-runner-linux-arm64-2.335.1.tar.gz", "browser_download_url": "https://example.com/arm64.tar.gz"},
			},
		})
	}))
	defer srv.Close()

	armRel, err := LatestRunner(context.Background(), srv.Client(), srv.URL, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if armRel.SHA256 != strings.Repeat("d", 64) {
		t.Errorf("arm64 SHA256 = %q, want %q (NOT the win-x64 trap %q)",
			armRel.SHA256, strings.Repeat("d", 64), strings.Repeat("b", 64))
	}

	x64Rel, err := LatestRunner(context.Background(), srv.Client(), srv.URL, "x64")
	if err != nil {
		t.Fatal(err)
	}
	if x64Rel.SHA256 != strings.Repeat("c", 64) {
		t.Errorf("x64 SHA256 = %q, want %q", x64Rel.SHA256, strings.Repeat("c", 64))
	}
}

func TestBakeUserData(t *testing.T) {
	ud, err := BakeUserData(Release{Version: "2.335.1", TarballURL: "https://x/t.gz", SHA256: "ff"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatal("missing #cloud-config header")
	}
	var doc struct {
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
		RunCmd [][]string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(ud), &doc); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, wf := range doc.WriteFiles {
		paths[wf.Path] = wf.Content
	}
	for _, p := range []string{"/run/bake-env", "/run/bake.sh", "/run/run-one-job"} {
		if paths[p] == "" {
			t.Errorf("missing write_file %s", p)
		}
	}
	// mandated deviation: single-quote shell quoting, not double-quote %q
	if !strings.Contains(paths["/run/bake-env"], `TARBALL_URL='https://x/t.gz'`) {
		t.Errorf("bake-env = %q", paths["/run/bake-env"])
	}
	if len(doc.RunCmd) != 1 || doc.RunCmd[0][1] != "/run/bake.sh" {
		t.Errorf("runcmd = %v", doc.RunCmd)
	}
}

func TestBakeUserDataQuotesShellMetachars(t *testing.T) {
	ud, err := BakeUserData(Release{Version: "1", TarballURL: "https://x/$(rm -rf /)'", SHA256: ""})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ud, `TARBALL_URL='https://x/$(rm -rf /)'\'''`) {
		t.Errorf("metachars not single-quoted:\n%s", ud)
	}
}
