package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/icelos-ffmpeg/probe"
	"github.com/dcadolph/icelos-ffmpeg/tooltest"
)

// scratchLimit caps what one run of the recipes may write.
const scratchLimit = 200 << 20

// frameTolerance is how far a measured frame time may sit from the recipe's.
const frameTolerance = 2 * time.Millisecond

// frameTimes reads each video frame's time from ffprobe, relative to the first frame.
func frameTimes(ctx context.Context, path string) ([]time.Duration, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "frame=pts_time", "-of", "csv=p=0", path).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe frames: %w", err)
	}
	var times []time.Duration
	for line := range strings.Lines(string(out)) {
		field, _, _ := strings.Cut(strings.TrimSpace(line), ",")
		if field == "" {
			continue
		}
		seconds, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return nil, fmt.Errorf("parse frame time %q: %w", field, err)
		}
		times = append(times, time.Duration(seconds*float64(time.Second)))
	}
	for i := len(times) - 1; i >= 0; i-- {
		times[i] -= times[0]
	}
	return times, nil
}

// proveFrames compares measured frame times with the recipe's.
func proveFrames(ctx context.Context, path string, truth Truth) []string {
	times, err := frameTimes(ctx, path)
	if err != nil {
		return []string{err.Error()}
	}
	want := truth.FrameTimes()
	if len(times) != len(want) {
		return []string{fmt.Sprintf("frames: got %d, want %d", len(times), len(want))}
	}
	for i := range want {
		if d := (times[i] - want[i]).Abs(); d > frameTolerance {
			return []string{fmt.Sprintf("frame %d: got %v, want %v", i, times[i], want[i])}
		}
	}
	return nil
}

// proveVideo compares the probed video stream with the recipe's.
func proveVideo(info probe.Info, truth Truth) []string {
	s, ok := info.FirstVideo()
	if !ok {
		return []string{"video stream missing"}
	}
	var problems []string
	compare := func(name, got, want string) {
		if want != "" && got != want {
			problems = append(problems, fmt.Sprintf("%s: got %q, want %q", name, got, want))
		}
	}
	compare("video codec", s.Codec, truth.VideoCodec)
	compare("sample aspect", s.SampleAspect, truth.SampleAspect)
	compare("field order", s.FieldOrder, truth.FieldOrder)
	compare("pixel format", s.PixFmt, truth.PixFmt)
	compare("color range", s.ColorRange, truth.Color.Range)
	compare("color space", s.ColorSpace, truth.Color.Space)
	compare("color transfer", s.ColorTransfer, truth.Color.Transfer)
	compare("color primaries", s.ColorPrimaries, truth.Color.Primaries)
	if s.Width != truth.Width || s.Height != truth.Height {
		problems = append(problems, fmt.Sprintf("size: got %dx%d, want %dx%d",
			s.Width, s.Height, truth.Width, truth.Height))
	}
	if s.Rotation() != truth.Rotation {
		problems = append(problems,
			fmt.Sprintf("rotation: got %d, want %d", s.Rotation(), truth.Rotation))
	}
	return problems
}

// proveAudio compares the probed audio streams with the recipe's.
func proveAudio(info probe.Info, truth Truth) []string {
	streams := info.Kind("audio")
	if len(streams) != len(truth.Audio) {
		return []string{fmt.Sprintf("audio tracks: got %d, want %d", len(streams), len(truth.Audio))}
	}
	var problems []string
	for i, want := range truth.Audio {
		s := streams[i]
		if s.Codec != want.Codec || s.Channels != want.Channels ||
			s.SampleRate != want.SampleRate || statedLanguage(s) != want.Language {
			problems = append(problems, fmt.Sprintf("audio %d: got %s %dch %d Hz %q, want %+v",
				i, s.Codec, s.Channels, s.SampleRate, s.Tags.Language, want))
		}
	}
	return problems
}

// statedLanguage returns a stream's language tag, with MP4's "und" read as no tag.
func statedLanguage(s probe.Stream) string {
	if s.Tags.Language == "und" {
		return ""
	}
	return s.Tags.Language
}

