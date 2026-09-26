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
)

func TestDownloadConditional(t *testing.T) {
	var hits atomic.Int32
	body := "vhdx-bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Wed, 16 Oct 2024 15:40:30 GMT")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "windows-base.vhdx")
	ctx := context.Background()

	cached, err := DownloadConditional(ctx, srv.Client(), srv.URL, dest)
	if err != nil || cached {
		t.Fatalf("first: cached=%v err=%v", cached, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != body {
		t.Errorf("content = %q", b)
	}
	var meta downloadMeta
	mb, err := os.ReadFile(dest + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.ETag != `"v1"` || meta.LastModified != "Wed, 16 Oct 2024 15:40:30 GMT" || meta.ContentLength != int64(len(body)) {
		t.Errorf("meta = %+v", meta)
	}

	cached, err = DownloadConditional(ctx, srv.Client(), srv.URL, dest)
	if err != nil || !cached {
		t.Fatalf("second: cached=%v err=%v", cached, err)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2 (one full, one 304)", hits.Load())
	}
}

func TestDownloadConditionalNoSidecarRefetches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			t.Error("validators sent without a sidecar")
		}
		_, _ = w.Write([]byte("new"))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(dest, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	cached, err := DownloadConditional(context.Background(), srv.Client(), srv.URL, dest)
	if err != nil || cached {
		t.Fatalf("cached=%v err=%v", cached, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "new" {
		t.Errorf("content = %q", b)
	}
}

func TestDownloadConditionalHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "f")
	if _, err := DownloadConditional(context.Background(), srv.Client(), srv.URL, dest); err == nil {
		t.Error("want error on 403")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("failed download left a file behind")
	}
}

func TestDownloadConditionalRefusesHTTPSDowngrade(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("downgraded"))
	}))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/image.vhdx", http.StatusFound)
	}))
	defer tlsSrv.Close()
	client := tlsSrv.Client() // trusts the test CA
	client.CheckRedirect = NoDowngradeRedirect

	dest := filepath.Join(t.TempDir(), "windows-base.vhdx")
	_, err := DownloadConditional(context.Background(), client, tlsSrv.URL+"/image.vhdx", dest)
	if err == nil {
		t.Fatal("want an error on an https -> http redirect")
	}
	if !strings.Contains(err.Error(), plain.URL) {
		t.Errorf("err = %v, want it to name the offending URL %s", err, plain.URL)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("refused download left a file behind")
	}
}

func TestDownloadConditionalAllowsHTTPSRedirect(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("redirected-bytes"))
	})
	mux.HandleFunc("/image.vhdx", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
	})
	srv = httptest.NewTLSServer(mux)
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = NoDowngradeRedirect

	dest := filepath.Join(t.TempDir(), "windows-base.vhdx")
	cached, err := DownloadConditional(context.Background(), client, srv.URL+"/image.vhdx", dest)
	if err != nil || cached {
		t.Fatalf("cached=%v err=%v", cached, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "redirected-bytes" {
		t.Errorf("content = %q", b)
	}
}
