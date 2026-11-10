//go:build windows

package proc

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW from the Win32 process creation flags, which syscall does
// not define. The child runs without a console window.
const createNoWindow = 0x08000000

// Hide sets cmd to start without a console window, keeping any other process attributes.
func Hide(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
