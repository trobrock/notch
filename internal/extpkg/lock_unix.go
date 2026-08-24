//go:build !windows

package extpkg

import (
	"errors"
	"syscall"
)

func processAlive(pid int) (alive, known bool) {
	if pid <= 0 {
		return false, true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM), true
}
