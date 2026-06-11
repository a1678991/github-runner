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
				"actions-runner-linux-x64-2.335.1.tar.gz " +
				"<!-- BEGIN SHA --> abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789 <!-- END -->",
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

func TestLatestRunnerArm64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"tag_name": "v2.335.1",
			"body": "actions-runner-linux-arm64-2.335.1.tar.gz aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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
	if rel.SHA256 != strings.Repeat("a", 64) {
		t.Errorf("SHA256 = %q", rel.SHA256)
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
