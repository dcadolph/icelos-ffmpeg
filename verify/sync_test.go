package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcadolph/icelos-ffmpeg/tooltest"
)

// TestSync proves the marker proof and its controls.
func TestSync(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	tests := []struct {
		Name      string
		Args      func(src string) []string
		File      string
		WantPass  bool
		WantInLog string
	}{{ // Test 0: An encode keeps flash and burst together and on schedule.
		Name: "encode", File: "out.mp4", WantPass: true,
		Args: func(src string) []string {
			return []string{"-i", src, "-c:v", "libx264", "-crf", "18", "-c:a", "aac"}
		},
	}, { // Test 1: Sound moved 200 ms late fails by about that much.
		Name: "late sound", File: "out.mkv", WantInLog: "worst offset 20",
		Args: func(src string) []string {
			return []string{
				"-i", src, "-itsoffset", "0.2", "-i", src, "-map", "0:v", "-map", "1:a", "-c", "copy",
			}
		},
	}, { // Test 2: Sound 50 ms late stays inside the limit.
		Name: "slightly late sound", File: "out.mkv", WantPass: true,
		Args: func(src string) []string {
			return []string{
				"-i", src, "-itsoffset", "0.05", "-i", src, "-map", "0:v", "-map", "1:a", "-c", "copy",
			}
		},
	}, { // Test 3: Sound 70 ms late is past the limit, under a frame rate that would have let it by.
		Name: "too late sound", File: "out.mkv", WantInLog: "worst offset 70",
		Args: func(src string) []string {
			return []string{
				"-i", src, "-itsoffset", "0.07", "-i", src, "-map", "0:v", "-map", "1:a", "-c", "copy",
			}
		},
	}, { // Test 4: Sound 50 ms early is past the shorter limit on that side.
		Name: "early sound", File: "out.mkv", WantInLog: "worst offset 50",
		Args: func(src string) []string {
			return []string{
				"-i", src, "-itsoffset", "0.05", "-i", src, "-map", "1:v", "-map", "0:a", "-c", "copy",
			}
		},
	}, { // Test 5: A timeline that starts late as a whole moves nothing against anything else.
		Name: "whole file late", File: "out.ts", WantPass: true,
		Args: func(src string) []string { return []string{"-i", src, "-c:v", "libx264", "-c:a", "aac"} },
	}, { // Test 6: Picture alone is held to the schedule.
		Name: "picture only", File: "out.mkv", WantPass: true,
		Args: func(src string) []string { return []string{"-i", src, "-an", "-c:v", "copy"} },
	}, { // Test 7: Sound alone is held to the schedule.
		Name: "sound only", File: "out.flac", WantPass: true,
		Args: func(src string) []string { return []string{"-i", src, "-vn", "-c:a", "flac"} },
	}, { // Test 8: Silenced sound has no bursts to read.
		Name: "no bursts", File: "out.mkv", WantInLog: "no onset near",
		Args: func(src string) []string {
			return []string{"-i", src, "-af", "volume=0", "-c:v", "copy", "-c:a", "flac"}
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, truth := simSource(t, "markers", dir)
			out := makeClip(t, dir, test.File, test.Args(src)...)
			got, err := Verify(t.Context(), Input{
				Source: src, Output: out, Expect: Expect{Markers: truth.Markers},
			})
			if err != nil {
				t.Fatal(err)
			}
			c := checkByName(t, got, CheckSync)
			t.Logf("%+v", c)
			if c.Pass != test.WantPass {
				t.Errorf("pass = %v, want %v: %s", c.Pass, test.WantPass, c.Measured)
			}
			if !strings.Contains(c.Measured, test.WantInLog) {
				t.Errorf("measured %q lacks %q", c.Measured, test.WantInLog)
			}
		})
	}
}

// TestNearest covers the pairing of an onset with its scheduled time.
func TestNearest(t *testing.T) {
	t.Parallel()
	times := []time.Duration{time.Second, 2 * time.Second}
	tests := []struct {
		Want     time.Duration
		WantTime time.Duration
		WantOK   bool
	}{{ // Test 0: An exact match.
		Want: time.Second, WantTime: time.Second, WantOK: true,
	}, { // Test 1: The closer of two.
		Want: 1800 * time.Millisecond, WantTime: 2 * time.Second, WantOK: true,
	}, { // Test 2: Too far from any.
		Want: 5 * time.Second,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, ok := nearest(times, test.Want)
			if got != test.WantTime || ok != test.WantOK {
				t.Errorf("nearest() = %v, %v, want %v, %v", got, ok, test.WantTime, test.WantOK)
			}
		})
	}
}

