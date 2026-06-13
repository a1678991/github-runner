package dockerbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBake(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"tag_name": "v2.335.1",
			"body": "",
			"assets": [
				{"name": "actions-runner-linux-x64-2.335.1.tar.gz", "browser_download_url": "https://example.com/x64.tar.gz"},
				{"name": "actions-runner-linux-arm64-2.335.1.tar.gz", "browser_download_url": "https://example.com/arm64.tar.gz"}
			]
		}`))
	}))
	defer api.Close()

	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	dockerBin := filepath.Join(dir, "docker")
	// The fake records argv and captures the build context directory
	// (last argument) so the test can inspect what would be built.
	script := `#!/bin/sh
echo "$@" >> ` + argvLog + `
for last; do :; done
cp -r "$last" ` + filepath.Join(dir, "context") + `
exit 0
`
	if err := os.WriteFile(dockerBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(dir, "state")
	err := Bake(context.Background(), BakeOptions{
		StateDir:  stateDir,
		HTTP:      api.Client(),
		APIBase:   api.URL,
		DockerBin: dockerBin,
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	argv, _ := os.ReadFile(argvLog)
	for _, want := range []string{
		"build --pull",
		"--target dind",
		"--build-arg RUNNER_VERSION=2.335.1",
		"--tag " + Image,
	} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("docker build argv missing %q:\n%s", want, argv)
		}
	}
	if strings.Contains(string(argv), "--target slim") {
		t.Error("default bake must build only the dind variant")
	}
	for _, f := range []string{"Dockerfile", "entrypoint.sh", "entrypoint-slim.sh"} {
		if _, err := os.Stat(filepath.Join(dir, "context", f)); err != nil {
			t.Errorf("build context missing %s: %v", f, err)
		}
	}

	var meta map[string]any
	b, err := os.ReadFile(filepath.Join(stateDir, "images", "docker-base.json"))
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["runner_version"] != "2.335.1" {
		t.Errorf("provenance runner_version = %q", meta["runner_version"])
	}
	if got := fmt.Sprint(meta["variants"]); got != "[dind]" {
		t.Errorf("provenance variants = %s, want [dind]", got)
	}
}

func TestBakeBothVariants(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"tag_name": "v2.335.1",
			"body": "",
			"assets": [
				{"name": "actions-runner-linux-x64-2.335.1.tar.gz", "browser_download_url": "https://example.com/x64.tar.gz"},
				{"name": "actions-runner-linux-arm64-2.335.1.tar.gz", "browser_download_url": "https://example.com/arm64.tar.gz"}
			]
		}`))
	}))
	defer api.Close()

	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	dockerBin := filepath.Join(dir, "docker")
	script := `#!/bin/sh
echo "$@" >> ` + argvLog + `
exit 0
`
	if err := os.WriteFile(dockerBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(dir, "state")
	err := Bake(context.Background(), BakeOptions{
		StateDir:  stateDir,
		HTTP:      api.Client(),
		APIBase:   api.URL,
		DockerBin: dockerBin,
		Variants:  []string{"dind", "slim"},
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	argv, _ := os.ReadFile(argvLog)
	for _, want := range []string{
		"--target dind",
		"--tag " + Image,
		"--target slim",
		"--tag " + SlimImage,
	} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("docker build argv missing %q:\n%s", want, argv)
		}
	}

	var meta map[string]any
	b, err := os.ReadFile(filepath.Join(stateDir, "images", "docker-base.json"))
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(meta["variants"]); got != "[dind slim]" {
		t.Errorf("provenance variants = %s, want [dind slim]", got)
	}
}

func TestBakeRejectsUnknownVariant(t *testing.T) {
	err := Bake(context.Background(), BakeOptions{
		StateDir:  t.TempDir(),
		DockerBin: "/bin/false",
		Variants:  []string{"fat"},
		Log:       slog.New(slog.DiscardHandler),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown image variant") {
		t.Fatalf("want unknown-variant error, got %v", err)
	}
}
