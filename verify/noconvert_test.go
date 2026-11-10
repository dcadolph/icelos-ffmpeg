package verify

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/icelos-ffmpeg/tooltest"
)

// Option arities for the ffmpeg command lines this package builds. An option missing from both
// tables fails the test, so a new option cannot hide an output file behind an unknown arity.
var (
	// flagOptions take no value.
	flagOptions = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-hide_banner", "-nostdin", "-nostats", "-xerror", "-noautorotate", "-copyts",
	}
	// valueOptions take one value.
	valueOptions = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-loglevel", "-v", "-i", "-map", "-vf", "-af", "-filter_complex", "-fps_mode",
		"-enc_time_base", "-c", "-c:v", "-c:a", "-f", "-hash", "-ss", "-t", "-frames:v",
	}
	// measureMuxers write only a measurement or raw decoded samples to standard output. null
	// writes nothing. None of them can hold a converted file.
	measureMuxers = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"null", "framecrc", "framemd5", "streamhash", "hash", "f32le", "s32le",
	}
	// pipeTargets are the outputs that name standard output rather than a file.
	pipeTargets = []string{"-", "pipe:", "pipe:1"} //nolint:gochecknoglobals // Not modified.
)

// argvProblems returns what is wrong with one ffmpeg command line: an output that is a file, a
// muxer that is not a measurement, an output whose muxer ffmpeg would guess, or an unknown option.
// A version query has no output and passes.
func argvProblems(argv []string) []string {
	if slices.Contains(argv, "-version") {
		return nil
	}
	var problems []string
	format, outputs := "", 0
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg != "-" && strings.HasPrefix(arg, "-") && slices.Contains(flagOptions, arg):
		case arg != "-" && strings.HasPrefix(arg, "-") && slices.Contains(valueOptions, arg):
			if i+1 >= len(argv) {
				return append(problems, fmt.Sprintf("%s has no value", arg))
			}
			i++
			switch arg {
			case "-f":
				format = argv[i]
			case "-i":
				format = ""
			}
		case arg != "-" && strings.HasPrefix(arg, "-"):
			problems = append(problems, fmt.Sprintf("unknown option %s: add it to a table", arg))
		default:
			outputs++
			if !slices.Contains(pipeTargets, arg) {
				problems = append(problems, fmt.Sprintf("output %q is a file", arg))
			}
			switch {
			case format == "":
				problems = append(problems, fmt.Sprintf("output %q has no -f, so ffmpeg guesses", arg))
			case !slices.Contains(measureMuxers, format):
				problems = append(problems, fmt.Sprintf("muxer %s can write a converted file", format))
			}
			format = ""
		}
	}
	if outputs == 0 {
		problems = append(problems, "no output found, so the line was misread")
	}
	return problems
}

