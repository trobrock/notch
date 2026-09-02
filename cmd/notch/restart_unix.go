//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// restartSelf replaces the current process with the newly installed binary.
// A successful call does not return.
func restartSelf() (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("locate updated executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	if err := syscall.Exec(executable, os.Args, os.Environ()); err != nil {
		return false, fmt.Errorf("start updated Notch: %w", err)
	}
	return true, nil
}
