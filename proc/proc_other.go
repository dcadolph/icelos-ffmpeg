//go:build !windows

package proc

import "os/exec"

// Hide does nothing outside Windows, where child processes have no console window to hide.
func Hide(*exec.Cmd) {}
