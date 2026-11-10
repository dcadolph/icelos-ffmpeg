package verify

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/icelos-ffmpeg/probe"
	"github.com/dcadolph/icelos-ffmpeg/tooltest"
)

// TestSound proves the SDR check, including that a delay does not read as a loss.
func TestSound(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	floor := mustFloor(t, SoundFloor, "aac", "128k")
	tests := []struct {
		Name     string
		Out      []string
		File     string
		Expect   Expect
		WantPass bool
		WantLag  string
	}{{ // Test 0: The web preset's sound clears the floor.
		Name: "128k", Out: []string{"-c:a", "aac", "-b:a", "128k"}, File: "out.m4a",
		Expect: Expect{MinSDR: floor}, WantPass: true,
	}, { // Test 1: A low bitrate falls below the floor.
		Name: "32k", Out: []string{"-c:a", "aac", "-b:a", "32k"}, File: "out.m4a",
		Expect: Expect{MinSDR: floor},
	}, { // Test 2: A codec delay no container hides still scores, since sync judges the delay.
		Name: "ac3 delay", Out: []string{"-c:a", "ac3", "-b:a", "384k"}, File: "out.ac3",
		Expect: Expect{MinSDR: 30}, WantPass: true,
	}, { // Test 3: Added silence moves the samples by exactly that much.
		Name: "delayed", Out: []string{"-af", "adelay=10ms:all=1", "-c:a", "flac"}, File: "out.flac",
		Expect: Expect{MinSDR: 60}, WantPass: true, WantLag: "aligned by 480 samples",
	}, { // Test 4: A downmix scores against the same downmix of the source, free of its gain.
		Name: "downmix", File: "mono.flac",
		Out:    []string{"-ac", "1", "-af", "volume=0.5", "-c:a", "flac"},
		Expect: Expect{MinSDR: 60}, WantPass: true,
	}, { // Test 5: A gain change on a stream that kept its channels is a loss.
		Name: "quieter", Out: []string{"-af", "volume=0.5", "-c:a", "flac"}, File: "out.flac",
		Expect: Expect{MinSDR: 60},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, _ := simSource(t, "markers", dir)
			out := makeClip(t, dir, test.File, append([]string{"-i", src, "-vn"}, test.Out...)...)
			got, err := Verify(t.Context(), Input{Source: src, Output: out, Expect: test.Expect})
			if err != nil {
				t.Fatal(err)
			}
			c := checkByName(t, got, CheckSound)
			t.Logf("%+v", c)
			if c.Pass != test.WantPass {
				t.Errorf("pass = %v, want %v: %s", c.Pass, test.WantPass, c.Measured)
			}
			if test.WantLag != "" && !strings.Contains(c.Measured, test.WantLag) {
				t.Errorf("measured %q lacks %q", c.Measured, test.WantLag)
			}
		})
	}
}

// TestSoundQuietOpening proves the alignment finds a codec's delay when the sound starts well into
// the track, past what the search compares.
func TestSoundQuietOpening(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src := makeClip(t, dir, "src.flac", "-f", "lavfi", "-i",
		"aevalsrc=if(gt(t\\,12)\\,0.5*sin(2*PI*700*t*t/12)\\,0):c=stereo:s=48000:d=16", "-c:a", "flac")
	out := makeClip(t, dir, "out.ac3", "-i", src, "-c:a", "ac3", "-b:a", "384k")
	got, err := Verify(t.Context(), Input{Source: src, Output: out, Expect: Expect{MinSDR: 20}})
	if err != nil {
		t.Fatal(err)
	}
	c := checkByName(t, got, CheckSound)
	t.Logf("%+v", c)
	if !c.Pass || !strings.Contains(c.Measured, "aligned by 256 samples") {
		t.Errorf("did not align past the silence: %s", c.Measured)
	}
}

