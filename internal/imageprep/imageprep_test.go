package imageprep

import (
	"slices"
	"testing"

	"github.com/a1678991/github-qemu-runner/internal/config"
)

func TestPlan(t *testing.T) {
	qemu := config.Pool{Backend: "qemu"}
	dind := config.Pool{Backend: "docker", Isolation: "gvisor"}
	slim := config.Pool{Backend: "docker", Isolation: "seccomp"}

	cases := []struct {
		name         string
		pools        []config.Pool
		force        bool
		present      map[string]bool
		wantQEMU     bool
		wantVariants []string
	}{
		{"qemu present, no force", []config.Pool{qemu}, false, map[string]bool{"qemu": true}, false, nil},
		{"qemu absent, no force", []config.Pool{qemu}, false, map[string]bool{"qemu": false}, true, nil},
		{"docker slim missing", []config.Pool{dind, slim}, false, map[string]bool{"dind": true, "slim": false}, false, []string{"slim"}},
		{"all present, no force", []config.Pool{qemu, dind}, false, map[string]bool{"qemu": true, "dind": true}, false, nil},
		{"force bakes everything used", []config.Pool{qemu, dind, slim}, true, map[string]bool{}, true, []string{"dind", "slim"}},
		{"force overrides present", []config.Pool{qemu, dind}, true, map[string]bool{"qemu": true, "dind": true}, true, []string{"dind"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.Config{Pools: tc.pools}
			got := plan(c, tc.force, func(a string) bool { return tc.present[a] })
			if got.QEMU != tc.wantQEMU {
				t.Errorf("QEMU = %v, want %v", got.QEMU, tc.wantQEMU)
			}
			if !slices.Equal(got.Variants, tc.wantVariants) {
				t.Errorf("Variants = %v, want %v", got.Variants, tc.wantVariants)
			}
		})
	}
}