// TestArgvProblems proves the command line reader flags each way a line could write a file.
func TestArgvProblems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   []string
		Want []string
	}{{ // Test 0: A hash to standard output passes.
		In: []string{"-hide_banner", "-i", "in.mp4", "-map", "0:0", "-f", "framemd5", "-"},
	}, { // Test 1: A null decode passes.
		In: []string{"-i", "in.mp4", "-f", "null", "-"},
	}, { // Test 2: A version query passes.
		In: []string{"-hide_banner", "-version"},
	}, { // Test 3: An MP4 muxer to standard output fails.
		In:   []string{"-i", "in.mp4", "-f", "mp4", "-"},
		Want: []string{"muxer mp4 can write a converted file"},
	}, { // Test 4: An output file fails, and a guessed muxer with it.
		In:   []string{"-i", "in.mp4", "out.mkv"},
		Want: []string{`output "out.mkv" is a file`, `output "out.mkv" has no -f, so ffmpeg guesses`},
	}, { // Test 5: An unknown option fails rather than swallowing what follows.
		In:   []string{"-i", "in.mp4", "-y", "-f", "null", "-"},
		Want: []string{"unknown option -y: add it to a table"},
	}, { // Test 6: An input format does not count as the output's muxer.
		In:   []string{"-f", "lavfi", "-i", "testsrc2", "pipe:1"},
		Want: []string{`output "pipe:1" has no -f, so ffmpeg guesses`},
	}, { // Test 7: A line with no output is misread.
		In:   []string{"-i", "in.mp4", "-f", "null"},
		Want: []string{"no output found, so the line was misread"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.Want, argvProblems(test.In), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// recordingFFmpeg writes an ffmpeg stand-in into dir that saves each command line it gets to its
// own file in dir/calls, NUL-separated, then runs the real ffmpeg. It returns the stand-in's path
// and the calls folder.
func recordingFFmpeg(t *testing.T, dir string) (string, string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	if err := os.Mkdir(calls, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ffmpeg")
	tooltest.WriteScript(t, path, fmt.Sprintf(`#!/bin/sh
f="$(mktemp '%s/call.XXXXXX')" || exit 99
for a in "$@"; do printf '%%s\000' "$a"; done >"$f"
exec '%s' "$@"
`, calls, ffmpeg))
	return path, calls
}

// recordedCalls reads every command line the stand-in saved.
func recordedCalls(t *testing.T, calls string) [][]string {
	t.Helper()
	entries, err := os.ReadDir(calls)
	if err != nil {
		t.Fatal(err)
	}
	var lines [][]string
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(calls, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"))
	}
	return lines
}

// TestNoOutputFile proves the verifier never converts: it runs Verify over fixtures that reach
// every proof, through an ffmpeg that records its command lines, and fails if any line names an
// output file or a muxer that could hold a converted file. Every output must go to standard output
// through null or a hash or raw sample muxer.
func TestNoOutputFile(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	box := Caption{
		At: 700 * time.Millisecond, Clear: 200 * time.Millisecond, X: 0.2, Y: 0.85, W: 0.6, H: 0.13,
	}
	drawn := "drawbox=x=iw*0.3:y=ih*0.88:w=iw*0.4:h=ih*0.06:color=white:t=fill:" +
		"enable='between(t,0.5,0.9)'"
	tests := []struct {
		Name   string
		Source func(t *testing.T, dir string) (string, Expect)
		Out    func(src string) []string
		File   string
	}{{ // Test 0: A remux reaches the packet and frame hashes.
		Name: "copy", File: "out.mkv",
		Source: func(t *testing.T, dir string) (string, Expect) {
			return makeClip(t, dir, "src.mp4", plainSource...), Expect{
				Streams: []string{"video:h264", "audio:aac"}, VideoCopied: true, AudioCopied: true,
			}
		},
		Out: func(src string) []string { return []string{"-i", src, "-c", "copy"} },
	}, { // Test 1: A lossless copy reaches the frame and sample hashes.
		Name: "lossless", File: "out.mkv",
		Source: func(t *testing.T, dir string) (string, Expect) {
			return makeClip(t, dir, "src.mkv", flacSource...), Expect{
				LosslessVideo: true, LosslessAudio: true,
			}
		},
		Out: func(src string) []string { return []string{"-i", src, "-c", "copy"} },
	}, { // Test 2: An encode of generated markers reaches the picture, sound, and sync proofs.
		Name: "markers", File: "out.mp4",
		Source: func(t *testing.T, dir string) (string, Expect) {
			src, truth := simSource(t, "markers", dir)
			return src, Expect{Markers: truth.Markers, MinSSIM: 0.5, PixFmt: "yuv420p"}
		},
		Out: func(src string) []string {
			return append([]string{"-i", src}, append(x264[:len(x264):len(x264)], "-c:a", "aac")...)
		},
	}, { // Test 3: Generated tones reach the sound score and the channel tones.
		Name: "tones", File: "out.m4a",
		Source: func(t *testing.T, dir string) (string, Expect) {
			src, truth := simSource(t, "tones-stereo", dir)
			return src, Expect{
				Tones: [][]float64{truth.Audio[0].Tones}, MinSDR: 1, Channels: 2,
				SampleRate: truth.Audio[0].SampleRate,
			}
		},
		Out: func(src string) []string {
			return []string{"-i", src, "-vn", "-c:a", "aac", "-b:a", "512k"}
		},
	}, { // Test 4: A drawn caption reaches the burned caption proof.
		Name: "caption", File: "out.mp4",
		Source: func(t *testing.T, dir string) (string, Expect) {
			src, _ := simSource(t, "subs", dir)
			return src, Expect{Caption: &box}
		},
		Out: func(src string) []string {
			return append([]string{"-i", src, "-map", "0:v", "-vf", drawn}, x264...)
		},
	}, { // Test 5: Gradients reach the combing proof.
		Name: "comb", File: "out.mp4",
		Source: func(t *testing.T, dir string) (string, Expect) {
			return makeClip(t, dir, "src.mp4", gradientSource...), Expect{CombProof: true}
		},
		Out: func(src string) []string { return append([]string{"-i", src}, x264...) },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, expect := test.Source(t, dir)
			out := makeClip(t, dir, test.File, test.Out(src)...)
			bin, calls := recordingFFmpeg(t, t.TempDir())
			before := listing(t, dir)
			got, err := Verify(t.Context(), Input{
				Source: src, Output: out, Expect: expect, Tools: Tools{FFmpeg: bin},
			})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(before, listing(t, dir)); diff != "" {
				t.Errorf("Verify changed the clip folder (-before +after):\n%s", diff)
			}
			lines := recordedCalls(t, calls)
			if len(lines) < 2 {
				t.Fatalf("recorded %d ffmpeg calls, want the version query and the proofs", len(lines))
			}
			for _, argv := range lines {
				for _, problem := range argvProblems(argv) {
					t.Errorf("%s: ffmpeg %s", problem, strings.Join(argv, " "))
				}
			}
			if got.Schema != SchemaVersion {
				t.Errorf("Schema = %d, want %d", got.Schema, SchemaVersion)
			}
			if diff := cmp.Diff(expect, got.Expect, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("receipt Expect mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// listing returns each file in dir with its size, so a test can see a file appear or change.
func listing(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %d\n", entry.Name(), info.Size())
	}
	return b.String()
}