// TestSoundPairing proves tracks pair by the plan and that unpaired tracks are not claimed.
// TestSoundSilentChannels proves a channel the source leaves silent does not fail the score, and a
// track silent throughout is noted rather than failed.
func TestSoundSilentChannels(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	tests := []struct {
		Name      string
		Graph     string
		WantNoted bool
	}{{ // Test 0: One silent channel leaves the other to score.
		Name: "one channel", Graph: "aevalsrc=0.5*sin(2*PI*440*t)|0:c=stereo:s=48000:d=2",
	}, { // Test 1: A silent track has nothing to score.
		Name: "whole track", Graph: "aevalsrc=0|0:c=stereo:s=48000:d=2", WantNoted: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src := makeClip(t, dir, "src.mka", "-f", "lavfi", "-i", test.Graph, "-c:a", "flac")
			out := makeClip(t, dir, "out.m4a", "-i", src, "-c:a", "aac", "-b:a", "128k")
			got, err := Verify(t.Context(), Input{Source: src, Output: out, Expect: Expect{MinSDR: 1}})
			if err != nil {
				t.Fatal(err)
			}
			noted := slices.ContainsFunc(got.Unchecked, func(n Note) bool { return n.Name == CheckSound })
			if noted != test.WantNoted {
				t.Errorf("noted = %v, want %v: %+v", noted, test.WantNoted, got.Unchecked)
			}
			if !test.WantNoted {
				if c := checkByName(t, got, CheckSound); !c.Pass {
					t.Errorf("sound failed: %s against %s", c.Measured, c.Threshold)
				}
			}
		})
	}
}

func TestLowestChannel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantResult float64
		Want       error
		WantErr    bool
	}{{ // Test 0: The lowest channel wins.
		In: "SDR ch0: 30.5 dB\nSDR ch1: 12.0 dB\n", WantResult: 12,
	}, { // Test 1: A silent channel on the Mac's C library is left out.
		In: "SDR ch0: 30.5 dB\nSDR ch1: nan dB\n", WantResult: 30.5,
	}, { // Test 2: A silent channel on Linux's C library is left out.
		In: "SDR ch0: 30.5 dB\nSDR ch1: -nan dB\n", WantResult: 30.5,
	}, { // Test 3: Minus infinity means the source's channel held nothing.
		In: "SDR ch0: 30.5 dB\nSDR ch1: -inf dB\n", WantResult: 30.5,
	}, { // Test 4: A perfect channel reads as infinity and stays the score.
		In: "SDR ch0: inf dB\n", WantResult: math.Inf(1),
	}, { // Test 5: Every channel silent.
		In: "SDR ch0: -nan dB\nSDR ch1: nan dB\n", Want: errSilent, WantErr: true,
	}, { // Test 6: No score at all.
		In: "nothing here\n", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := lowestChannel([]byte(test.In))
			if (err != nil) != test.WantErr || (test.Want != nil && !errors.Is(err, test.Want)) {
				t.Fatalf("lowestChannel() error = %v, want %v", err, test.Want)
			}
			if got != test.WantResult {
				t.Errorf("lowestChannel() = %v, want %v", got, test.WantResult)
			}
		})
	}
}

