package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/dcadolph/icelos-ffmpeg/probe"
	"github.com/dcadolph/icelos-ffmpeg/tooltest"
)

// Inline sources, built fresh for each case that names them.
var (
	// plainSource is a short H.264 and AAC clip.
	plainSource = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k",
	}
	// flacSource is the same clip with lossless sound in Matroska.
	flacSource = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "flac",
	}
	// hlgSource is a clip tagged with wide gamut primaries and the HLG transfer curve.
	hlgSource = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2," +
			"setparams=range=tv:colorspace=bt2020nc:color_trc=arib-std-b67:color_primaries=bt2020",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
	}
	// bframeSource is a clip whose video reorders frames.
	bframeSource = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-preset", "medium", "-bf", "3", "-pix_fmt", "yuv420p", "-c:a", "aac",
	}
	// twoTrackSource is a clip with two different sound tracks.
	twoTrackSource = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=2",
		"-map", "0", "-map", "1", "-map", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "flac",
	}
	// fullRangeSource is a clip tagged full range, which an unstated range does not stand for.
	fullRangeSource = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-f", "lavfi", "-i",
		"testsrc2=size=320x180:rate=24:duration=2,scale=out_range=pc,format=yuvj420p",
		"-c:v", "libx264", "-preset", "ultrafast",
	}
	// anamorphicSource is a PAL frame with non-square pixels.
	anamorphicSource = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-f", "lavfi", "-i", "testsrc2=size=720x576:rate=25:duration=1,setsar=64/45",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
	}
	// combedSource is truly interlaced footage tagged top field first, from gradients, since idet
	// reads gradients true where it misreads testsrc2.
	combedSource = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-f", "lavfi", "-i",
		"gradients=size=720x576:rate=50:duration=2:speed=0.5," +
			"tinterlace=interleave_top,setfield=tff",
		"-c:v", "mpeg2video", "-q:v", "2", "-flags", "+ilme+ildct", "-f", "mpegts",
	}
	// gradientSource is progressive gradients footage.
	gradientSource = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-f", "lavfi", "-i", "gradients=size=640x360:rate=24:duration=2:speed=0.5",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
	}
	// variableSource keeps every third frame of the first second and all of the next, so its
	// frames are not evenly spaced.
	variableSource = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
		"-f", "lavfi", "-i", `testsrc2=size=320x180:rate=30:duration=2,select='gte(t\,1)+not(mod(n\,3))'`,
		"-fps_mode", "passthrough", "-enc_time_base", "filter", "-c:v", "libx264", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p",
	}
)

// x264 encodes video fast and copies nothing else unless asked.
var x264 = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
	"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
}

