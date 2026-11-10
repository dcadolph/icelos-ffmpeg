// Package tooltest fails a test when a command-line tool it needs is not installed, so the tests
// that drive the real ffmpeg run or fail and never skip, and writes the fake tools other tests
// run in its place.
package tooltest

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// Require fails the test through t.Fatalf, naming the first tool in names that is not on PATH.
func Require(t testing.TB, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("%s is not on PATH; the real-ffmpeg tests run or fail, never skip", name)
			return
		}
	}
}

// WriteScript writes body to path as an executable script. It holds syscall.ForkLock while the
// file is open, so a process that a parallel test starts at that moment cannot inherit the write
// handle, which on Linux makes running the script fail with "text file busy".
func WriteScript(t testing.TB, path, body string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // Test script.
		t.Fatalf("write script: %v", err)
	}
}

// SmallDisk mounts a file system of the given size in a temporary folder and returns the folder,
// so a test can fill a disk without filling the real one. It unmounts the folder when the test ends.
func SmallDisk(t testing.TB, megabytes int) string {
	t.Helper()
	dir := t.TempDir()
	run := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if runtime.GOOS == "darwin" {
		ram := fmt.Sprintf("ram://%d", megabytes*2048)
		// hdiutil writes deprecation warnings to stderr, so the device comes from stdout alone.
		out, err := exec.Command("hdiutil", "attach", "-nomount", ram).Output()
		fields := strings.Fields(string(out))
		if err != nil || len(fields) == 0 {
			t.Fatalf("hdiutil attach -nomount %s: %v: %q", ram, err, out)
		}
		dev := fields[0]
		t.Cleanup(func() {
			if out, err := exec.Command("hdiutil", "detach", dev, "-force").CombinedOutput(); err != nil {
				t.Errorf("detach %s: %v: %s", dev, err, out)
			}
		})
		run("newfs_hfs", dev)
		run("diskutil", "quiet", "mount", "-mountPoint", dir, dev)
		run("chmod", "700", dir)
		return dir
	}
	run("sudo", "mount", "-t", "tmpfs", "-o", fmt.Sprintf("size=%dm,mode=0777", megabytes), "tmpfs", dir)
	t.Cleanup(func() {
		if out, err := exec.Command("sudo", "umount", dir).CombinedOutput(); err != nil {
			t.Errorf("unmount %s: %v: %s", dir, err, out)
		}
	})
	return dir
}
