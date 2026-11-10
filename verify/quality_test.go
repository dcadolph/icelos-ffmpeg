package verify

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/icelos-ffmpeg/sim"
	"github.com/dcadolph/icelos-ffmpeg/tooltest"
)

// simSource generates the named recipe in dir and returns its path and truth.
func simSource(t *testing.T, name, dir string) (string, sim.Truth) {
	t.Helper()
	recipe, ok := sim.Find(name)
	if !ok {
		t.Fatalf("no recipe %s", name)
	}
	path, truth, err := sim.Generate(context.Background(), recipe, dir)
	if err != nil {
		t.Fatal(err)
	}
	return path, truth
}

// checkByName returns the first check called name.
func checkByName(t *testing.T, r Receipt, name string) Check {
	t.Helper()
	i := slices.IndexFunc(r.Checks, func(c Check) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("no %s check in %v, notes %v", name, checkNames(r.Checks), noteNames(r.Unchecked))
	}
	return r.Checks[i]
}

// mustFloor returns a table row's floor.
func mustFloor(t *testing.T, lookup func(codec, level string) (float64, bool), codec, level string) float64 {
	t.Helper()
	floor, ok := lookup(codec, level)
	if !ok {
		t.Fatalf("no floor for %s at %s", codec, level)
	}
	return floor
}

// TestPicture proves the SSIM check against the table's web floor. The picture source's flash and
// test pattern hold a lot of detail, so a coarse quantizer falls far below the floor.
func TestPicture(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	floor := mustFloor(t, PictureFloor, "h264", "balanced")
	tests := []struct {
		Name     string
		Out      []string
		Expect   Expect
		WantPass bool
		WantNote bool
		WantNone bool
	}{{ // Test 0: The web preset's quality clears the floor.
		Name: "web quality", Out: []string{"-c:v", "libx264", "-crf", "23"},
		Expect: Expect{MinSSIM: floor}, WantPass: true,
	}, { // Test 1: A coarse quantizer falls below the floor.
		Name: "crf 45", Out: []string{"-c:v", "libx264", "-crf", "45"},
		Expect: Expect{MinSSIM: floor},
	}, { // Test 2: A scaled picture scores against the source at the output's size.
		Name: "scaled", Out: []string{"-vf", "scale=320:180", "-c:v", "libx264", "-crf", "18"},
		Expect: Expect{MinSSIM: floor, Scaled: true}, WantPass: true,
	}, { // Test 3: An encode with no floor says so.
		Name: "no floor", Out: []string{"-c:v", "libx264", "-crf", "23"}, WantNote: true,
	}, { // Test 4: A copy has packets to compare and no score.
		Name: "copied", Out: []string{"-c:v", "copy"},
		Expect: Expect{MinSSIM: floor, VideoCopied: true}, WantNone: true,
	}, { // Test 5: A lossless encode has frames to compare and no score.
		Name: "lossless", Out: []string{"-c:v", "ffv1"},
		Expect: Expect{MinSSIM: floor, LosslessVideo: true}, WantNone: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, _ := simSource(t, "markers", dir)
			out := makeClip(t, dir, "out.mkv", append([]string{"-i", src, "-an"}, test.Out...)...)
			got, err := Verify(t.Context(), Input{Source: src, Output: out, Expect: test.Expect})
			if err != nil {
				t.Fatal(err)
			}
			hasCheck := slices.Contains(checkNames(got.Checks), CheckPicture)
			hasNote := slices.Contains(noteNames(got.Unchecked), CheckPicture)
			switch {
			case test.WantNone:
				if hasCheck || hasNote {
					t.Errorf("picture proof ran on a stream with nothing to score: %+v", got)
				}
			case test.WantNote:
				if hasCheck || !hasNote {
					t.Errorf("want a picture note, got checks %v", checkNames(got.Checks))
				}
			default:
				c := checkByName(t, got, CheckPicture)
				t.Logf("%+v", c)
				if c.Pass != test.WantPass {
					t.Errorf("pass = %v, want %v: %s", c.Pass, test.WantPass, c.Measured)
				}
				if c.Score <= 0 || c.Score > 1 {
					t.Errorf("score %v outside SSIM's range", c.Score)
				}
			}
		})
	}
}