func TestSoundPairing(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src, _ := simSource(t, "multiaudio", dir)
	second := makeClip(t, dir, "second.mka", "-i", src, "-map", "0:a:1", "-c:a", "flac")
	tests := []struct {
		Name     string
		Tracks   []int
		WantPass bool
		WantNote bool
	}{{ // Test 0: The plan names the track that was kept.
		Name: "right track", Tracks: []int{1}, WantPass: true,
	}, { // Test 1: A different track of the source fails.
		Name: "wrong track", Tracks: []int{0},
	}, { // Test 2: Without a plan the counts differ, so nothing is claimed.
		Name: "no plan", WantNote: true,
	}, { // Test 3: A plan for more tracks than the output holds claims nothing.
		Name: "too many", Tracks: []int{0, 1}, WantNote: true,
	}, { // Test 4: A track the source lacks claims nothing.
		Name: "out of range", Tracks: []int{9}, WantNote: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := Verify(t.Context(), Input{
				Source: src, Output: second, Expect: Expect{MinSDR: 40, AudioTracks: test.Tracks},
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.WantNote {
				if diff := cmp.Diff([]string(nil), checkNames(got.Failed())); diff != "" {
					t.Errorf("failed checks mismatch (-want +got):\n%s", diff)
				}
				return
			}
			c := checkByName(t, got, CheckSound)
			t.Logf("%+v", c)
			if c.Pass != test.WantPass {
				t.Errorf("pass = %v, want %v: %s", c.Pass, test.WantPass, c.Measured)
			}
		})
	}
}

// TestChannelTones proves a swapped channel fails on every channel the swap touches.
func TestChannelTones(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	tests := []struct {
		Name     string
		Recipe   string
		Filter   string
		Tones    func(have [][]float64) [][]float64
		WantPass bool
	}{{ // Test 0: Every channel of a surround encode holds its tone.
		Name: "surround", Recipe: "tones-51", Filter: "anull", WantPass: true,
	}, { // Test 1: Front left and front right swapped fails.
		Name: "swapped", Recipe: "tones-51", Filter: "channelmap=1|0|2|3|4|5",
	}, { // Test 2: A silenced channel fails.
		Name: "silenced", Recipe: "tones-51", Filter: "pan=5.1|c0=c0|c1=0*c1|c2=c2|c3=c3|c4=c4|c5=c5",
	}, { // Test 3: Stereo holds its tones.
		Name: "stereo", Recipe: "tones-stereo", Filter: "anull", WantPass: true,
	}, { // Test 4: More tracks stated than the output holds fails.
		Name: "track count", Recipe: "tones-stereo", Filter: "anull",
		Tones: func(have [][]float64) [][]float64 { return append(have, have[0]) },
	}, { // Test 5: Fewer channels than stated fails.
		Name: "channel count", Recipe: "tones-stereo", Filter: "pan=mono|c0=c0",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, truth := simSource(t, test.Recipe, dir)
			out := makeClip(t, dir, "out.m4a", "-i", src, "-vn", "-af", test.Filter,
				"-c:a", "aac", "-b:a", "512k")
			tones := [][]float64{truth.Audio[0].Tones}
			if test.Tones != nil {
				tones = test.Tones(tones)
			}
			got, err := Verify(t.Context(), Input{Source: src, Output: out, Expect: Expect{Tones: tones}})
			if err != nil {
				t.Fatal(err)
			}
			c := checkByName(t, got, CheckChannelTones)
			t.Logf("%+v", c)
			if c.Pass != test.WantPass {
				t.Errorf("pass = %v, want %v: %s", c.Pass, test.WantPass, c.Measured)
			}
		})
	}
}

// TestChannels proves the channel count, layout, and sample rate check.
func TestChannels(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	tests := []struct {
		Name     string
		Out      []string
		Expect   Expect
		WantPass bool
		WantNone bool
	}{{ // Test 0: The source's five one layout survives.
		Name: "kept", Out: []string{"-c:a", "aac"},
		Expect: Expect{Channels: 6, SampleRate: 48000}, WantPass: true,
	}, { // Test 1: A planned downmix passes.
		Name: "downmix", Out: []string{"-c:a", "aac", "-ac", "2"},
		Expect: Expect{Channels: 2}, WantPass: true,
	}, { // Test 2: An unplanned downmix fails.
		Name: "lost channels", Out: []string{"-c:a", "aac", "-ac", "2"}, Expect: Expect{Channels: 6},
	}, { // Test 3: A changed sample rate fails.
		Name: "rate", Out: []string{"-c:a", "aac", "-ar", "44100"}, Expect: Expect{SampleRate: 48000},
	}, { // Test 4: The same channel count in another layout fails.
		Name: "layout", Out: []string{"-c:a", "flac", "-af", "aformat=channel_layouts=hexagonal"},
		Expect: Expect{Channels: 6},
	}, { // Test 5: A plan that states neither runs nothing.
		Name: "unstated", Out: []string{"-c:a", "aac"}, WantNone: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, _ := simSource(t, "tones-51", dir)
			out := makeClip(t, dir, "out.mka", append([]string{"-i", src, "-vn"}, test.Out...)...)
			got, err := Verify(t.Context(), Input{Source: src, Output: out, Expect: test.Expect})
			if err != nil {
				t.Fatal(err)
			}
			if test.WantNone {
				for _, c := range got.Checks {
					if c.Name == CheckChannels {
						t.Errorf("channel check ran: %+v", c)
					}
				}
				return
			}
			c := checkByName(t, got, CheckChannels)
			t.Logf("%+v", c)
			if c.Pass != test.WantPass {
				t.Errorf("pass = %v, want %v: %s", c.Pass, test.WantPass, c.Threshold)
			}
		})
	}
}

