//go:build !windows

package tui

import (
	"errors"
	"os"
	"syscall"
	"time"
)

func interruptTerminalRead(file *os.File) (bool, error) {
	if file == nil {
		return false, nil
	}
	if err := file.SetReadDeadline(time.Now()); err == nil {
		return false, nil
	}
	if err := syscall.SetNonblock(int(file.Fd()), true); err != nil {
		return false, err
	}
	return true, nil
}

func restoreTerminalRead(file *os.File, nonblocking bool) error {
	if file == nil {
		return nil
	}
	deadlineErr := file.SetReadDeadline(time.Time{})
	var nonblockErr error
	if nonblocking {
		nonblockErr = syscall.SetNonblock(int(file.Fd()), false)
	}
	return errors.Join(deadlineErr, nonblockErr)
}
