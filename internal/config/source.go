package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IsLocalSource reports whether a windows.image / windows.virtio_win
// value names a file on this host rather than something to download.
// Validation has already rejected everything that is neither an http(s)
// URL nor an absolute path, so "absolute" is the whole test; the bake,
// the controller preflight and `setup` share this one definition.
func IsLocalSource(s string) bool {
	return filepath.IsAbs(s)
}

// validateSource accepts an http(s) URL or an absolute path for key.
func validateSource(key, val string) error {
	switch {
	case strings.HasPrefix(val, "http://"), strings.HasPrefix(val, "https://"):
		return nil
	case strings.HasPrefix(val, "file://"):
		// A file:// URL is neither fetched nor opened; say so instead of
		// failing later with a confusing "no such host".
		return fmt.Errorf("%s: use a plain absolute path, not a file:// URL", key)
	case IsLocalSource(val):
		return nil
	}
	return fmt.Errorf("%s must be an http(s) URL or an absolute path", key)
}

// CheckLocalSource preflights a local image source: the file must exist
// and be a regular file. A readable file under /home still gets a
// warning — the systemd units run with ProtectHome=yes, so the service
// sees an empty /home even though the operator running `setup` does not.
func CheckLocalSource(path string) (warning string, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: not a regular file", path)
	}
	if strings.HasPrefix(path, "/home/") {
		return fmt.Sprintf("%s: the systemd unit runs with ProtectHome=yes; "+
			"the service cannot read files under /home", path), nil
	}
	return "", nil
}