// burst returns silence with a noisy burst starting at the given sample, which correlates with
// itself at one offset only.
func burst(length, start int) []float32 {
	samples := make([]float32, length)
	for i := start; i < min(start+200, length); i++ {
		samples[i] = float32(math.Sin(float64(i-start) * float64(i-start) * 0.01))
	}
	return samples
}

// TestBestLag proves the alignment search finds a shift both ways and holds still on a pure tone,
// whose every period correlates the same.
func TestBestLag(t *testing.T) {
	t.Parallel()
	tone := make([]float32, 4800)
	for i := range tone {
		tone[i] = float32(math.Sin(2 * math.Pi * 480 * float64(i) / 48000))
	}
	tests := []struct {
		Ref     []float32
		Out     []float32
		WantLag int
	}{{ // Test 0: No shift.
		Ref: burst(4800, 1000), Out: burst(4800, 1000), WantLag: 0,
	}, { // Test 1: The output runs late.
		Ref: burst(4800, 1000), Out: burst(4800, 1300), WantLag: 300,
	}, { // Test 2: The output runs early.
		Ref: burst(4800, 1000), Out: burst(4800, 800), WantLag: -200,
	}, { // Test 3: A pure tone stays at zero.
		Ref: tone, Out: tone, WantLag: 0,
	}, { // Test 4: Silence gives no reason to move.
		Ref: make([]float32, 4800), Out: make([]float32, 4800), WantLag: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := bestLag(test.Ref, test.Out, 0, 500, 1); got != test.WantLag {
				t.Errorf("bestLag() = %d, want %d", got, test.WantLag)
			}
		})
	}
}

// TestToneShare covers the power reading on tones, silence, and nothing.
func TestToneShare(t *testing.T) {
	t.Parallel()
	const rate = 48000
	tone := func(freq float64) []float32 {
		samples := make([]float32, rate)
		for i := range samples {
			samples[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/rate))
		}
		return samples
	}
	tests := []struct {
		Samples []float32
		Freq    float64
		Rate    int
		WantMin float64
		WantMax float64
	}{{ // Test 0: A pure tone holds all its power at its frequency.
		Samples: tone(440), Freq: 440, Rate: rate, WantMin: 0.99, WantMax: 1.01,
	}, { // Test 1: It holds none at another.
		Samples: tone(440), Freq: 880, Rate: rate, WantMin: 0, WantMax: 0.01,
	}, { // Test 2: Silence holds none.
		Samples: make([]float32, 100), Freq: 440, Rate: rate, WantMin: 0, WantMax: 0,
	}, { // Test 3: Nothing holds none.
		Freq: 440, Rate: rate, WantMin: 0, WantMax: 0,
	}, { // Test 4: No sample rate holds none.
		Samples: tone(440), Freq: 440, WantMin: 0, WantMax: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := toneShare(test.Samples, test.Freq, test.Rate)
			if got < test.WantMin || got > test.WantMax {
				t.Errorf("toneShare() = %v, want %v to %v", got, test.WantMin, test.WantMax)
			}
		})
	}
}

// TestDecimate covers the coarse search's averaging.
func TestDecimate(t *testing.T) {
	t.Parallel()
	got := decimate([]float32{1, 3, 5, 7, 9}, 2)
	if diff := cmp.Diff([]float32{2, 6}, got); diff != "" {
		t.Errorf("decimate mismatch (-want +got):\n%s", diff)
	}
}

