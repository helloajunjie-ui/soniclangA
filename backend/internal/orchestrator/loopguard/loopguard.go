// Package loopguard implements the four-signal fuse (repeated calls / consecutive
// failures / no-op / no plan progress) with soft→hard escalation (DESIGN §6.7).
// M2 ships the step-limit and naive repetition detection; full soft→hard
// escalation is stubbed for later tuning.
package loopguard

// State is the watchdog state across loop iterations.
type State struct {
	Steps             int
	LastToolSignature string
	Repeated          int
}

// Check returns whether the loop must hard-stop and why.
func Check(s *State) (stop bool, reason string) {
	if s.Steps > 50 {
		return true, "exceeded max steps"
	}
	if s.Repeated >= 3 {
		return true, "repeated identical tool calls detected"
	}
	return false, ""
}