// channelRates reads the zero crossing rate of each channel of an audio track.
func channelRates(ctx context.Context, path string, track int) ([]float64, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-nostdin", "-nostats", "-i", path,
		"-map", fmt.Sprintf("0:a:%d", track), "-af", "astats", "-f", "null", "-")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("astats: %w", err)
	}
	var rates []float64
	var overall bool
	for line := range strings.Lines(stderr.String()) {
		overall = overall || strings.Contains(line, "] Overall")
		_, value, found := strings.Cut(line, "Zero crossings rate: ")
		if !found || overall {
			continue
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return nil, fmt.Errorf("parse rate %q: %w", value, err)
		}
		rates = append(rates, rate)
	}
	return rates, nil
}

// proveTones checks that each channel holds its stated tone.
func proveTones(ctx context.Context, path string, truth Truth) []string {
	var problems []string
	for track, want := range truth.Audio {
		if len(want.Tones) == 0 {
			continue
		}
		rates, err := channelRates(ctx, path, track)
		if err != nil {
			return []string{err.Error()}
		}
		if len(rates) != len(want.Tones) {
			problems = append(problems, fmt.Sprintf("track %d channels: got %d, want %d",
				track, len(rates), len(want.Tones)))
			continue
		}
		for ch, tone := range want.Tones {
			got := rates[ch] * float64(want.SampleRate) / 2
			if math.Abs(got-tone)/tone > 0.01 {
				problems = append(problems,
					fmt.Sprintf("track %d channel %d tone: got %.1f, want %.1f", track, ch, got, tone))
			}
		}
	}
	return problems
}

// detected returns the seconds following key on each line of log.
func detected(log, key string) []time.Duration {
	var times []time.Duration
	pattern := regexp.MustCompile(regexp.QuoteMeta(key) + `:?\s*(-?[0-9.]+)`)
	for _, m := range pattern.FindAllStringSubmatch(log, -1) {
		seconds, err := strconv.ParseFloat(m[1], 64)
		if err == nil {
			times = append(times, time.Duration(seconds*float64(time.Second)))
		}
	}
	return times
}

// logOf runs ffmpeg with args and returns what it logged.
func logOf(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-hide_banner", "-nostdin", "-nostats"}, args...)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", full...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stderr.String(), fmt.Errorf("ffmpeg: %w", err)
	}
	return stderr.String(), nil
}

// proveMarkers checks every flash and burst against the schedule. The first burst has no silence
// before it, so burst onsets start at the second marker.
func proveMarkers(ctx context.Context, path string, truth Truth) []string {
	if len(truth.Markers) == 0 {
		return nil
	}
	const slack = time.Second/24 + 5*time.Millisecond
	log, err := logOf(ctx, "-i", path, "-map", "0:v:0",
		"-vf", "negate,blackdetect=d=0.03:pix_th=0.1:pic_th=0.98", "-f", "null", "-")
	if err != nil {
		return []string{err.Error()}
	}
	flashes := detected(log, "black_start")
	log, err = logOf(ctx, "-i", path, "-map", "0:a:0",
		"-af", "silencedetect=noise=-50dB:d=0.2", "-f", "null", "-")
	if err != nil {
		return []string{err.Error()}
	}
	const eof = 5 * time.Millisecond
	bursts := slices.DeleteFunc(detected(log, "silence_end"), func(d time.Duration) bool {
		return d > truth.Duration-eof
	})
	var problems []string
	compare := func(name string, got, want []time.Duration) {
		if len(got) != len(want) {
			problems = append(problems, fmt.Sprintf("%s: got %v, want %v", name, got, want))
			return
		}
		for i := range want {
			if (got[i] - want[i]).Abs() > slack {
				problems = append(problems, fmt.Sprintf("%s %d: got %v, want %v", name, i, got[i], want[i]))
			}
		}
	}
	compare("flash", flashes, truth.Markers)
	compare("burst", bursts, truth.Markers[1:])
	return problems
}

