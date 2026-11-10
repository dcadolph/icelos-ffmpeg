package proc

import (
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCommand(t *testing.T) {
	t.Parallel()
	cmd := Command(t.Context(), "ffmpeg", "-hide_banner", "-encoders")
	if diff := cmp.Diff([]string{"ffmpeg", "-hide_banner", "-encoders"}, cmd.Args); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
	if runtime.GOOS != "windows" && cmd.SysProcAttr != nil {
		t.Errorf("SysProcAttr = %+v, want none outside Windows", cmd.SysProcAttr)
	}
}
