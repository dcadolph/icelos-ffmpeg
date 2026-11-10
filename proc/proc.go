// Package proc starts child processes without a console window on Windows, so neither a GUI
// app that runs the converter nor the converter's own ffmpeg and ffprobe runs flash a terminal.
package proc

import (
	"context"
	"os/exec"
)

// Command returns exec.CommandContext(ctx, name, args...) set to start without a console window
// on Windows.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	Hide(cmd)
	return cmd
}