// ffmpegRun runs ffmpeg with args and fails the test on an error.
func ffmpegRun(t *testing.T, args ...string) {
	t.Helper()
	full := append([]string{"-hide_banner", "-nostdin", "-v", "error", "-y"}, args...)
	if out, err := exec.CommandContext(t.Context(), "ffmpeg", full...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// makeClip writes a file named name in dir from ffmpeg args and returns its path.
func makeClip(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	ffmpegRun(t, append(append([]string{}, args...), path)...)
	return path
}

// checkNames returns the names of checks.
func checkNames(checks []Check) []string {
	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	return names
}

// noteNames returns the names of notes.
func noteNames(notes []Note) []string {
	var names []string
	for _, n := range notes {
		names = append(names, n.Name)
	}
	return names
}

func TestVerify(t *testing.T) { //nolint:funlen,maintidx // Test function.
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	tests := []struct {
		Name       string
		Source     []string
		SourceName string
		Prep       []string
		Input      []string
		Out        []string
		OutName    string
		Expect     Expect
		WantFailed []string
		WantHas    string
		WantRan    []string
		WantNotes  []string
	}{{ // Test 0: A remux passes every check it runs.
		Name: "remux", Source: plainSource, Out: []string{"-c", "copy"}, OutName: "out.mkv",
		Expect: Expect{
			Streams: []string{"video:h264", "audio:aac"}, VideoCopied: true, AudioCopied: true,
		},
		WantRan: []string{
			CheckContainer, CheckDecode, CheckStreams, CheckDuration, CheckFrames, CheckTiming, CheckDisplay,
			CheckSampleAspect, CheckAspect, CheckRotation, CheckBitDepth, CheckFieldOrder, CheckCopiedVideo,
			CheckCopiedAudio,
		},
		WantNotes: []string{CheckColor},
	}, { // Test 1: A plain encode passes.
		Name: "encode", Source: plainSource, Out: append(x264[:len(x264):len(x264)], "-c:a", "aac"),
		Expect: Expect{Streams: []string{"video:h264", "audio:aac"}, PixFmt: "yuv420p"},
		WantRan: []string{
			CheckContainer, CheckDecode, CheckStreams, CheckDuration, CheckFrames, CheckTiming, CheckDisplay,
			CheckSampleAspect, CheckAspect, CheckRotation, CheckBitDepth, CheckFieldOrder, CheckSound,
		},
		WantNotes: []string{CheckColor},
	}, { // Test 2: A rotated copy keeps its rotation as data.
		Name: "rotated copy", Source: plainSource, Prep: []string{"-display_rotation", "90"},
		Out: []string{"-c", "copy"}, Expect: Expect{VideoCopied: true, AudioCopied: true},
	}, { // Test 3: A rotated encode turns the pixels and keeps the shown size.
		Name: "rotated encode", Source: plainSource, Prep: []string{"-display_rotation", "90"},
		Out: x264, Expect: Expect{PixFmt: "yuv420p"},
	}, { // Test 4: An encode that leaves the turn as data instead of in the pixels fails rotation.
		Name: "noautorotate", Source: plainSource, Prep: []string{"-display_rotation", "90"},
		Input: []string{"-noautorotate"},
		Out:   x264, Expect: Expect{},
		WantFailed: []string{CheckRotation},
	}, { // Test 5: Non-square pixels survive an encode.
		Name: "anamorphic", Source: anamorphicSource, Out: x264, Expect: Expect{PixFmt: "yuv420p"},
	}, { // Test 6: Square pixels forced on an anamorphic source fail on shape.
		Name: "setsar", Source: anamorphicSource,
		Out:        append(x264[:len(x264):len(x264)], "-vf", "setsar=1"),
		WantFailed: []string{CheckSampleAspect, CheckAspect},
	}, { // Test 7: Wide gamut tags survive a copy.
		Name: "hlg copy", Source: hlgSource, Out: []string{"-c", "copy"},
		Expect: Expect{VideoCopied: true},
	}, { // Test 8: A retagged transfer curve fails the color tags.
		Name: "color retag", Source: hlgSource,
		Out:        append(x264[:len(x264):len(x264)], "-vf", "setparams=color_trc=bt709"),
		WantFailed: []string{CheckColor},
	}, { // Test 9: Ten-bit HEVC proves its depth.
		Name: "ten bit", Source: plainSource,
		Out: []string{
			"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=none",
			"-pix_fmt", "yuv420p10le",
		},
		Expect: Expect{PixFmt: "yuv420p10le"},
	}, { // Test 10: An eight-bit output against a ten-bit plan fails the depth.
		Name: "wrong depth", Source: plainSource, Out: x264, Expect: Expect{PixFmt: "yuv420p10le"},
		WantFailed: []string{CheckBitDepth},
	}, { // Test 11: FFV1 decodes to the source's exact frames.
		Name: "ffv1", Source: plainSource, Out: []string{"-c:v", "ffv1", "-c:a", "flac"},
		OutName: "out.mkv",
		Expect:  Expect{LosslessVideo: true},
	}, { // Test 12: A lossy encode against a lossless plan fails on the frames.
		Name: "lossy as lossless", Source: plainSource, Out: x264, Expect: Expect{LosslessVideo: true},
		WantFailed: []string{CheckLosslessVideo},
	}, { // Test 13: FLAC decodes to the source's exact samples.
		Name: "flac", Source: flacSource, SourceName: "src.mkv",
		Out:     []string{"-c:v", "copy", "-c:a", "flac"},
		OutName: "out.mkv", Expect: Expect{VideoCopied: true, LosslessAudio: true},
	}, { // Test 14: A lossy audio encode against a lossless plan fails on the samples.
		Name: "lossy audio as lossless", Source: flacSource, SourceName: "src.mkv",
		Out: []string{"-c:v", "copy", "-c:a", "aac"}, OutName: "out.mkv",
		Expect: Expect{VideoCopied: true, LosslessAudio: true}, WantFailed: []string{CheckLosslessAudio},
	}, { // Test 15: Clean gradients through an encode prove no combing.
		Name: "comb proof", Source: gradientSource, Out: x264, Expect: Expect{CombProof: true},
	}, { // Test 16: A deinterlaced encode of combed footage reads progressive.
		Name: "deinterlaced", Source: combedSource, SourceName: "src.ts",
		Out:    append(x264[:len(x264):len(x264)], "-vf", "yadif=mode=send_frame"),
		Expect: Expect{CombProof: true},
	}, { // Test 17: An interlaced source encoded without deinterlacing stays combed.
		Name: "combed", Source: combedSource, SourceName: "src.ts", Out: x264,
		Expect:     Expect{CombProof: true},
		WantFailed: []string{CheckCombing},
	}, { // Test 18: An encode tagged interlaced fails the field order.
		Name: "tagged interlaced", Source: combedSource, SourceName: "src.ts",
		Out:        append(x264[:len(x264):len(x264)], "-flags", "+ilme+ildct"),
		WantFailed: []string{CheckFieldOrder},
		WantNotes:  []string{CheckStreams, CheckBitDepth, CheckCombing},
	}, { // Test 19: An interlaced copy keeps its tag and notes the combing it cannot prove.
		Name: "interlaced copy", Source: combedSource, SourceName: "src.ts", Out: []string{"-c", "copy"},
		OutName: "out.mkv", Expect: Expect{VideoCopied: true},
		WantNotes: []string{CheckStreams, CheckCombing},
	}, { // Test 20: Variable rate kept through the filter time base passes timing.
		Name: "variable rate", Source: variableSource,
		Out: append(x264[:len(x264):len(x264)], "-fps_mode", "passthrough", "-enc_time_base", "filter"),
	}, { // Test 21: Variable rate forced constant fails on frames and length.
		Name: "forced constant", Source: variableSource,
		Out:        append(x264[:len(x264):len(x264)], "-fps_mode", "cfr", "-r", "20"),
		WantFailed: []string{CheckDuration, CheckFrames},
		WantNotes:  []string{CheckStreams, CheckTiming, CheckColor, CheckBitDepth},
	}, { // Test 22: Variable rate rounded to a coarse encoder time base fails timing alone.
		Name: "rounded times", Source: variableSource,
		Out: append(x264[:len(x264):len(x264)],
			"-fps_mode", "passthrough", "-enc_time_base", "1:24"),
		WantFailed: []string{CheckTiming},
	}, { // Test 23: A set rate lands on the count its length implies.
		Name: "set rate", Source: plainSource, Out: append(x264[:len(x264):len(x264)], "-r", "30"),
		Expect: Expect{Rate: 30},
		WantRan: []string{
			CheckContainer, CheckDecode, CheckDuration, CheckFrames, CheckDisplay, CheckSampleAspect,
			CheckAspect, CheckRotation, CheckFieldOrder, CheckSound,
		},
	}, { // Test 24: A set rate the output did not take fails the frame count.
		Name: "wrong rate", Source: plainSource, Out: x264, Expect: Expect{Rate: 30},
		WantFailed: []string{CheckFrames},
	}, { // Test 25: A scale keeps the shape and is not held to the size.
		Name: "scaled", Source: plainSource,
		Out:    append(x264[:len(x264):len(x264)], "-vf", "scale=160:90"),
		Expect: Expect{Scaled: true},
		WantRan: []string{
			CheckContainer, CheckDecode, CheckDuration, CheckFrames, CheckTiming, CheckAspect,
			CheckRotation, CheckFieldOrder, CheckSound,
		},
	}, { // Test 26: A waived check becomes a note and cannot fail.
		Name: "waived", Source: plainSource, Out: x264,
		Expect:    Expect{Rate: 30, Waive: map[string]string{CheckFrames: "not this case"}},
		WantNotes: []string{CheckStreams, CheckFrames, CheckColor, CheckBitDepth},
	}, { // Test 27: A dropped frame fails the frame count and cannot be paired for timing.
		Name: "dropped frame", Source: plainSource,
		Out: append(x264[:len(x264):len(x264)],
			"-vf", `select='not(eq(n\,10))'`, "-fps_mode", "passthrough"),
		WantFailed: []string{CheckFrames},
		WantNotes:  []string{CheckStreams, CheckTiming, CheckColor, CheckBitDepth},
	}, { // Test 28: A quarter turn in the pixels fails the shown size and shape.
		Name: "transpose", Source: plainSource,
		Out:        append(x264[:len(x264):len(x264)], "-vf", "transpose=1"),
		WantFailed: []string{CheckDisplay, CheckAspect},
	}, { // Test 29: A stream the plan did not ask for fails the stream list.
		Name: "wrong streams", Source: plainSource, Out: x264,
		Expect: Expect{Streams: []string{"video:hevc", "audio:aac"}}, WantFailed: []string{CheckStreams},
	}, { // Test 30: An output cut short fails on length and frames.
		Name: "cut short", Source: plainSource, Out: append(x264[:len(x264):len(x264)], "-t", "1"),
		WantFailed: []string{CheckDuration, CheckFrames},
		WantNotes:  []string{CheckStreams, CheckTiming, CheckColor, CheckBitDepth},
	}, { // Test 31: Noise in one packet of a copy fails the hash, whatever else it breaks.
		Name: "noisy copy", Source: plainSource,
		Out:    []string{"-c", "copy", "-bsf:v", `noise=amount=if(eq(n\,10)\,1\,0)`},
		Expect: Expect{VideoCopied: true}, WantHas: CheckCopiedVideo,
	}, { // Test 32: Wide gamut tags survive an encode.
		Name: "hlg encode", Source: hlgSource, Out: x264,
	}, { // Test 33: A full range source encoded to an unstated range fails the color tags.
		Name: "full range lost", Source: fullRangeSource,
		Out:        append(x264[:len(x264):len(x264)], "-vf", "scale=out_range=tv"),
		WantFailed: []string{CheckColor},
	}, { // Test 34: A copy with reshaped packets still decodes to the source's frames.
		Name: "reshaped copy", Source: bframeSource, Out: []string{"-c", "copy"}, OutName: "out.ts",
		Expect: Expect{VideoCopied: true, AudioCopied: true},
	}, { // Test 35: Reordered frames still line up in time.
		Name: "b-frames encode", Source: bframeSource,
		Out: []string{"-c:v", "libx264", "-preset", "medium", "-bf", "3", "-pix_fmt", "yuv420p"},
	}, { // Test 36: Sound pulled out of a video is held to the source's sound.
		Name: "audio only", Source: plainSource, Out: []string{"-vn", "-c:a", "aac"}, OutName: "out.m4a",
		Expect:  Expect{Streams: []string{"audio:aac"}},
		WantRan: []string{CheckContainer, CheckDecode, CheckStreams, CheckDuration, CheckSound},
	}, { // Test 37: Sound cut short fails on length.
		Name: "audio cut short", Source: plainSource, Out: []string{"-vn", "-c:a", "aac", "-t", "1"},
		OutName: "out.m4a", WantFailed: []string{CheckDuration},
	}, { // Test 38: A video-copy plan with no video left fails the copy.
		Name: "copied video gone", Source: plainSource, Out: []string{"-vn", "-c:a", "copy"},
		OutName: "out.m4a", Expect: Expect{VideoCopied: true}, WantFailed: []string{CheckCopiedVideo},
	}, { // Test 39: An audio-copy plan with no audio left fails the copy.
		Name: "copied audio gone", Source: plainSource, Out: []string{"-an", "-c:v", "copy"},
		Expect: Expect{VideoCopied: true, AudioCopied: true}, WantFailed: []string{CheckCopiedAudio},
	}, { // Test 40: A lossless plan with no audio left fails the samples.
		Name: "lossless audio gone", Source: flacSource, SourceName: "src.mkv",
		Out:     []string{"-an", "-c:v", "copy"},
		OutName: "out.mkv", Expect: Expect{VideoCopied: true, LosslessAudio: true},
		WantFailed: []string{CheckLosslessAudio},
	}, { // Test 41: A dropped track cannot be paired, and the receipt says so.
		Name: "track dropped", Source: twoTrackSource, SourceName: "src.mkv",
		Out: []string{"-map", "0:v", "-map", "0:a:0", "-c", "copy"}, OutName: "out.mkv",
		Expect:    Expect{VideoCopied: true, AudioCopied: true},
		WantNotes: []string{CheckStreams, CheckCopiedAudio},
	}, { // Test 42: Tracks in swapped order fail the copy.
		Name: "tracks swapped", Source: twoTrackSource, SourceName: "src.mkv",
		Out: []string{"-map", "0:v", "-map", "0:a:1", "-map", "0:a:0", "-c", "copy"}, OutName: "out.mkv",
		Expect:     Expect{VideoCopied: true, AudioCopied: true},
		WantFailed: []string{CheckCopiedAudio, CheckCopiedAudio},
	}, { // Test 43: A video made from audio has no source frames to compare.
		Name:       "picture from sound",
		Source:     []string{"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:a", "aac"},
		SourceName: "src.m4a",
		Out: append(x264[:len(x264):len(x264)], "-filter_complex", "[0:a]showwaves=s=320x180[v]",
			"-map", "[v]", "-map", "0:a"),
		WantRan: []string{CheckContainer, CheckDecode, CheckDuration, CheckSound},
	}, { // Test 44: Noise in a middle audio packet fails a reshaped copy despite the end margin.
		Name: "noisy reshaped audio", Source: bframeSource, OutName: "out.ts",
		Out:     []string{"-c", "copy", "-bsf:a", `noise=amount=if(eq(n\,20)\,1\,0)`},
		Expect:  Expect{VideoCopied: true, AudioCopied: true},
		WantHas: CheckCopiedAudio,
	}, { // Test 45: A codec that always writes full range passes a plan that says so.
		Name: "stated full range",
		Source: []string{
			"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=1", "-c:v", "libx264",
			"-preset", "ultrafast", "-pix_fmt", "yuv420p", "-color_range", "tv",
		},
		Out: []string{"-c:v", "mjpeg", "-q:v", "3"}, OutName: "out.mov", Expect: Expect{Range: "pc"},
	}, { // Test 46: The same plan fails an encode that kept limited range.
		Name: "full range not written", Source: hlgSource, Out: x264, Expect: Expect{Range: "pc"},
		WantFailed: []string{CheckColor},
	}, { // Test 47: The order between kinds says nothing, so a plan may list them either way.
		Name: "streams by kind", Source: plainSource, Out: x264,
		Expect: Expect{Streams: []string{"audio:aac", "video:h264"}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			sourceName := test.SourceName
			if sourceName == "" {
				sourceName = "src.mp4"
			}
			src := makeClip(t, dir, sourceName, test.Source...)
			if test.Prep != nil {
				prep := append(append(append([]string{}, test.Prep...), "-i", src), "-c", "copy")
				src = makeClip(t, dir, "prep-"+sourceName, prep...)
			}
			outName := test.OutName
			if outName == "" {
				outName = "out.mp4"
			}
			args := append(append(append([]string{}, test.Input...), "-i", src), test.Out...)
			out := makeClip(t, dir, outName, args...)
			got, err := Verify(t.Context(), Input{Source: src, Output: out, Expect: test.Expect})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range got.Checks {
				t.Logf("%+v", c)
			}
			if test.WantHas != "" {
				if !slices.Contains(checkNames(got.Failed()), test.WantHas) {
					t.Errorf("failed checks %v lack %s", checkNames(got.Failed()), test.WantHas)
				}
			} else if diff := cmp.Diff(
				test.WantFailed, checkNames(got.Failed()), cmpopts.EquateEmpty(),
			); diff != "" {
				t.Errorf("failed checks mismatch (-want +got):\n%s", diff)
			}
			if test.WantRan != nil {
				if diff := cmp.Diff(test.WantRan, checkNames(got.Checks)); diff != "" {
					t.Errorf("checks that ran mismatch (-want +got):\n%s", diff)
				}
			}
			if test.WantNotes != nil {
				notes := slices.DeleteFunc(noteNames(got.Unchecked), func(name string) bool {
					return name == CheckPicture || name == CheckSound
				})
				if diff := cmp.Diff(test.WantNotes, notes); diff != "" {
					t.Errorf("notes mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// flipBytes inverts count bytes at the middle of the file at path.
func flipBytes(t *testing.T, path string, count int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range count {
		data[len(data)/2+i] ^= 0xff
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDamagedOutput(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src := makeClip(t, dir, "src.mp4", plainSource...)
	remux := func(t *testing.T, name string) string {
		t.Helper()
		return makeClip(t, t.TempDir(), name, "-i", src, "-c", "copy", "-movflags", "+faststart")
	}
	tests := []struct {
		Name       string
		Build      func(t *testing.T) string
		WantFailed []string
		WantHas    string
		WantRan    []string
	}{{ // Test 0: A zero-byte file fails the container check and stops there.
		Name: "zero bytes",
		Build: func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "out.mp4")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		},
		WantFailed: []string{CheckContainer}, WantRan: []string{CheckContainer},
	}, { // Test 1: A missing file fails the container check and stops there.
		Name: "missing",
		Build: func(t *testing.T) string {
			t.Helper()
			return filepath.Join(t.TempDir(), "nothing.mp4")
		},
		WantFailed: []string{CheckContainer}, WantRan: []string{CheckContainer},
	}, { // Test 2: Text named as media fails the container check and stops there.
		Name: "not media",
		Build: func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "out.mp4")
			if err := os.WriteFile(path, []byte("this is not a video"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		},
		WantFailed: []string{CheckContainer}, WantRan: []string{CheckContainer},
	}, { // Test 3: A file cut mid data still probes, and then loses length and frames to the cut.
		Name: "cut after faststart",
		Build: func(t *testing.T) string {
			t.Helper()
			path := remux(t, "out.mp4")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, info.Size()/2); err != nil {
				t.Fatal(err)
			}
			return path
		},
		WantFailed: []string{CheckDecode, CheckDuration, CheckFrames, CheckCopiedVideo, CheckCopiedAudio},
	}, { // Test 4: One flipped run of bytes in a copy fails the hash, whatever else it breaks.
		Name: "flipped bytes",
		Build: func(t *testing.T) string {
			t.Helper()
			path := remux(t, "out.mp4")
			flipBytes(t, path, 8)
			return path
		},
		WantHas: CheckCopiedVideo,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := Verify(t.Context(), Input{
				Source: src, Output: test.Build(t), Expect: Expect{VideoCopied: true, AudioCopied: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range got.Checks {
				t.Logf("%+v", c)
			}
			if got.Passed() {
				t.Error("Passed() = true on a damaged output")
			}
			if test.WantHas != "" {
				if !slices.Contains(checkNames(got.Failed()), test.WantHas) {
					t.Errorf("failed checks %v lack %s", checkNames(got.Failed()), test.WantHas)
				}
			} else if diff := cmp.Diff(
				test.WantFailed, checkNames(got.Failed()), cmpopts.EquateEmpty(),
			); diff != "" {
				t.Errorf("failed checks mismatch (-want +got):\n%s", diff)
			}
			if test.WantRan != nil {
				if diff := cmp.Diff(test.WantRan, checkNames(got.Checks)); diff != "" {
					t.Errorf("checks that ran mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// corpusDir holds the real clips the tests use.
const corpusDir = "testdata/media"

// TestVerifyRealClips proves a stream copy of every real clip, so a proof that misreads a real
// muxer or a real encoder's stream shows up here rather than as a false alarm on a user's receipt.
func TestVerifyRealClips(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	clips := 0
	for _, entry := range entries {
		if entry.Name() == "manifest.json" {
			continue
		}
		clips++
		t.Run(entry.Name(), func(t *testing.T) {
			t.Parallel()
			src := filepath.Join(corpusDir, entry.Name())
			info, err := Tools{}.probe(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			_, hasVideo := info.FirstVideo()
			_, hasAudio := info.FirstAudio()
			out := makeClip(t, t.TempDir(), "out"+filepath.Ext(src), "-i", src, "-map", "0", "-c", "copy")
			got, err := Verify(t.Context(), Input{
				Source: src, Output: out, Expect: Expect{VideoCopied: hasVideo, AudioCopied: hasAudio},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range got.Failed() {
				t.Errorf("%+v", c)
			}
			for _, n := range got.Unchecked {
				t.Logf("%+v", n)
			}
		})
	}
	if clips == 0 {
		t.Fatal("the corpus has no clips")
	}
}

// TestVerifyEditListCopy proves a copy into a container without edit lists fails when it plays
// the frames the source's edit list hides, which a packet hash alone cannot see.
func TestVerifyEditListCopy(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	src := filepath.Join(corpusDir, "sintel-trailer-h264-aac.mp4")
	out := makeClip(t, t.TempDir(), "out.mkv", "-i", src, "-map", "0", "-c", "copy")
	got, err := Verify(t.Context(), Input{
		Source: src, Output: out, Expect: Expect{VideoCopied: true, AudioCopied: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := checkNames(got.Failed())
	for _, name := range []string{CheckFrames, CheckDuration} {
		if !slices.Contains(failed, name) {
			t.Errorf("failed checks %v lack %s", failed, name)
		}
	}
}

// TestVerifyUnread proves a hash that cannot run on the source is a note, and one that cannot run
// on the output is a failed check, through an ffmpeg that fails only on the named pass.
func TestVerifyUnread(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src := makeClip(t, dir, "src.mkv", flacSource...)
	out := makeClip(t, dir, "out.mkv", "-i", src, "-c", "copy")
	reencoded := makeClip(t, dir, "ts-out.ts", "-i", src, "-c:v", "copy", "-c:a", "aac")
	tests := []struct {
		Name       string
		Fail       string
		Output     string
		Expect     Expect
		WantFailed []string
		WantNotes  []string
	}{{ // Test 0: The source's packets cannot be read.
		Name: "source packets", Fail: "src.mkv*streamhash", Output: out,
		Expect:    Expect{VideoCopied: true},
		WantNotes: []string{CheckCopiedVideo},
	}, { // Test 1: The output's packets cannot be read.
		Name: "output packets", Fail: "out.mkv*streamhash", Output: out,
		Expect:     Expect{VideoCopied: true},
		WantFailed: []string{CheckCopiedVideo},
	}, { // Test 2: The source's frames cannot be read.
		Name: "source frames", Fail: "src.mkv*framemd5", Output: out,
		Expect:    Expect{LosslessVideo: true},
		WantNotes: []string{CheckLosslessVideo},
	}, { // Test 3: The output's frames cannot be read.
		Name: "output frames", Fail: "out.mkv*framemd5", Output: out,
		Expect:     Expect{LosslessVideo: true},
		WantFailed: []string{CheckLosslessVideo},
	}, { // Test 4: The source's samples cannot be read.
		Name: "source samples", Fail: "src.mkv*-f hash", Output: out,
		Expect:    Expect{LosslessAudio: true},
		WantNotes: []string{CheckLosslessAudio},
	}, { // Test 5: The output's samples cannot be read.
		Name: "output samples", Fail: "out.mkv*-f hash", Output: out,
		Expect:     Expect{LosslessAudio: true},
		WantFailed: []string{CheckLosslessAudio},
	}, { // Test 6: Audio that fell back to frames cannot be read in the output.
		Name: "output audio frames", Fail: "ts-out.ts*framemd5", Output: reencoded,
		Expect:     Expect{AudioCopied: true},
		WantFailed: []string{CheckCopiedAudio},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			fake := filepath.Join(t.TempDir(), "ffmpeg")
			tooltest.WriteScript(t, fake, "#!/bin/sh\ncase \"$*\" in\n*"+
				strings.ReplaceAll(test.Fail, " ", "\\ ")+
				"*) echo 'cannot read' >&2; exit 1;;\nesac\nexec ffmpeg \"$@\"\n")
			got, err := Verify(t.Context(), Input{
				Source: src, Output: test.Output, Expect: test.Expect, Tools: Tools{FFmpeg: fake},
			})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(
				test.WantFailed, checkNames(got.Failed()), cmpopts.EquateEmpty(),
			); diff != "" {
				t.Errorf("failed checks mismatch (-want +got):\n%s", diff)
			}
			notes := noteNames(got.Unchecked)
			for _, want := range test.WantNotes {
				if !slices.Contains(notes, want) {
					t.Errorf("notes %v lack %s", notes, want)
				}
			}
		})
	}
}

func TestAlignedFrames(t *testing.T) {
	t.Parallel()
	seq := strings.Split("a b c d e f g h", " ")
	tests := []struct {
		Name string
		Want []string
		Got  []string
		Edge int
		Pass bool
	}{{ // Test 0: The same frames pass.
		Name: "equal", Want: seq, Got: seq, Edge: 2, Pass: true,
	}, { // Test 1: One frame dropped at the start of the source passes.
		Name: "shifted", Want: seq, Got: append([]string{"z"}, seq...), Edge: 2, Pass: true,
	}, { // Test 2: A frame lost at the end passes.
		Name: "short end", Want: seq, Got: seq[:7], Edge: 2, Pass: true,
	}, { // Test 3: Both end frames differing passes.
		Name: "ends differ", Want: seq, Got: strings.Split("x b c d e f g y", " "), Edge: 2, Pass: true,
	}, { // Test 4: A middle frame differing fails.
		Name: "middle differs", Want: seq, Got: strings.Split("a b c X e f g h", " "), Edge: 2,
	}, { // Test 5: Three extra frames fail, past the margin.
		Name: "too many", Want: seq, Got: append(seq[:len(seq):len(seq)], "x", "y", "z"), Edge: 2,
	}, { // Test 6: Three lost frames fail, past the margin.
		Name: "too few", Want: seq, Got: seq[:5], Edge: 2,
	}, { // Test 7: A shift past the margin fails.
		Name: "shifted too far", Want: seq, Got: strings.Split("a a a a a a b c d e f", " "), Edge: 2,
	}, { // Test 8: A clip under five frames must match exactly.
		Name: "short clip", Edge: 2,
		Want: []string{"a", "b", "c", "d"}, Got: []string{"a", "b", "c", "x"},
	}, { // Test 9: A short clip that matches passes.
		Name: "short clip equal", Want: []string{"a", "b"}, Got: []string{"a", "b"}, Edge: 2, Pass: true,
	}, { // Test 10: No margin means exact, so one differing end frame fails.
		Name: "exact", Want: seq, Got: strings.Split("x b c d e f g h", " "),
	}, { // Test 11: No margin passes on the same frames.
		Name: "exact equal", Want: seq, Got: seq, Pass: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := alignedFrames(test.Want, test.Got, test.Edge); got != test.Pass {
				t.Errorf("alignedFrames() = %v, want %v", got, test.Pass)
			}
		})
	}
}

func TestFrameScanSpan(t *testing.T) {
	t.Parallel()
	ms := time.Millisecond
	tests := []struct {
		Times []time.Duration
		Want  time.Duration
	}{{  // Test 0: No frames play for no time.
	}, { // Test 1: One frame has no known length.
		Times: []time.Duration{0},
	}, { // Test 2: Two frames count the last as long as the first.
		Times: []time.Duration{0, 40 * ms}, Want: 80 * ms,
	}, { // Test 3: The last gap sets the last frame's length.
		Times: []time.Duration{0, 40 * ms, 100 * ms}, Want: 160 * ms,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := (frameScan{Times: test.Times}).Span(); got != test.Want {
				t.Errorf("Span() = %s, want %s", got, test.Want)
			}
		})
	}
}

func TestPlayTime(t *testing.T) {
	t.Parallel()
	scan := frameScan{Times: []time.Duration{0, time.Second}}
	tests := []struct {
		Stream probe.Stream
		Want   time.Duration
		Scan   frameScan
	}{{ // Test 0: The decoded frames win over a recorded duration.
		Stream: probe.Stream{Duration: "5.000000"}, Scan: scan, Want: 2 * time.Second,
	}, { // Test 1: No decoded frames falls back to the recorded duration.
		Stream: probe.Stream{Duration: "5.000000"}, Want: 5 * time.Second,
	}, { // Test 2: Neither gives nothing.
		Stream: probe.Stream{Duration: "N/A"}, Want: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := playTime(test.Stream, test.Scan); got != test.Want {
				t.Errorf("playTime() = %s, want %s", got, test.Want)
			}
		})
	}
}

func TestByKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   []string
		Want []string
	}{{ // Test 0: Kinds come together.
		In:   []string{"video:h264", "audio:aac", "subtitle:subrip", "audio:mp3"},
		Want: []string{"audio:aac", "audio:mp3", "subtitle:subrip", "video:h264"},
	}, { // Test 1: A muxer that writes audio first reads the same.
		In:   []string{"audio:mp3", "video:flv1"},
		Want: []string{"audio:mp3", "video:flv1"},
	}, { // Test 2: The order within a kind stays, so swapped tracks differ.
		In:   []string{"audio:mp3", "audio:aac"},
		Want: []string{"audio:mp3", "audio:aac"},
	}, { // Test 3: Nothing stays nothing.
		In: nil, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.Want, byKind(test.In), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseSpan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantResult time.Duration
		WantErr    bool
	}{{ // Test 0: From the first frame's start to the last one's end, in the header's time base.
		In:         "#tb 0: 1/1000\n0, 100, 100, 50, 4, 0x0\n0, 150, 150, 50, 4, 0x0\n",
		WantResult: 100 * time.Millisecond,
	}, { // Test 1: No frames.
		In: "#tb 0: 1/1000\n", WantErr: true,
	}, { // Test 2: A bad time base.
		In: "#tb 0: x/y\n", WantErr: true,
	}, { // Test 3: A short line.
		In: "#tb 0: 1/1000\n0, 100\n", WantErr: true,
	}, { // Test 4: A frame before any time base.
		In: "0, 100, 100, 50, 4, 0x0\n", WantErr: true,
	}, { // Test 5: A time that does not parse.
		In: "#tb 0: 1/1000\n0, x, x, y, 4, 0x0\n", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := parseSpan([]byte(test.In))
			if (err != nil) != test.WantErr {
				t.Fatalf("parseSpan() error = %v, want error %v", err, test.WantErr)
			}
			if got != test.WantResult {
				t.Errorf("parseSpan() = %s, want %s", got, test.WantResult)
			}
		})
	}
}

// TestProofNotes reaches the note paths no real file can, with proof values built by hand.
func TestProofNotes(t *testing.T) {
	t.Parallel()
	copied := Input{Expect: Expect{VideoCopied: true}}
	tests := []struct {
		Name      string
		Proof     proof
		Run       func(p *proof)
		WantNotes []string
		WantRan   []string
	}{{ // Test 0: A source with no frame size cannot prove the picture's shape.
		Name: "aspect", Run: (*proof).aspect, WantNotes: []string{CheckAspect},
	}, { // Test 1: A frame size runs the shape proof.
		Name: "aspect ran", Run: (*proof).aspect, WantRan: []string{CheckAspect},
		Proof: proof{
			srcVideo: probe.Stream{Width: 16, Height: 9}, outVideo: probe.Stream{Width: 16, Height: 9},
		},
	}, { // Test 2: A copy whose source states no field order cannot prove it.
		Name: "field order empty", Run: (*proof).fieldOrder, WantNotes: []string{CheckFieldOrder},
		Proof: proof{in: copied},
	}, { // Test 3: An unknown field order is no statement either.
		Name: "field order unknown", Run: (*proof).fieldOrder, WantNotes: []string{CheckFieldOrder},
		Proof: proof{in: copied, srcVideo: probe.Stream{FieldOrder: "unknown"}},
	}, { // Test 4: A stated field order runs the proof.
		Name: "field order ran", Run: (*proof).fieldOrder, WantRan: []string{CheckFieldOrder},
		Proof: proof{in: copied, srcVideo: probe.Stream{FieldOrder: "tt"}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			p := test.Proof
			test.Run(&p)
			if diff := cmp.Diff(test.WantNotes, noteNames(p.receipt.Unchecked)); diff != "" {
				t.Errorf("notes mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantRan, checkNames(p.receipt.Checks)); diff != "" {
				t.Errorf("checks mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestVerifyCannotRun(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src := makeClip(t, dir, "src.mp4", plainSource...)
	out := makeClip(t, dir, "out.mkv", "-i", src, "-c", "copy")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	tests := []struct {
		Ctx   context.Context //nolint:containedctx // Test table.
		Input Input
		Name  string
	}{{ // Test 0: A source that is not there cannot be proven against.
		Ctx: t.Context(), Name: "missing source",
		Input: Input{Source: filepath.Join(dir, "nothing.mp4"), Output: out},
	}, { // Test 1: A missing ffmpeg is the machine's fault, not the output's.
		Ctx: t.Context(), Name: "missing ffmpeg",
		Input: Input{Source: src, Output: out, Tools: Tools{FFmpeg: filepath.Join(dir, "no-ffmpeg")}},
	}, { // Test 2: A missing ffprobe is the machine's fault, not the output's.
		Ctx: t.Context(), Name: "missing ffprobe",
		Input: Input{Source: src, Output: out, Tools: Tools{FFprobe: filepath.Join(dir, "no-ffprobe")}},
	}, { // Test 3: A context canceled before the start stops the run.
		Ctx: canceled, Name: "canceled", Input: Input{Source: src, Output: out},
	}, { // Test 4: An expired context stops the run.
		Ctx: expired, Name: "expired", Input: Input{Source: src, Output: out},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if _, err := Verify(test.Ctx, test.Input); !errors.Is(err, ErrVerify) {
				t.Errorf("Verify() = %v, want %v", err, ErrVerify)
			}
		})
	}
}

func TestVerifyCanceledMidway(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src := makeClip(t, dir, "src.mp4", plainSource...)
	out := makeClip(t, dir, "out.mkv", "-i", src, "-c", "copy")
	started := filepath.Join(dir, "started")
	stuck := filepath.Join(dir, "stuck-ffmpeg")
	tooltest.WriteScript(t, stuck, "#!/bin/sh\ncase \"$*\" in *-version*) exec ffmpeg \"$@\";; esac\n"+
		"touch "+started+"\nexec sleep 60\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	begin := time.Now()
	_, err := Verify(ctx, Input{Source: src, Output: out, Tools: Tools{FFmpeg: stuck}})
	if !errors.Is(err, ErrVerify) || !errors.Is(err, context.Canceled) {
		t.Errorf("Verify() = %v, want %v from the canceled context", err, ErrVerify)
	}
	if took := time.Since(begin); took > 30*time.Second {
		t.Errorf("Verify() took %s after the cancel, want the stuck tool ended", took)
	}
}

func TestReceipt(t *testing.T) {
	t.Parallel()
	pass := Check{Name: CheckFrames, Pass: true}
	fail := Check{Name: CheckDecode}
	tests := []struct {
		Receipt    Receipt
		WantFailed []string
		WantPassed bool
	}{{ // Test 0: Nothing measured is not a pass.
		Receipt: Receipt{}, WantPassed: false,
	}, { // Test 1: Every proof passing is a pass.
		Receipt: Receipt{Checks: []Check{pass}}, WantPassed: true,
	}, { // Test 2: One failed proof fails the receipt and is named.
		Receipt: Receipt{Checks: []Check{pass, fail}}, WantPassed: false,
		WantFailed: []string{CheckDecode},
	}, { // Test 3: A note alone is not a pass.
		Receipt: Receipt{Unchecked: []Note{{Name: CheckColor}}}, WantPassed: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Receipt.Passed(); got != test.WantPassed {
				t.Errorf("Passed() = %v, want %v", got, test.WantPassed)
			}
			if diff := cmp.Diff(
				test.WantFailed, checkNames(test.Receipt.Failed()), cmpopts.EquateEmpty(),
			); diff != "" {
				t.Errorf("Failed() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReceiptJSON(t *testing.T) {
	t.Parallel()
	receipt := Receipt{
		Schema: SchemaVersion, Output: "o.mp4", Source: "s.mp4", Tool: "ffmpeg version 9",
		Checks: []Check{{
			Name: CheckFrames, Pass: true, Measured: "10 frames", Threshold: "10", Method: "count",
		}},
		Unchecked: []Note{{Name: CheckColor, Reason: "No tags."}},
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"schema":2,"output":"o.mp4","source":"s.mp4","checks":[{"name":"frames",` +
		`"pass":true,"measured":"10 frames","threshold":"10","method":"count"}],` +
		`"unchecked":[{"name":"color","reason":"No tags."}],"tool":"ffmpeg version 9",` +
		`"expect":{}}`
	if diff := cmp.Diff(want, string(data)); diff != "" {
		t.Errorf("JSON mismatch (-want +got):\n%s", diff)
	}
}

func TestParseFrames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In        string
		WantTimes []time.Duration
		WantErr   bool
	}{{ // Test 0: Times scale by the time base and count from the first frame.
		In: "#tb 0: 1/1000\n#media_type 0: video\n" +
			"0, 500, 500, 1, 8, 0xa\n0, 750, 750, 1, 8, 0xb\n",
		WantTimes: []time.Duration{0, 250 * time.Millisecond},
	}, { // Test 1: No frames is no times.
		In: "#tb 0: 1/1000\n",
	}, { // Test 2: A broken time base is an error.
		In: "#tb 0: nonsense\n", WantErr: true,
	}, { // Test 3: A zero time base is an error.
		In: "#tb 0: 0/1000\n", WantErr: true,
	}, { // Test 4: A frame before any time base is an error.
		In: "0, 1, 1, 1, 8, 0xa\n", WantErr: true,
	}, { // Test 5: A short frame line is an error.
		In: "#tb 0: 1/1000\n0, 1\n", WantErr: true,
	}, { // Test 6: A time that is not a number is an error.
		In: "#tb 0: 1/1000\n0, 1, x, 1, 8, 0xa\n", WantErr: true,
	}, { // Test 7: Empty input is no times.
		In: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := parseFrames([]byte(test.In))
			if (err != nil) != test.WantErr {
				t.Fatalf("parseFrames() error = %v, want error %v", err, test.WantErr)
			}
			if diff := cmp.Diff(test.WantTimes, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseIdet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In        string
		WantShare float64
		Want      idetCounts
	}{{ // Test 0: The last totals line wins over earlier ones.
		In: "[Parsed_idet_0 @ 0x1] [info] Multi frame detection: " +
			"TFF:     1 BFF:     1 Progressive:     1 Undetermined:     1\n" +
			"[Parsed_idet_0 @ 0x1] [info] Multi frame detection: " +
			"TFF:    10 BFF:    10 Progressive:    20 Undetermined:     0\n",
		Want: idetCounts{TFF: 10, BFF: 10, Progressive: 20}, WantShare: 0.5,
	}, { // Test 1: A log without totals reads zero.
		In: "nothing here\n", Want: idetCounts{}, WantShare: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := parseIdet([]byte(test.In))
			if diff := cmp.Diff(test.Want, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantShare, got.Share()); diff != "" {
				t.Errorf("share mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestErrorLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want []string
	}{{ // Test 0: Error, fatal, and panic lines count, with or without a component.
		In:   "[h264 @ 0x1] [error] bad\n[fatal] worse\n[out#0/null @ 0x2] [panic] worst\n[info] fine\n",
		Want: []string{"[h264 @ 0x1] [error] bad", "[fatal] worse", "[out#0/null @ 0x2] [panic] worst"},
	}, { // Test 1: A file name holding the word is not an error.
		In: "[info] Input #0, from '/tmp/[error] clip.mp4':\n",
	}, { // Test 2: A warning is not an error.
		In: "[h264 @ 0x1] [warning] careful\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.Want, errorLines([]byte(test.In)), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSampleAspect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want ratio
	}{
		{In: "64:45", Want: ratio{N: 64, D: 45}}, // Test 0: A stated shape.
		{In: "", Want: ratio{N: 1, D: 1}},        // Test 1: Missing means square.
		{In: "0:1", Want: ratio{N: 1, D: 1}},     // Test 2: Zero means square.
		{In: "N/A", Want: ratio{N: 1, D: 1}},     // Test 3: Not a ratio means square.
		{In: "a:b", Want: ratio{N: 1, D: 1}},     // Test 4: Not numbers means square.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.Want, sampleAspect(test.In)); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFirstField proves the tags muxers use for one first field read as one.
func TestFirstField(t *testing.T) {
	t.Parallel()
	if firstField("tt") != firstField("tb") || firstField("bb") != firstField("bt") {
		t.Error("tags for one first field differ")
	}
	if firstField("tt") == firstField("bb") {
		t.Error("top first and bottom first read as one")
	}
	if firstField("progressive") != "progressive" {
		t.Error("a tag with no first field changed")
	}
}

func TestPaddedBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Src, Out   []byte
		Limit      int64
		WantPadded int64
	}{{ // Test 0: Zeros past the end, within the limit.
		Src: []byte{1, 2}, Out: []byte{1, 2, 0, 0}, Limit: 4, WantPadded: 2,
	}, { // Test 1: No bytes past the end.
		Src: []byte{1, 2}, Out: []byte{1, 2}, Limit: 4,
	}, { // Test 2: A changed sample.
		Src: []byte{1, 2}, Out: []byte{1, 3, 0, 0}, Limit: 4,
	}, { // Test 3: Sound past the end.
		Src: []byte{1, 2}, Out: []byte{1, 2, 0, 9}, Limit: 4,
	}, { // Test 4: More zeros than the limit.
		Src: []byte{1, 2}, Out: []byte{1, 2, 0, 0, 0}, Limit: 2,
	}, { // Test 5: A shorter output.
		Src: []byte{1, 2, 3}, Out: []byte{1, 2}, Limit: 4,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := paddedBytes(bytes.NewReader(test.Src), bytes.NewReader(test.Out), test.Limit)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.WantPadded {
				t.Errorf("padded = %d, want %d", got, test.WantPadded)
			}
		})
	}
}
