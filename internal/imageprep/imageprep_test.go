package imageprep

import (
	"slices"
	"testing"

	"github.com/a1678991/github-qemu-runner/internal/config"
)

func TestPlan(t *testing.T) {
	qemu := config.Pool{Backend: "qemu", OS: "linux"}
	win := config.Pool{Backend: "qemu", OS: "windows"}
	dind := config.Pool{Backend: "docker", Isolation: "gvisor"}
	slim := config.Pool{Backend: "docker", Isolation: "seccomp"}

	cases := []struct {
		name         string
		pools        []config.Pool
		force        bool
		present      map[string]bool
		wantQEMU     bool
		wantVariants []string
		wantWindows  bool
	}{
		{"qemu present, no force", []config.Pool{qemu}, false, map[string]bool{"qemu": true}, false, nil, false},
		{"qemu absent, no force", []config.Pool{qemu}, false, map[string]bool{"qemu": false}, true, nil, false},
		{"docker slim missing", []config.Pool{dind, slim}, false, map[string]bool{"dind": true, "slim": false}, false, []string{"slim"}, false},
		{"all present, no force", []config.Pool{qemu, dind}, false, map[string]bool{"qemu": true, "dind": true}, false, nil, false},
		{"force bakes everything used", []config.Pool{qemu, dind, slim}, true, map[string]bool{}, true, []string{"dind", "slim"}, false},
		{"force overrides present", []config.Pool{qemu, dind}, true, map[string]bool{"qemu": true, "dind": true}, true, []string{"dind"}, false},
		{"windows absent", []config.Pool{win}, false, map[string]bool{"windows": false}, false, nil, true},
		{"windows present", []config.Pool{win}, false, map[string]bool{"windows": true}, false, nil, false},
		{"windows force", []config.Pool{qemu, win}, true, map[string]bool{"qemu": true, "windows": true}, true, nil, true},
		{"linux pool does not bake windows", []config.Pool{qemu}, true, map[string]bool{}, true, nil, false},
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
			if got.Windows != tc.wantWindows {
				t.Errorf("Windows = %v, want %v", got.Windows, tc.wantWindows)
			}
		})
	}
}