// proveComb checks idet against the recipe's field order, on footage idet reads true.
func proveComb(ctx context.Context, path string, truth Truth) []string {
	if !truth.CombProof {
		return nil
	}
	log, err := logOf(ctx, "-i", path, "-map", "0:v:0", "-vf", "idet", "-f", "null", "-")
	if err != nil {
		return []string{err.Error()}
	}
	all := regexp.MustCompile(`Multi frame detection: TFF:\s*(\d+) BFF:\s*(\d+) Progressive:\s*(\d+)`).
		FindAllStringSubmatch(log, -1)
	if len(all) == 0 {
		return []string{"idet printed no totals"}
	}
	// ffmpeg logs an empty first total from the graph it configures before decoding.
	m := all[len(all)-1]
	tff, _ := strconv.Atoi(m[1])
	bff, _ := strconv.Atoi(m[2])
	progressive, _ := strconv.Atoi(m[3])
	total := float64(tff + bff + progressive)
	if total == 0 {
		return []string{"idet read no frames"}
	}
	var share float64
	switch truth.FieldOrder {
	case "tt":
		share = float64(tff) / total
	case "bb":
		share = float64(bff) / total
	default:
		share = float64(progressive) / total
	}
	if share < 0.95 {
		return []string{fmt.Sprintf("idet: TFF %d BFF %d progressive %d against tag %q",
			tff, bff, progressive, truth.FieldOrder)}
	}
	return nil
}

// proveMastering checks for mastering metadata on the first decoded frame.
func proveMastering(ctx context.Context, path string, truth Truth) []string {
	if !truth.Mastering {
		return nil
	}
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-read_intervals", "%+#1", "-show_frames", "-of", "json", path).Output()
	if err != nil {
		return []string{fmt.Sprintf("ffprobe side data: %v", err)}
	}
	if !bytes.Contains(out, []byte("Mastering display metadata")) {
		return []string{"mastering metadata missing"}
	}
	return nil
}

// srtCue matches one cue of ffmpeg's SubRip output.
var srtCue = regexp.MustCompile( //nolint:gochecknoglobals // Not modified, simplifies testing.
	`(\d+):(\d+):(\d+),(\d+) --> (\d+):(\d+):(\d+),(\d+)\n([^\n]*)`)

// cueStyle matches the style marks ffmpeg writes around ASS text when it renders SubRip.
var cueStyle = regexp.MustCompile( //nolint:gochecknoglobals // Not modified, simplifies testing.
	`<[^>]*>|\{[^}]*\}`)

// cueClock turns the four matched clock fields into a duration.
func cueClock(fields []string) time.Duration {
	parts := make([]int, len(fields))
	for i, f := range fields {
		parts[i], _ = strconv.Atoi(f)
	}
	return time.Duration(parts[0])*time.Hour + time.Duration(parts[1])*time.Minute +
		time.Duration(parts[2])*time.Second + time.Duration(parts[3])*time.Millisecond
}

// proveSubtitles compares each subtitle stream's kind and text with the recipe's.
func proveSubtitles(ctx context.Context, path string, info probe.Info, truth Truth) []string {
	streams := info.Kind("subtitle")
	if len(streams) != len(truth.Subtitles) {
		return []string{fmt.Sprintf("subtitle tracks: got %d, want %d",
			len(streams), len(truth.Subtitles))}
	}
	var problems []string
	for i, want := range truth.Subtitles {
		if streams[i].Codec != want.Codec || streams[i].Tags.Language != want.Language {
			problems = append(problems, fmt.Sprintf("subtitle %d: got %s %q, want %s %q",
				i, streams[i].Codec, streams[i].Tags.Language, want.Codec, want.Language))
		}
		out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-nostdin", "-v", "error",
			"-i", path, "-map", fmt.Sprintf("0:s:%d", i), "-c:s", "srt", "-f", "srt", "-").Output()
		if err != nil {
			return []string{fmt.Sprintf("extract subtitle %d: %v", i, err)}
		}
		var got []Line
		for _, m := range srtCue.FindAllStringSubmatch(strings.ReplaceAll(string(out), "\r", ""), -1) {
			got = append(got, Line{cueClock(m[1:5]), cueClock(m[5:9]), cueStyle.ReplaceAllString(m[9], "")})
		}
		if diff := cmp.Diff(want.Lines, got); diff != "" {
			problems = append(problems, fmt.Sprintf("subtitle %d cues (-want +got):\n%s", i, diff))
		}
	}
	return problems
}

// probedContainer holds the container facts sim's tests read beyond package probe.
type probedContainer struct {
	// Chapters lists the chapter marks.
	Chapters []probedChapter `json:"chapters"`
	// Format holds the container tags.
	Format struct {
		// Tags maps tag names to values.
		Tags map[string]string `json:"tags"`
	} `json:"format"`
}

