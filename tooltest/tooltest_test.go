package tooltest

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// recorder is a testing.TB stand-in that records a Fatalf call instead of ending the test.
type recorder struct {
	// TB satisfies the interface's unexported method and is never called.
	testing.TB
	// Calls counts the Fatalf calls.
	Calls int
	// Message is the formatted text of the last Fatalf call.
	Message string
}

// Helper does nothing, since the recorder has no call stack to trim.
func (r *recorder) Helper() {}

// Fatalf records the formatted message.
func (r *recorder) Fatalf(format string, args ...any) {
	r.Calls++
	r.Message = fmt.Sprintf(format, args...)
}

func TestRequire(t *testing.T) {
	t.Parallel()
	const missing = "icelos-no-such-tool"
	tests := []struct {
		WantMessage string
		Names       []string
		WantCalls   int
	}{{ // Test 0: No tools asked for.
		Names: nil, WantCalls: 0, WantMessage: "",
	}, { // Test 1: A tool that go test puts on PATH.
		Names: []string{"go"}, WantCalls: 0, WantMessage: "",
	}, { // Test 2: A missing tool after a present one.
		Names: []string{"go", missing}, WantCalls: 1,
		WantMessage: missing + " is not on PATH; the real-ffmpeg tests run or fail, never skip",
	}, { // Test 3: Only the first missing tool is named.
		Names: []string{missing, missing + "-two"}, WantCalls: 1,
		WantMessage: missing + " is not on PATH; the real-ffmpeg tests run or fail, never skip",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &recorder{}
			Require(r, test.Names...)
			if diff := cmp.Diff(test.WantCalls, r.Calls); diff != "" {
				t.Errorf("Fatalf calls mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantMessage, r.Message); diff != "" {
				t.Errorf("message mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWriteScript(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantOutput string
		Workers    int
		Runs       int
	}{{ // Test 0: One script runs and prints its output.
		Workers: 1, Runs: 1, WantOutput: "ran\n",
	}, { // Test 1: Scripts written and run at once from many goroutines never hit a busy file.
		Workers: 8, Runs: 40, WantOutput: "ran\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var wg sync.WaitGroup
			errs := make(chan string, test.Workers*test.Runs)
			for w := range test.Workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range test.Runs {
						path := filepath.Join(dir, fmt.Sprintf("tool-%d", w))
						WriteScript(t, path, "#!/bin/sh\necho ran\n")
						out, err := exec.Command(path).Output()
						if err != nil || string(out) != test.WantOutput {
							errs <- fmt.Sprintf("%s: %v: %q", filepath.Base(path), err, out)
						}
					}
				}()
			}
			wg.Wait()
			close(errs)
			var failed []string
			for e := range errs {
				failed = append(failed, e)
			}
			if diff := cmp.Diff("", strings.Join(failed, "\n")); diff != "" {
				t.Errorf("script runs failed (-want +got):\n%s", diff)
			}
		})
	}
}