// TestPicturePairsFrames proves the score pairs frames one for one. A lossless copy at exact times
// scores as lossless against a source whose times round to the millisecond, an output at a new rate
// or missing its repeats scores against the source at that rate, and a picture one frame late at the
// same count scores low.
func TestPicturePairsFrames(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src, _ := simSource(t, "markers", dir)
	exact := makeClip(t, dir, "exact.avi", "-i", src, "-an", "-c:v", "ffv1", "-fps_mode", "cfr", "-r", "24")
	faster := makeClip(t, dir, "faster.mkv", "-i", src, "-an", "-c:v", "ffv1", "-fps_mode", "cfr", "-r", "30")
	repeats := makeClip(t, dir, "repeats.mkv", "-i", src, "-an", "-c:v", "ffv1", "-vf",
		"fps=24,mpdecimate", "-fps_mode", "vfr")
	late := makeClip(t, dir, "late.mkv", "-i", src, "-an", "-c:v", "ffv1", "-vf",
		"trim=start_frame=1,setpts=PTS-STARTPTS,tpad=stop_mode=clone:stop=1")
	tests := []struct {
		Name     string
		Output   string
		Rate     float64
		Floor    float64
		WantPass bool
	}{{ // Test 0: Exact times against rounded ones.
		Name: "exact", Output: exact, Floor: 0.999, WantPass: true,
	}, { // Test 1: A new frame rate pairs against the source at that rate.
		Name: "new rate", Output: faster, Rate: 30, Floor: 0.98, WantPass: true,
	}, { // Test 2: A file that skips repeats holds fewer frames than the source.
		Name: "repeats", Output: repeats, Rate: 24, Floor: 0.98, WantPass: true,
	}, { // Test 3: Every frame is the next frame, so the count alone cannot catch it.
		Name: "late", Output: late, Floor: 0.999, WantPass: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := Verify(t.Context(), Input{
				Source: src, Output: test.Output, Expect: Expect{MinSSIM: test.Floor, Rate: test.Rate},
			})
			if err != nil {
				t.Fatal(err)
			}
			if c := checkByName(t, got, CheckPicture); c.Pass != test.WantPass {
				t.Errorf("picture pass = %v, want %v: %+v", c.Pass, test.WantPass, c)
			}
		})
	}
}

// TestBurned proves the caption proof against a box drawn in the caption's place. The real burn
// runs in the engine's calibration, on the ffmpeg builds that have the subtitles filter.
func TestBurned(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	box := Caption{
		At: 700 * time.Millisecond, Clear: 200 * time.Millisecond, X: 0.2, Y: 0.85, W: 0.6, H: 0.13,
	}
	drawn := "drawbox=x=iw*0.3:y=ih*0.88:w=iw*0.4:h=ih*0.06:color=white:t=fill:" +
		"enable='between(t,0.5,0.9)'"
	always := "drawbox=x=iw*0.3:y=ih*0.88:w=iw*0.4:h=ih*0.06:color=white:t=fill"
	tests := []struct {
		Name     string
		Filter   string
		Caption  Caption
		WantPass bool
	}{{ // Test 0: A caption that shows only at its time passes.
		Name: "drawn", Filter: drawn, Caption: box, WantPass: true,
	}, { // Test 1: No caption fails.
		Name: "missing", Filter: "null", Caption: box,
	}, { // Test 2: A mark on every frame is not a caption at its time.
		Name: "always there", Filter: always, Caption: box,
	}, { // Test 3: A caption elsewhere in the frame fails.
		Name: "wrong place", Filter: strings.ReplaceAll(drawn, "ih*0.88", "ih*0.1"), Caption: box,
	}, { // Test 4: A box the filter refuses is a failed proof.
		Name: "bad box", Filter: drawn,
		Caption: Caption{At: box.At, Clear: box.Clear, X: 2, Y: 2, W: 2, H: 2},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, _ := simSource(t, "subs", dir)
			out := makeClip(t, dir, "out.mp4", "-i", src, "-map", "0:v", "-vf", test.Filter,
				"-c:v", "libx264", "-crf", "12")
			got, err := Verify(t.Context(), Input{
				Source: src, Output: out, Expect: Expect{Caption: &test.Caption},
			})
			if err != nil {
				t.Fatal(err)
			}
			c := checkByName(t, got, CheckBurned)
			t.Logf("%+v", c)
			if c.Pass != test.WantPass {
				t.Errorf("pass = %v, want %v: %s", c.Pass, test.WantPass, c.Measured)
			}
		})
	}
}