// probedChapter is one chapter as ffprobe prints it.
type probedChapter struct {
	// Start is the chapter's start in seconds.
	Start string `json:"start_time"`
	// End is the chapter's end in seconds.
	End string `json:"end_time"`
	// Tags holds the chapter's title.
	Tags struct {
		// Title is the chapter's name.
		Title string `json:"title"`
	} `json:"tags"`
}

// proveContainer compares chapters and container tags with the recipe's.
func proveContainer(ctx context.Context, path string, truth Truth) []string {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_chapters",
		"-show_entries", "format_tags", "-of", "json", path).Output()
	if err != nil {
		return []string{fmt.Sprintf("ffprobe container: %v", err)}
	}
	var got probedContainer
	if err := json.Unmarshal(out, &got); err != nil {
		return []string{fmt.Sprintf("parse container: %v", err)}
	}
	var problems []string
	var marks []Chapter
	for _, c := range got.Chapters {
		start, _ := strconv.ParseFloat(c.Start, 64)
		end, _ := strconv.ParseFloat(c.End, 64)
		marks = append(marks, Chapter{
			time.Duration(start * float64(time.Second)).Round(time.Millisecond),
			time.Duration(end * float64(time.Second)).Round(time.Millisecond),
			c.Tags.Title,
		})
	}
	if diff := cmp.Diff(truth.Chapters, marks, cmpopts.EquateEmpty()); diff != "" {
		problems = append(problems, fmt.Sprintf("chapters (-want +got):\n%s", diff))
	}
	tags := make(map[string]string, len(got.Format.Tags))
	for name, value := range got.Format.Tags {
		tags[strings.ToLower(name)] = value
	}
	for name, want := range truth.Tags {
		if tags[name] != want {
			problems = append(problems, fmt.Sprintf("tag %s: got %q, want %q", name, tags[name], want))
		}
	}
	return problems
}

// proveDamage checks that a broken file fails the way the recipe says, and only that way.
func proveDamage(ctx context.Context, path string, truth Truth) []string {
	_, probeErr := probe.File(ctx, path)
	if truth.Damage == DamageProbe {
		if probeErr == nil {
			return []string{"damage: probe read a file that should fail"}
		}
		return nil
	}
	if probeErr != nil {
		return []string{fmt.Sprintf("damage: probe failed on a file that should read: %v", probeErr)}
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-nostdin", "-v", "error", "-xerror",
		"-i", path, "-f", "null", "-")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil && stderr.Len() == 0 {
		return []string{"damage: decode finished cleanly on a file that should fail"}
	}
	return nil
}

// prove runs every proof the Truth calls for and returns what did not hold.
func prove(ctx context.Context, path string, truth Truth) []string {
	if truth.Damage != DamageNone {
		return proveDamage(ctx, path, truth)
	}
	info, err := probe.File(ctx, path)
	if err != nil {
		return []string{fmt.Sprintf("probe: %v", err)}
	}
	var problems []string
	if truth.VideoCodec != "" {
		problems = append(problems, proveVideo(info, truth)...)
		problems = append(problems, proveFrames(ctx, path, truth)...)
	}
	problems = append(problems, proveAudio(info, truth)...)
	problems = append(problems, proveTones(ctx, path, truth)...)
	problems = append(problems, proveMarkers(ctx, path, truth)...)
	problems = append(problems, proveComb(ctx, path, truth)...)
	problems = append(problems, proveMastering(ctx, path, truth)...)
	problems = append(problems, proveSubtitles(ctx, path, info, truth)...)
	problems = append(problems, proveContainer(ctx, path, truth)...)
	return problems
}

// treeSize returns how many bytes the files under dir hold.
func treeSize(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return total
}

func TestRecipes(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	root := t.TempDir()
	t.Run("all", func(t *testing.T) {
		for _, recipe := range Recipes() {
			t.Run(recipe.Name, func(t *testing.T) {
				t.Parallel()
				dir := filepath.Join(root, recipe.Name)
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				path, truth, err := Generate(t.Context(), recipe, dir)
				if err != nil {
					t.Fatalf("Generate: %v", err)
				}
				if diff := cmp.Diff(filepath.Join(dir, recipe.File), path); diff != "" {
					t.Errorf("path mismatch (-want +got):\n%s", diff)
				}
				for _, problem := range prove(t.Context(), path, truth) {
					t.Error(problem)
				}
			})
		}
	})
	size := treeSize(t, root)
	t.Logf("recipes wrote %d bytes", size)
	if size > scratchLimit {
		t.Errorf("recipes wrote %d bytes, over the %d limit", size, scratchLimit)
	}
}

