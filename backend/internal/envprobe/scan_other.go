//go:build !windows

package envprobe

import "runtime"

// scan on a non-Windows host does what it can: PATH only.
//
// The catalogue's install patterns are Windows paths because that is where the
// product ships first. Reporting an empty inventory and letting Probe return
// the catalogue's Missing list is the honest answer — claiming a Linux box has
// "nothing installed" would be a guess, so Platform is reported and the caller
// can say so.
func scan() scanResult {
	return scanResult{Platform: runtime.GOOS, PathExes: pathExes()}
}
