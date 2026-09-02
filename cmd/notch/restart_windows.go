//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// restartSelf starts the newly installed binary. The caller exits the current
// process after a successful return.
func restartSelf() (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("locate updated executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	command := exec.Command(executable, os.Args[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = os.Environ()
	if err := command.Start(); err != nil {
		return false, fmt.Errorf("start updated Notch: %w", err)
	}
	_ = command.Process.Release()
	return true, nil
}
