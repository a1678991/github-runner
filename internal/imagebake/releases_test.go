package imagebake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLatestGitForWindows(t *testing.T) {
	sha := strings.Repeat("d", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/git-for-windows/git/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.55.0.windows.5",
			"body": "Filename | SHA-256\n-------- | -------\n" +
				"Git-2.55.0.5-64-bit.exe | " + sha + "\n" +
				"Git-2.55.0.5-arm64.exe | " + strings.Repeat("a", 64) + "\n",
			"assets": []map[string]any{
				{"name": "Git-2.55.0.5-arm64.exe", "browser_download_url": "https://x/arm64.exe"},
				{"name": "Git-2.55.0.5-64-bit.exe", "browser_download_url": "https://x/64.exe"},
				{"name": "PortableGit-2.55.0.5-64-bit.7z.exe", "browser_download_url": "https://x/p.exe"},
			},
		})
	}))
	defer srv.Close()
	rel, err := LatestGitForWindows(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "2.55.0.windows.5" || rel.TarballURL != "https://x/64.exe" || rel.SHA256 != sha {
		t.Errorf("rel = %+v", rel)
	}
}

func TestLatestGitForWindowsNoSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.55.0.windows.5",
			"body":     "no table",
			"assets": []map[string]any{
				{"name": "Git-2.55.0.5-64-bit.exe", "browser_download_url": "https://x/64.exe"},
			},
		})
	}))
	defer srv.Close()
	rel, err := LatestGitForWindows(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel.SHA256 != "" {
		t.Errorf("SHA256 = %q, want empty", rel.SHA256)
	}
}

func TestLatestGitForWindowsMissingAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1", "body": "", "assets": []map[string]any{}})
	}))
	defer srv.Close()
	if _, err := LatestGitForWindows(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Error("want error when no 64-bit installer asset")
	}
}
