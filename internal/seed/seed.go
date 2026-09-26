// Package seed renders cloud-init NoCloud data and packs it into the seed
// ISO each ephemeral VM boots with.
package seed

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// UserData renders the per-VM cloud-config: write the JIT blob where the
// baked run-one-job script expects it, then invoke that script. Everything
// else (runner install, user, Docker) is baked into the base image.
func UserData(jitConfig string) (string, error) {
	doc := map[string]any{
		"write_files": []map[string]any{{
			"path":        "/etc/runner-jit.conf",
			"owner":       "runner:runner",
			"permissions": "0600",
			"content":     jitConfig,
		}},
		"runcmd": [][]string{{"/usr/local/bin/run-one-job"}},
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(b), nil
}

// MetaData gives each VM a unique instance-id so cloud-init treats every
// boot as a first boot of a new instance.
func MetaData(instanceID, hostname string) string {
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, hostname)
}

// BuildISO writes user-data/meta-data into dir and packs them into
// dir/seed.iso with the volume label cloud-init's NoCloud datasource looks
// for. Requires genisoimage on PATH.
func BuildISO(ctx context.Context, dir, userData, metaData string) (string, error) {
	return BuildISOFiles(ctx, dir, "cidata", map[string]string{
		"user-data": userData,
		"meta-data": metaData,
	})
}

// BuildISOFiles writes files (name -> content) into dir and packs them
// into dir/seed.iso with the given volume label. Joliet keeps the exact
// (mixed-case) names, which Windows needs for Unattend.xml; Rock Ridge
// does the same for Linux.
func BuildISOFiles(ctx context.Context, dir, volid string, files map[string]string) (string, error) {
	names := make([]string, 0, len(files))
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return "", err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	iso := filepath.Join(dir, "seed.iso")
	cmd := exec.CommandContext(ctx, "genisoimage",
		append([]string{"-output", "seed.iso", "-volid", volid, "-joliet", "-rock"}, names...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("genisoimage: %v: %s", err, out)
	}
	// The ISO may carry the JIT config; match the source files' 0600.
	if err := os.Chmod(iso, 0o600); err != nil {
		return "", err
	}
	return iso, nil
}
