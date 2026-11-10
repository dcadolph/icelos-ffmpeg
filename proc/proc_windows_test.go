//go:build windows

package proc

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestHideWindows(t *testing.T) {
	t.Parallel()
	cmd := Command(t.Context(), "cmd.exe", "/c", "exit", "0")
	attr := cmd.SysProcAttr
	if attr == nil || !attr.HideWindow || attr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("SysProcAttr = %+v, want a hidden window and CREATE_NO_WINDOW", attr)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("hidden child failed: %v", err)
	}
	kept := exec.Command("cmd.exe")
	kept.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	Hide(kept)
	if kept.SysProcAttr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("Hide dropped an existing creation flag")
	}
}
