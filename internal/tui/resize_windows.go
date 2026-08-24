//go:build windows

package tui

import "os"

// Windows terminals do not expose SIGWINCH. Rendering still recalculates the
// terminal size for every input and agent event.
var signalNotify = func(_ chan<- os.Signal) {}
