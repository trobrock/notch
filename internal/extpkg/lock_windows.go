//go:build windows

package extpkg

// Windows does not expose the Unix signal-0 liveness probe through os.Process.
// The timestamp-based stale lock recovery remains available there.
func processAlive(pid int) (alive, known bool) { return false, false }