// TestLayoutClass proves the side and back forms of a layout read as one.
func TestLayoutClass(t *testing.T) {
	t.Parallel()
	if layoutClass("5.1(side)") != layoutClass("5.1") || layoutClass("5.1(back)") != "5.1" {
		t.Error("side and back forms differ")
	}
	if layoutClass("stereo") == layoutClass("mono") {
		t.Error("different layouts read as one")
	}
}

// TestSoundScoreRefuses covers the inputs the score cannot run on.
func TestSoundScoreRefuses(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src, _ := simSource(t, "markers", dir)
	out := filepath.Join(dir, "absent.m4a")
	_, _, err := (Tools{}).soundScore(t.Context(), src, out, probe.Stream{}, probe.Stream{})
	if err == nil {
		t.Error("scored a stream with no sample rate")
	}
	_, _, err = (Tools{}).soundScore(t.Context(), src, out,
		probe.Stream{Index: 1, SampleRate: 48000}, probe.Stream{Index: 0, SampleRate: 48000})
	if err == nil {
		t.Error("scored a missing output")
	}
}

// TestSoundLevelCheck proves the level check on footage with no floor: sound that keeps its level passes
// and a track that lost its sound fails.
func TestSoundLevelCheck(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	tests := []struct {
		Name     string
		Out      []string
		File     string
		WantPass bool
	}{{ // Test 0: A lossy encode keeps the level.
		Name: "aac", Out: []string{"-c:a", "aac", "-b:a", "128k"}, File: "out.m4a", WantPass: true,
	}, { // Test 1: A low bitrate that fails a waveform floor still keeps the level.
		Name: "32k", Out: []string{"-c:a", "aac", "-b:a", "32k"}, File: "out.m4a", WantPass: true,
	}, { // Test 2: A downmix has more room.
		Name: "downmix", Out: []string{"-ac", "1", "-c:a", "aac"}, File: "out.m4a", WantPass: true,
	}, { // Test 3: Silence where the source has sound fails.
		Name: "silenced", Out: []string{"-af", "volume=0", "-c:a", "aac"}, File: "out.m4a",
	}, { // Test 4: A large gain change fails.
		Name: "quieter", Out: []string{"-af", "volume=0.1", "-c:a", "aac"}, File: "out.m4a",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, _ := simSource(t, "markers", dir)
			out := makeClip(t, dir, test.File, append([]string{"-i", src, "-vn"}, test.Out...)...)
			got, err := Verify(t.Context(), Input{Source: src, Output: out})
			if err != nil {
				t.Fatal(err)
			}
			c := checkByName(t, got, CheckSound)
			t.Logf("%+v", c)
			if c.Pass != test.WantPass {
				t.Errorf("pass = %v, want %v: %s", c.Pass, test.WantPass, c.Measured)
			}
			if !slices.ContainsFunc(got.Unchecked, func(n Note) bool {
				return n.Name == CheckSound && strings.Contains(n.Reason, "not scored")
			}) {
				t.Errorf("the receipt does not say the sound is not scored: %+v", got.Unchecked)
			}
		})
	}
}

// TestSoundLevelSilentSource proves a silent source has nothing to compare, which is a note.
func TestSoundLevelSilentSource(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	src := makeClip(t, dir, "src.flac", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo:d=2", "-c:a", "flac")
	out := makeClip(t, dir, "out.m4a", "-i", src, "-c:a", "aac")
	got, err := Verify(t.Context(), Input{Source: src, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(got.Checks, func(c Check) bool { return c.Name == CheckSound }) {
		t.Errorf("a silent source ran the sound check: %+v", got.Checks)
	}
	if !slices.ContainsFunc(got.Unchecked, func(n Note) bool {
		return n.Name == CheckSound && strings.Contains(n.Reason, "silent")
	}) {
		t.Errorf("the receipt does not say the source is silent: %+v", got.Unchecked)
	}
}
