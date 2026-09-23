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

// hiddenNamespaces are the directory trees the packaged systemd units
// (and the NixOS module) replace for the service, with the reason each
// one is unreachable. A file there is readable by the operator running
// `setup` and invisible to the running unit.
var hiddenNamespaces = []struct{ prefix, reason string }{
	{"/home/", "the systemd unit runs with ProtectHome=yes; the service cannot read files under /home"},
	{"/root/", "the systemd unit runs with ProtectHome=yes; the service cannot read files under /root"},
	{"/run/user/", "the systemd unit runs with ProtectHome=yes; the service cannot read files under /run/user"},
	{"/tmp/", "the systemd unit runs with PrivateTmp=yes; the service gets its own empty /tmp"},
	{"/var/tmp/", "the systemd unit runs with PrivateTmp=yes; the service gets its own empty /var/tmp"},
}

// hiddenNamespaceWarning describes why the unit could not read path, or
// "" when the path is in a tree the unit shares with the rest of the
// host. Lexical: the path is cleaned, not resolved, so a symlink out of
// a hidden tree still warns (the unit follows it from inside the
// namespace, where it does not exist).
func hiddenNamespaceWarning(path string) string {
	clean := filepath.Clean(path)
	for _, h := range hiddenNamespaces {
		if strings.HasPrefix(clean, h.prefix) {
			return fmt.Sprintf("%s: %s", path, h.reason)
		}
	}
	return ""
}

// CheckLocalSource preflights a local image source: the file must exist
// and be a regular file. A readable file in a tree the units replace
// (see hiddenNamespaces) still gets a warning rather than an error —
// `setup` runs as the operator and can see what the service cannot.
func CheckLocalSource(path string) (warning string, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: not a regular file", path)
	}
	return hiddenNamespaceWarning(path), nil
}
