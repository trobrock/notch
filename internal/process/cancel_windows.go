//go:build windows

package process

import "os/exec"

func configureCancellation(*exec.Cmd) {}
