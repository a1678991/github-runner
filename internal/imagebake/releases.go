package imagebake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// ghRelease is the slice of GitHub's release object the bakes need.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatestRelease(ctx context.Context, client *http.Client, apiBase, repo string) (ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(apiBase, "/")+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return ghRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return ghRelease{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ghRelease{}, fmt.Errorf("%s releases/latest: %s", repo, resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return ghRelease{}, err
	}
	return rel, nil
}

var gitForWindowsAssetRe = regexp.MustCompile(`^Git-[0-9][0-9.]*-64-bit\.exe$`)

// LatestGitForWindows resolves the newest Git for Windows 64-bit
// installer. The SHA-256 comes from the "<asset> | <sha>" table in the
// release notes; when the table is absent SHA256 is empty and the caller
// proceeds on TLS alone.
func LatestGitForWindows(ctx context.Context, client *http.Client, apiBase string) (Release, error) {
	rel, err := fetchLatestRelease(ctx, client, apiBase, "git-for-windows/git")
	if err != nil {
		return Release{}, err
	}
	out := Release{Version: strings.TrimPrefix(rel.TagName, "v")}
	var name string
	for _, a := range rel.Assets {
		if gitForWindowsAssetRe.MatchString(a.Name) {
			name, out.TarballURL = a.Name, a.BrowserDownloadURL
			break
		}
	}
	if out.TarballURL == "" {
		return Release{}, fmt.Errorf("no Git-*-64-bit.exe asset in git-for-windows release %s", rel.TagName)
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*\|\s*([0-9a-fA-F]{64})\s*$`)
	if m := re.FindStringSubmatch(rel.Body); m != nil {
		out.SHA256 = strings.ToLower(m[1])
	}
	return out, nil
}
