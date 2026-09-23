package imagebake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// downloadMeta is the validator sidecar next to a conditionally
// downloaded file (<dest>.meta).
type downloadMeta struct {
	ETag          string `json:"etag"`
	LastModified  string `json:"last_modified"`
	ContentLength int64  `json:"content_length"`
}

// DownloadConditional fetches url to dest unless the server reports it
// unchanged (304) against the validators saved in <dest>.meta. For large
// upstream files with no published checksum (the Windows evaluation
// VHDX). Returns cached=true when the existing file was kept. A failed
// fetch leaves dest as it was.
func DownloadConditional(ctx context.Context, client *http.Client, url, dest string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	metaPath := dest + ".meta"
	if _, statErr := os.Stat(dest); statErr == nil {
		if mb, err := os.ReadFile(metaPath); err == nil {
			var meta downloadMeta
			if json.Unmarshal(mb, &meta) == nil {
				if meta.ETag != "" {
					req.Header.Set("If-None-Match", meta.ETag)
				}
				if meta.LastModified != "" {
					req.Header.Set("If-Modified-Since", meta.LastModified)
				}
			}
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified {
		return true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	meta := downloadMeta{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"), ContentLength: n}
	mb, err := json.Marshal(meta)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(metaPath, mb, 0o644); err != nil && !errors.Is(err, os.ErrPermission) {
		return false, err
	}
	return false, nil
}
