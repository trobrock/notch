//go:build windows

package subagent

import "os/exec"

func setProcessGroup(*exec.Cmd)      {}
func killProcessGroup(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
