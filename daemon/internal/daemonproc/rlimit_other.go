//go:build !unix

package daemonproc

// raiseFileLimit is a no-op away from Unix.
//
// Windows has no RLIMIT_NOFILE. Handle count is bounded by the per-process handle table, which is large
// enough that nothing analogous needs raising, so there is no equivalent knob to turn — reporting 0 lets
// the caller log "not applicable" rather than inventing a number.
func raiseFileLimit() (uint64, error) { return 0, nil }
