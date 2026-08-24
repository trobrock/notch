//go:build windows

package tui

import (
	"os"
	"time"
)

func interruptTerminalRead(file *os.File) (bool, error) {
	if file == nil {
		return false, nil
	}
	return false, file.SetReadDeadline(time.Now())
}

func restoreTerminalRead(file *os.File, _ bool) error {
	if file == nil {
		return nil
	}
	return file.SetReadDeadline(time.Time{})
}
