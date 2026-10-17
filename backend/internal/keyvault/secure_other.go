//go:build !windows

package keyvault

import "os"

// hardenPath is a no-op outside Windows: the 0700/0600 modes passed to MkdirAll
// and WriteFile are honoured on these platforms, so the vault is already
// owner-only. The chmod is still issued to fix up a directory that was created
// earlier with a looser mode.
func hardenPath(path string, isDir bool) error {
	mode := os.FileMode(0o600)
	if isDir {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return nil
}

// warnHardening logs nothing here; the non-Windows path either succeeds or the
// caller treats it as a startup error, so there is no silent-degradation mode
// to report.
func warnHardening(string, error) {}