// TestProveCatchesWrongTruth holds the proofs to account: each file is sound, and each Truth is
// wrong in one way the proofs must name.
func TestProveCatchesWrongTruth(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	tests := []struct {
		Name       string
		Recipe     Recipe
		Mutate     func(*Truth)
		WantNeedle string
	}{{ // Test 0: A constant rate file fails against the two rate Truth.
		Name:       "one rate against two",
		Recipe:     Variable(24, 24),
		Mutate:     func(tr *Truth) { *tr = Variable(24, 60).Truth },
		WantNeedle: "frames: got 144, want 252",
	}, { // Test 1: A frame count one short.
		Name:   "frame count",
		Recipe: noAudio(),
		Mutate: func(tr *Truth) {
			tr.Segments = []Segment{{FPS: clipRate, Frames: clipFrames - 1}}
		},
		WantNeedle: "frames: got 48, want 47",
	}, { // Test 2: Evenly spaced frames against a slower rate.
		Name:       "frame times",
		Recipe:     noAudio(),
		Mutate:     func(tr *Truth) { tr.Segments = []Segment{{FPS: 25, Frames: clipFrames}} },
		WantNeedle: "frame 2:",
	}, { // Test 3: A wrong size.
		Name:       "size",
		Recipe:     odd(),
		Mutate:     func(tr *Truth) { tr.Width = 640 },
		WantNeedle: "size: got 641x361, want 640x361",
	}, { // Test 4: A wrong turn.
		Name:       "rotation",
		Recipe:     rotated(90),
		Mutate:     func(tr *Truth) { tr.Rotation = 270 },
		WantNeedle: "rotation: got 90, want 270",
	}, { // Test 5: Channels swapped.
		Name:   "channel tones",
		Recipe: tonesStereo(),
		Mutate: func(tr *Truth) {
			tr.Audio = []AudioTrack{
				{Codec: "flac", Channels: 2, SampleRate: sampleRate, Tones: []float64{880, 440}},
			}
		},
		WantNeedle: "channel 0 tone",
	}, { // Test 6: Markers moved a full 200 ms.
		Name:   "marker times",
		Recipe: markers(),
		Mutate: func(tr *Truth) {
			for i := range tr.Markers {
				tr.Markers[i] += 200 * time.Millisecond
			}
		},
		WantNeedle: "flash 0:",
	}, { // Test 7: Progressive footage called interlaced.
		Name:       "comb",
		Recipe:     gradientsClean(),
		Mutate:     func(tr *Truth) { tr.FieldOrder = "tt" },
		WantNeedle: "field order",
	}, { // Test 8: Interlaced footage called progressive by idet alone.
		Name:       "comb reading",
		Recipe:     interlaced("interlaced-x", "top", "tff", "tt"),
		Mutate:     func(tr *Truth) { tr.FieldOrder = "" },
		WantNeedle: "idet:",
	}, { // Test 9: Mastering metadata the file does not hold.
		Name:       "mastering",
		Recipe:     hdrHLG(),
		Mutate:     func(tr *Truth) { tr.Mastering = true },
		WantNeedle: "mastering metadata missing",
	}, { // Test 10: A cue with the wrong words.
		Name:       "subtitle text",
		Recipe:     subs(),
		Mutate:     func(tr *Truth) { tr.Subtitles[0].Lines[0].Text = "Wrong." },
		WantNeedle: "subtitle 0 cues",
	}, { // Test 11: A chapter that starts elsewhere.
		Name:       "chapter",
		Recipe:     chapters(),
		Mutate:     func(tr *Truth) { tr.Chapters[1].Start = 600 * time.Millisecond },
		WantNeedle: "chapters",
	}, { // Test 12: A tag the file does not hold.
		Name:       "tag",
		Recipe:     metadata(),
		Mutate:     func(tr *Truth) { tr.Tags["title"] = "Other" },
		WantNeedle: "tag title",
	}, { // Test 13: A language that differs.
		Name:       "language",
		Recipe:     multiAudio(),
		Mutate:     func(tr *Truth) { tr.Audio[1].Language = "deu" },
		WantNeedle: "audio 1:",
	}, { // Test 14: A cut file called readable.
		Name:       "probe damage reads",
		Recipe:     truncatedMiddle(),
		Mutate:     func(tr *Truth) { tr.Damage = DamageProbe },
		WantNeedle: "damage: probe read",
	}, { // Test 15: A file that cannot be read called merely damaged in decode.
		Name:       "decode damage cannot probe",
		Recipe:     zero(),
		Mutate:     func(tr *Truth) { tr.Damage = DamageDecode },
		WantNeedle: "damage: probe failed",
	}, { // Test 16: A whole file called damaged.
		Name:       "whole file called damaged",
		Recipe:     noAudio(),
		Mutate:     func(tr *Truth) { tr.Damage = DamageDecode },
		WantNeedle: "damage: decode finished cleanly",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			path, truth, err := Generate(t.Context(), test.Recipe, t.TempDir())
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			test.Mutate(&truth)
			problems := prove(t.Context(), path, truth)
			if !slices.ContainsFunc(problems, func(p string) bool {
				return strings.Contains(p, test.WantNeedle)
			}) {
				t.Errorf("problems %q do not name %q", problems, test.WantNeedle)
			}
		})
	}
}

