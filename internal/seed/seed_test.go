package seed

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUserData(t *testing.T) {
	ud, err := UserData("JITBLOB==")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatalf("missing #cloud-config header: %q", ud[:30])
	}
	var doc struct {
		WriteFiles []struct {
			Path        string `yaml:"path"`
			Owner       string `yaml:"owner"`
			Permissions string `yaml:"permissions"`
			Content     string `yaml:"content"`
		} `yaml:"write_files"`
		RunCmd [][]string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(ud), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.WriteFiles) != 1 {
		t.Fatalf("write_files = %+v", doc.WriteFiles)
	}
	wf := doc.WriteFiles[0]
	if wf.Path != "/etc/runner-jit.conf" || wf.Owner != "runner:runner" ||
		wf.Permissions != "0600" || wf.Content != "JITBLOB==" {
		t.Errorf("write_files[0] = %+v", wf)
	}
	if len(doc.RunCmd) != 1 || len(doc.RunCmd[0]) != 1 || doc.RunCmd[0][0] != "/usr/local/bin/run-one-job" {
		t.Errorf("runcmd = %+v", doc.RunCmd)
	}
}

func TestMetaData(t *testing.T) {
	got := MetaData("ghq-fmt-ab12", "ghq-fmt-ab12")
	want := "instance-id: ghq-fmt-ab12\nlocal-hostname: ghq-fmt-ab12\n"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestBuildISO(t *testing.T) {
	if _, err := exec.LookPath("genisoimage"); err != nil {
		t.Skip("genisoimage not installed")
	}
	dir := t.TempDir()
	iso, err := BuildISO(context.Background(), dir, "#cloud-config\n{}\n", MetaData("i", "h"))
	if err != nil {
		t.Fatal(err)
	}
	if iso != filepath.Join(dir, "seed.iso") {
		t.Errorf("iso path = %q", iso)
	}
	fi, err := os.Stat(iso)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Error("seed.iso is empty")
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("seed.iso mode = %o, want 0600", fi.Mode().Perm())
	}
	// the source files must exist alongside (genisoimage read them)
	for _, f := range []string{"user-data", "meta-data"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
