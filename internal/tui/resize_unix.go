//go:build !windows

package tui

import (
	"os"
	"os/signal"
	"syscall"
)

var signalNotify = func(ch chan<- os.Signal) { signal.Notify(ch, syscall.SIGWINCH) }