// TestChapters proves the chapter proof keeps, drops, and moves.
func TestChapters(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	moved := ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=100\nEND=500\ntitle=One\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=500\nEND=1000\ntitle=Two\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=1000\nEND=1500\ntitle=Three\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=1500\nEND=2000\ntitle=Four\n"
	tests := []struct {
		Name     string
		Out      []string
		Moved    bool
		Expect   Expect
		WantPass bool
	}{{ // Test 0: Copied chapters stay in place.
		Name: "kept", WantPass: true,
	}, { // Test 1: Dropped chapters fail a plan that keeps them.
		Name: "lost", Out: []string{"-map_chapters", "-1"},
	}, { // Test 2: Dropped chapters pass a plan that drops them.
		Name: "dropped", Out: []string{"-map_chapters", "-1"},
		Expect: Expect{DropChapters: true}, WantPass: true,
	}, { // Test 3: Kept chapters fail a plan that drops them.
		Name: "still there", Expect: Expect{DropChapters: true},
	}, { // Test 4: A chapter 100 ms late fails.
		Name: "moved", Moved: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, _ := simSource(t, "chapters", dir)
			args := []string{"-i", src}
			if test.Moved {
				meta := filepath.Join(dir, "moved.txt")
				if err := os.WriteFile(meta, []byte(moved), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "-i", meta, "-map", "0", "-map_chapters", "1")
			}
			args = append(append(args, test.Out...), "-c", "copy")
			out := makeClip(t, dir, "out.mkv", args...)
			got, err := Verify(t.Context(), Input{Source: src, Output: out, Expect: test.Expect})
			if err != nil {
				t.Fatal(err)
			}
			c := checkByName(t, got, CheckChapters)
			t.Logf("%+v", c)
			if c.Pass != test.WantPass {
				t.Errorf("pass = %v, want %v: %s", c.Pass, test.WantPass, c.Measured)
			}
		})
	}
}

// TestMetadata proves the tag proof keeps, drops, and catches a change.
func TestMetadata(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	tests := []struct {
		Name     string
		Out      []string
		File     string
		Expect   Expect
		WantPass bool
	}{{ // Test 0: Copied tags stay.
		Name: "kept", File: "out.mp4", WantPass: true,
	}, { // Test 1: Stripped tags fail a plan that keeps them.
		Name: "lost", File: "out.mp4",
		Out: []string{"-map_metadata", "-1", "-metadata:s:a:0", "language=und"},
	}, { // Test 2: Stripped tags pass a plan that strips them.
		Name: "stripped", File: "out.mp4",
		Out:    []string{"-map_metadata", "-1", "-metadata:s:a:0", "language=und"},
		Expect: Expect{DropMetadata: true}, WantPass: true,
	}, { // Test 3: Kept tags fail a plan that strips them.
		Name: "still there", File: "out.mp4", Expect: Expect{DropMetadata: true},
	}, { // Test 4: A changed title fails.
		Name: "changed", File: "out.mp4", Out: []string{"-metadata", "title=Other"},
	}, { // Test 5: A changed language fails.
		Name: "language", File: "out.mp4", Out: []string{"-metadata:s:a:0", "language=spa"},
	}, { // Test 6: A plan can say the container holds no language.
		Name: "no language", File: "out.mp4", Out: []string{"-metadata:s:a:0", "language=spa"},
		Expect: Expect{NoTrackLanguage: true}, WantPass: true,
	}, { // Test 7: A track named by the plan compares to its own language.
		Name: "picked track", File: "out.mp4", Expect: Expect{AudioTracks: []int{0}}, WantPass: true,
	}, { // Test 8: A plan can say the container holds no artist.
		Name: "no artist", File: "out.mp4", Out: []string{"-metadata", "artist="},
		Expect: Expect{NoTags: []string{"artist"}}, WantPass: true,
	}, { // Test 9: A title the container holds still has to be there.
		Name: "title lost beside no artist", File: "out.mp4", Out: []string{"-metadata", "title=Other"},
		Expect: Expect{NoTags: []string{"artist"}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, _ := simSource(t, "metadata", dir)
			args := append(append([]string{"-i", src}, test.Out...), "-c", "copy")
			out := makeClip(t, dir, test.File, args...)
			got, err := Verify(t.Context(), Input{Source: src, Output: out, Expect: test.Expect})
			if err != nil {
				t.Fatal(err)
			}
			c := checkByName(t, got, CheckMetadata)
			t.Logf("%+v", c)
			if c.Pass != test.WantPass {
				t.Errorf("pass = %v, want %v: %s against %s", c.Pass, test.WantPass, c.Measured, c.Threshold)
			}
		})
	}
}