// TestBurnedNoVideo proves an output without a picture cannot hold a burned caption.
func TestBurnedNoVideo(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src, _ := simSource(t, "markers", dir)
	out := makeClip(t, dir, "out.flac", "-i", src, "-vn", "-c:a", "flac")
	got, err := Verify(t.Context(), Input{
		Source: src, Output: out, Expect: Expect{Caption: &Caption{W: 1, H: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := checkByName(t, got, CheckBurned); c.Pass {
		t.Errorf("passed with no picture: %+v", c)
	}
}

// TestScoresUnread proves a score that cannot run is a failed proof rather than a pass, through an
// ffmpeg that fails only on the named pass.
func TestScoresUnread(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src, truth := simSource(t, "markers", dir)
	out := makeClip(t, dir, "out.mkv", "-i", src, "-c:v", "libx264", "-crf", "18", "-c:a", "aac")
	tests := []struct {
		Name   string
		Fail   string
		Expect Expect
		Want   string
	}{{ // Test 0: The picture score cannot run.
		Name: "picture", Fail: "ssim", Expect: Expect{MinSSIM: 0.5}, Want: CheckPicture,
	}, { // Test 1: The sound score cannot run.
		Name: "sound score", Fail: "asdr", Expect: Expect{MinSDR: 1}, Want: CheckSound,
	}, { // Test 2: The samples for alignment cannot be read.
		Name: "sound samples", Fail: "f32le", Expect: Expect{MinSDR: 1}, Want: CheckSound,
	}, { // Test 3: The flashes cannot be read.
		Name: "flashes", Fail: "blackdetect", Expect: Expect{Markers: truth.Markers}, Want: CheckSync,
	}, { // Test 4: The bursts cannot be read.
		Name: "bursts", Fail: "silencedetect", Expect: Expect{Markers: truth.Markers}, Want: CheckSync,
	}, { // Test 5: The samples for the tones cannot be read.
		Name: "tones", Fail: "f32le", Expect: Expect{Tones: [][]float64{{1000, 1000}}},
		Want: CheckChannelTones,
	}, { // Test 6: The caption's frames cannot be read.
		Name: "caption", Fail: "blend", Expect: Expect{Caption: &Caption{W: 1, H: 1}}, Want: CheckBurned,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			fake := filepath.Join(t.TempDir(), "ffmpeg")
			tooltest.WriteScript(t, fake, "#!/bin/sh\ncase \"$*\" in\n*"+test.Fail+
				"*) echo 'cannot read' >&2; exit 1;;\nesac\nexec ffmpeg \"$@\"\n")
			got, err := Verify(t.Context(), Input{
				Source: src, Output: out, Expect: test.Expect, Tools: Tools{FFmpeg: fake},
			})
			if err != nil {
				t.Fatal(err)
			}
			failed := checkNames(got.Failed())
			if diff := cmp.Diff([]string{test.Want}, failed, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("failed checks mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLastFloat covers the log number reader.
func TestLastFloat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Log       string
		WantValue float64
		WantError bool
	}{{ // Test 0: The last match wins.
		Log: "All:0.5\nAll:0.75\n", WantValue: 0.75,
	}, { // Test 1: No match is an error.
		Log: "nothing here\n", WantError: true,
	}, { // Test 2: A match that does not parse is an error.
		Log: "All:1.2.3\n", WantError: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := lastFloat(regexp.MustCompile(`All:([0-9.]+)`), []byte(test.Log))
			if (err != nil) != test.WantError {
				t.Fatalf("error = %v, want error %v", err, test.WantError)
			}
			if got != test.WantValue {
				t.Errorf("value = %v, want %v", got, test.WantValue)
			}
		})
	}
}

// TestDecibels covers the score text, naming a perfect score.
func TestDecibels(t *testing.T) {
	t.Parallel()
	if got := decibels(math.Inf(1)); got != "perfect" {
		t.Errorf("decibels(inf) = %q", got)
	}
	if got := decibels(33.44); got != "33.4 dB" {
		t.Errorf("decibels(33.44) = %q", got)
	}
}