func TestLong(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	path, truth, err := Generate(t.Context(), Long("long-short", 3), t.TempDir())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, problem := range prove(t.Context(), path, truth) {
		t.Error(problem)
	}
}

func TestLongRecipes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		WantLength time.Duration
		WantFrames int
	}{{ // Test 0: The deep tier's clip.
		Name: "long20m", WantLength: 20 * time.Minute, WantFrames: 14400,
	}, { // Test 1: The exhaustive tier's clip.
		Name: "long2h", WantLength: 2 * time.Hour, WantFrames: 86400,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			recipe, ok := Find(test.Name)
			if !ok {
				t.Fatalf("Find(%q) found nothing", test.Name)
			}
			if diff := cmp.Diff(test.WantLength, recipe.Truth.Duration); diff != "" {
				t.Errorf("length mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantFrames, recipe.Truth.Frames()); diff != "" {
				t.Errorf("frames mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRecipeNamesAreUnique(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, r := range slices.Concat(Recipes(), LongRecipes()) {
		if seen[r.Name] {
			t.Errorf("recipe name %q appears twice", r.Name)
		}
		seen[r.Name] = true
	}
}

func TestFindMissing(t *testing.T) {
	t.Parallel()
	if _, ok := Find("no such recipe"); ok {
		t.Error("Find found a recipe that does not exist")
	}
}

func TestTruth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Truth       Truth
		WantTimes   []time.Duration
		WantFrames  int
		WantDisplay [2]int
	}{{ // Test 0: No video.
		Truth: Truth{}, WantTimes: nil, WantFrames: 0, WantDisplay: [2]int{0, 0},
	}, { // Test 1: One run.
		Truth:      Truth{Segments: []Segment{{FPS: 10, Frames: 2}}, Width: 4, Height: 2},
		WantTimes:  []time.Duration{0, 100 * time.Millisecond},
		WantFrames: 2, WantDisplay: [2]int{4, 2},
	}, { // Test 2: A second run starts where the first run ends, and a quarter turn swaps the size.
		Truth: Truth{
			Segments: []Segment{{FPS: 10, Frames: 2}, {FPS: 20, Frames: 2}},
			Width:    4, Height: 2, Rotation: 270,
		},
		WantTimes: []time.Duration{
			0, 100 * time.Millisecond, 200 * time.Millisecond, 250 * time.Millisecond,
		},
		WantFrames: 4, WantDisplay: [2]int{2, 4},
	}, { // Test 3: A half turn keeps the size.
		Truth: Truth{Width: 4, Height: 2, Rotation: 180}, WantDisplay: [2]int{4, 2},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantFrames, test.Truth.Frames()); diff != "" {
				t.Errorf("frames mismatch (-want +got):\n%s", diff)
			}
			got := test.Truth.FrameTimes()
			if diff := cmp.Diff(test.WantTimes, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("times mismatch (-want +got):\n%s", diff)
			}
			w, h := test.Truth.DisplaySize()
			if diff := cmp.Diff(test.WantDisplay, [2]int{w, h}); diff != "" {
				t.Errorf("display mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestClocks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In      time.Duration
		WantSRT string
		WantASS string
	}{{ // Test 0: Zero.
		In: 0, WantSRT: "00:00:00,000", WantASS: "0:00:00.00",
	}, { // Test 1: Every field in use.
		In:      time.Hour + 2*time.Minute + 3*time.Second + 450*time.Millisecond,
		WantSRT: "01:02:03,450", WantASS: "1:02:03.45",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantSRT, srtClock(test.In)); diff != "" {
				t.Errorf("SubRip mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantASS, assClock(test.In)); diff != "" {
				t.Errorf("ASS mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGenerateFails(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg")
	file := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	badStep := Recipe{
		Name: "bad", File: "bad.mkv",
		Steps: func(_, out string) [][]string { return [][]string{{"-no-such-option", out}} },
	}
	tests := []struct {
		Ctx         context.Context //nolint:containedctx // Test table.
		Dir         string
		Bin         string
		Recipe      Recipe
		WantMessage string
		Name        string
	}{{ // Test 0: The folder does not exist.
		Name: "missing folder", Ctx: t.Context(), Dir: filepath.Join(file, "x"), Bin: "ffmpeg",
		Recipe: noAudio(), WantMessage: "noaudio",
	}, { // Test 1: The folder is a file.
		Name: "folder is a file", Ctx: t.Context(), Dir: file, Bin: "ffmpeg",
		Recipe: noAudio(), WantMessage: "is not a folder",
	}, { // Test 2: The tool is not there.
		Name: "missing tool", Ctx: t.Context(), Bin: "icelos-no-such-tool",
		Recipe: noAudio(), WantMessage: "ffmpeg step",
	}, { // Test 3: ffmpeg refuses the arguments and its reason is kept.
		Name: "bad step", Ctx: t.Context(), Bin: "ffmpeg",
		Recipe: badStep, WantMessage: "no-such-option",
	}, { // Test 4: The context is already over.
		Name: "canceled", Ctx: canceled, Bin: "ffmpeg", Recipe: noAudio(), WantMessage: "ffmpeg step",
	}, { // Test 5: A sidecar cannot be written.
		Name: "sidecar", Ctx: t.Context(), Bin: "ffmpeg",
		Recipe: Recipe{
			Name: "sidecar", File: "x.mkv",
			Sidecars: map[string]string{filepath.Join("no-such", "x.txt"): "text"},
		}, WantMessage: "sidecar",
	}, { // Test 6: Finish fails.
		Name: "finish", Ctx: t.Context(), Bin: "ffmpeg",
		Recipe: Recipe{
			Name: "finish", File: "x.mkv",
			Finish: func(string) error { return errors.New("no room") },
		}, WantMessage: "finish: no room",
	}, { // Test 7: The steps ran and wrote nothing.
		Name: "no file", Ctx: t.Context(), Bin: "ffmpeg",
		Recipe: Recipe{Name: "nothing", File: "x.mkv"}, WantMessage: "nothing",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			dir := test.Dir
			if dir == "" {
				dir = t.TempDir()
			}
			_, _, err := generate(test.Ctx, test.Bin, test.Recipe, dir)
			if !errors.Is(err, ErrSim) {
				t.Fatalf("error %v is not ErrSim", err)
			}
			if !strings.Contains(err.Error(), test.WantMessage) {
				t.Errorf("error %q does not hold %q", err, test.WantMessage)
			}
		})
	}
}

func TestCutTo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Bytes       int
		Size        int64
		WantMessage string
		WantBytes   int64
	}{{ // Test 0: A cut shortens the file.
		Name: "cut", Bytes: 10, Size: 4, WantBytes: 4,
	}, { // Test 1: A file already as short as the cut would damage nothing.
		Name: "too short", Bytes: 4, Size: 4, WantMessage: "too short to cut", WantBytes: 4,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, make([]byte, test.Bytes), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			err := cutTo(path, test.Size)
			if test.WantMessage == "" && err != nil {
				t.Fatalf("cutTo: %v", err)
			}
			if test.WantMessage != "" &&
				(err == nil || !strings.Contains(err.Error(), test.WantMessage)) {
				t.Fatalf("error %v does not hold %q", err, test.WantMessage)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if diff := cmp.Diff(test.WantBytes, info.Size()); diff != "" {
				t.Errorf("size mismatch (-want +got):\n%s", diff)
			}
		})
	}
	if err := cutTo(filepath.Join(t.TempDir(), "missing"), 1); err == nil {
		t.Error("cutTo on a missing file returned no error")
	}
}
