package sim

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// clipSeconds is how long most recipes play.
	clipSeconds = 2
	// clipRate is the frame rate of most recipes.
	clipRate = 24
	// clipFrames is how many frames most recipes hold.
	clipFrames = clipSeconds * clipRate
	// clipLength is how long most recipes play, as a duration.
	clipLength = clipSeconds * time.Second
	// sampleRate is the sample rate of every generated audio track.
	sampleRate = 48000
	// truncatedCut is where the moov-last truncated recipe is cut, in bytes.
	truncatedCut = 200000
)

// lavfi returns the input arguments for a lavfi graph.
func lavfi(graph string) []string {
	return []string{"-f", "lavfi", "-i", graph}
}

// picture returns a testsrc2 graph of the given size, rate, and seconds.
func picture(width, height, rate, seconds int) string {
	return fmt.Sprintf("testsrc2=size=%dx%d:rate=%d:duration=%d", width, height, rate, seconds)
}

// smallPicture returns the graph most recipes use, followed by any extra filters.
func smallPicture(filters ...string) string {
	return strings.Join(append([]string{picture(320, 180, clipRate, clipSeconds)}, filters...), ",")
}

// tones returns an aevalsrc graph with one sine per channel, lasting seconds.
func tones(layout string, seconds int, freqs ...float64) string {
	exprs := make([]string, 0, len(freqs))
	for _, f := range freqs {
		exprs = append(exprs, fmt.Sprintf("0.5*sin(2*PI*%g*t)", f))
	}
	return fmt.Sprintf("aevalsrc=%s:c=%s:s=%d:d=%d",
		strings.Join(exprs, "|"), layout, sampleRate, seconds)
}

// x264 returns the arguments for a clean, fast H.264 encode in pixel format pix.
func x264(pix string) []string {
	return []string{"-c:v", "libx264", "-preset", "ultrafast", "-crf", "12", "-pix_fmt", pix}
}

// x265 returns the arguments for a fast HEVC encode in pixel format pix, with params given to
// the encoder after its log setting.
func x265(pix, params string) []string {
	full := "log-level=error"
	if params != "" {
		full += ":" + params
	}
	return []string{
		"-c:v", "libx265", "-preset", "ultrafast", "-crf", "16", "-pix_fmt", pix,
		"-x265-params", full, "-tag:v", "hvc1",
	}
}

// single builds a recipe from one ffmpeg command made of parts, ending at the final path.
func single(name, file string, truth Truth, parts ...[]string) Recipe {
	return Recipe{
		Name: name,
		File: file,
		Steps: func(_, out string) [][]string {
			return [][]string{append(slices.Concat(parts...), out)}
		},
		Truth: truth,
	}
}

// smallTruth returns the Truth of the small H.264 clip most recipes start from.
func smallTruth() Truth {
	return Truth{
		Duration:     clipLength,
		VideoCodec:   "h264",
		Segments:     []Segment{{FPS: clipRate, Frames: clipFrames}},
		Width:        320,
		Height:       180,
		SampleAspect: "1:1",
		FieldOrder:   "progressive",
		PixFmt:       "yuv420p",
	}
}

// Recipes returns every recipe short enough to generate on each run of the tests.
func Recipes() []Recipe {
	return slices.Concat(
		[]Recipe{markers(), tonesStereo(), tones51(), Variable(24, 60), variableMatroska()},
		[]Recipe{odd(), anamorphic()},
		[]Recipe{
			gradientsClean(), interlaced("interlaced-tt", "top", "tff", "tt"),
			interlaced("interlaced-bb", "bottom", "bff", "bb"),
		},
		[]Recipe{rotated(90), rotated(180), rotated(270)},
		[]Recipe{tenBit(), hdrHLG(), hdrPQ(), multiAudio(), noAudio(), audioOnly()},
		[]Recipe{subs(), chapters(), metadata()},
		[]Recipe{truncatedTail(), truncatedMiddle(), zero(), notMedia()},
	)
}

// LongRecipes returns the recipes that take minutes to generate, for the slower test tiers.
func LongRecipes() []Recipe {
	return []Recipe{Long("long20m", 20*60), Long("long2h", 2*60*60)}
}

// markers is a picture and a sound that flash and sound together for the first 100 ms of each
// second, which pins picture and sound to known times.
func markers() Recipe {
	const seconds, length = 4, 100 * time.Millisecond
	flash := "drawbox=x=0:y=0:w=iw:h=ih:color=white:t=fill:enable='lt(mod(t\\,1)\\,0.1)'"
	burst := "if(lt(mod(t\\,1)\\,0.1)\\,0.8*sin(2*PI*1000*t)\\,0)"
	truth := Truth{
		Duration:     seconds * time.Second,
		VideoCodec:   "h264",
		Segments:     []Segment{{FPS: clipRate, Frames: seconds * clipRate}},
		Width:        640,
		Height:       360,
		SampleAspect: "1:1",
		FieldOrder:   "progressive",
		PixFmt:       "yuv420p",
		Audio:        []AudioTrack{{Codec: "flac", Channels: 2, SampleRate: sampleRate}},
		MarkerLength: length,
	}
	for i := range seconds {
		truth.Markers = append(truth.Markers, time.Duration(i)*time.Second)
	}
	return single("markers", "markers.mkv", truth,
		lavfi(picture(640, 360, clipRate, seconds)+","+flash),
		lavfi(fmt.Sprintf("aevalsrc=%s|%s:c=stereo:s=%d:d=%d", burst, burst, sampleRate, seconds)),
		x264("yuv420p"), []string{"-c:a", "flac"})
}

// tonesStereo holds a different tone on each stereo channel.
func tonesStereo() Recipe {
	freqs := []float64{440, 880}
	truth := smallTruth()
	truth.Audio = []AudioTrack{{Codec: "flac", Channels: 2, SampleRate: sampleRate, Tones: freqs}}
	return single("tones-stereo", "tones-stereo.mkv", truth,
		lavfi(smallPicture()), lavfi(tones("stereo", clipSeconds, freqs...)),
		x264("yuv420p"), []string{"-c:a", "flac"})
}

// tones51 holds a different tone on each of six channels, the low-frequency one inside the band
// a surround encoder keeps for it.
func tones51() Recipe {
	freqs := []float64{300, 500, 700, 60, 900, 1100}
	truth := smallTruth()
	truth.Audio = []AudioTrack{{Codec: "flac", Channels: 6, SampleRate: sampleRate, Tones: freqs}}
	return single("tones-51", "tones-51.mkv", truth,
		lavfi(smallPicture()), lavfi(tones("5.1", clipSeconds, freqs...)),
		x264("yuv420p"), []string{"-c:a", "flac"})
}

// Variable holds three seconds at the first rate and three at the second, with no gap between
// them. It picks frames from one faster graph, so the times are exact and no concat can mangle
// them.
func Variable(first, second int) Recipe {
	const part = 3
	base := first * second / gcd(first, second)
	pick := fmt.Sprintf(`select='if(lt(t\,%d)\,not(mod(n\,%d))\,not(mod(n\,%d)))'`,
		part, base/first, base/second)
	truth := Truth{
		Duration:     2 * part * time.Second,
		VideoCodec:   "h264",
		Segments:     []Segment{{FPS: first, Frames: part * first}, {FPS: second, Frames: part * second}},
		Width:        320,
		Height:       180,
		SampleAspect: "1:1",
		FieldOrder:   "progressive",
		PixFmt:       "yuv420p",
		Audio:        []AudioTrack{{Codec: "aac", Channels: 1, SampleRate: sampleRate}},
	}
	return single("vfr", "vfr.mp4", truth,
		lavfi(picture(320, 180, base, 2*part)+","+pick),
		lavfi(fmt.Sprintf("sine=frequency=440:sample_rate=%d:duration=%d", sampleRate, 2*part)),
		[]string{"-fps_mode", "passthrough", "-enc_time_base", "filter"},
		x264("yuv420p"), []string{"-c:a", "aac", "-b:a", "128k"})
}

// variableMatroska holds the same frames as Variable(24, 60) in Matroska, which states no frame
// rate, so ffmpeg guesses one and an encoder given that guess drops frames.
func variableMatroska() Recipe {
	base := Variable(24, 60)
	return Recipe{
		Name: "vfr-mkv",
		File: "vfr-mkv.mkv",
		Steps: func(dir, out string) [][]string {
			first := filepath.Join(dir, "vfr-mkv-base.mp4")
			return append(base.Steps(dir, first), []string{"-i", first, "-c", "copy", out})
		},
		Truth: base.Truth,
	}
}

// gcd returns the greatest common divisor of two positive numbers.
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// odd has an odd width and height, which only full chroma can hold, since lavfi rounds a
// subsampled size to even.
func odd() Recipe {
	truth := smallTruth()
	truth.Width, truth.Height, truth.PixFmt = 641, 361, "yuv444p"
	return single("odd", "odd.mkv", truth,
		lavfi(picture(642, 362, clipRate, clipSeconds)+",format=yuv444p,crop=641:361"),
		x264("yuv444p"))
}

// anamorphic is a PAL MPEG-2 frame whose pixels are wider than they are tall.
func anamorphic() Recipe {
	return single("anamorphic", "anamorphic.mpg", Truth{
		Duration:     clipLength,
		VideoCodec:   "mpeg2video",
		Segments:     []Segment{{FPS: 25, Frames: clipSeconds * 25}},
		Width:        720,
		Height:       576,
		SampleAspect: "64:45",
		FieldOrder:   "progressive",
		PixFmt:       "yuv420p",
		Audio:        []AudioTrack{{Codec: "mp2", Channels: 1, SampleRate: sampleRate}},
	},
		lavfi(picture(720, 576, 25, clipSeconds)+",setsar=64/45"),
		lavfi(fmt.Sprintf("sine=frequency=440:sample_rate=%d:duration=%d", sampleRate, clipSeconds)),
		[]string{"-c:v", "mpeg2video", "-q:v", "2", "-c:a", "mp2", "-b:a", "128k"})
}

// gradientsClean is progressive footage from gradients, whose idet reading is the control for
// the interlaced recipes.
func gradientsClean() Recipe {
	truth := smallTruth()
	truth.Width, truth.Height, truth.CombProof = 640, 360, true
	return single("gradients", "gradients.mkv", truth,
		lavfi(fmt.Sprintf("gradients=size=640x360:rate=%d:duration=%d:speed=0.5",
			clipRate, clipSeconds)),
		x264("yuv420p"))
}

// interlaced is truly interlaced gradients footage with the first field given by field and the
// tag ffprobe reads given by order. It uses gradients because idet misreads testsrc2.
func interlaced(name, field, first, order string) Recipe {
	return single(name, name+".ts", Truth{
		Duration:     clipLength,
		VideoCodec:   "mpeg2video",
		Segments:     []Segment{{FPS: 25, Frames: clipSeconds * 25}},
		Width:        720,
		Height:       576,
		FieldOrder:   order,
		CombProof:    true,
		SampleAspect: "1:1",
		PixFmt:       "yuv420p",
	},
		lavfi(fmt.Sprintf("gradients=size=720x576:rate=50:duration=%d:speed=0.5,"+
			"tinterlace=interleave_%s,setfield=%s", clipSeconds, field, first)),
		[]string{"-c:v", "mpeg2video", "-q:v", "2", "-flags", "+ilme+ildct"})
}

// rotated is a landscape clip whose stored turn says to show it rotated by degrees.
func rotated(degrees int) Recipe {
	truth := smallTruth()
	truth.Rotation = degrees
	name := fmt.Sprintf("rotated-%d", degrees)
	return Recipe{
		Name: name,
		File: name + ".mp4",
		Steps: func(dir, out string) [][]string {
			base := filepath.Join(dir, name+"-base.mp4")
			return [][]string{
				append(slices.Concat(lavfi(smallPicture()), x264("yuv420p")), base),
				{"-display_rotation", fmt.Sprint(degrees), "-i", base, "-c", "copy", out},
			}
		},
		Truth: truth,
	}
}

// tenBit is 10-bit HEVC.
func tenBit() Recipe {
	truth := smallTruth()
	truth.VideoCodec, truth.PixFmt = "hevc", "yuv420p10le"
	return single("tenbit", "tenbit.mp4", truth,
		lavfi(smallPicture()), x265("yuv420p10le", ""))
}

// hdrHLG is 10-bit HEVC tagged with wide gamut primaries and the HLG transfer curve.
func hdrHLG() Recipe {
	truth := smallTruth()
	truth.VideoCodec, truth.PixFmt = "hevc", "yuv420p10le"
	truth.Color = Color{"tv", "bt2020nc", "arib-std-b67", "bt2020"}
	return single("hdr-hlg", "hdr-hlg.mp4", truth,
		lavfi(smallPicture("format=yuv420p10le",
			"setparams=range=tv:colorspace=bt2020nc:color_trc=arib-std-b67:color_primaries=bt2020")),
		x265("yuv420p10le", ""))
}

// hdrPQ is 10-bit HEVC tagged with the PQ transfer curve and carrying mastering metadata.
func hdrPQ() Recipe {
	truth := smallTruth()
	truth.VideoCodec, truth.PixFmt, truth.Mastering = "hevc", "yuv420p10le", true
	truth.Color = Color{"tv", "bt2020nc", "smpte2084", "bt2020"}
	master := "hdr10=1:max-cll=1000,400:master-display=" +
		"G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)"
	return single("hdr-pq", "hdr-pq.mp4", truth,
		lavfi(smallPicture("format=yuv420p10le",
			"setparams=range=tv:colorspace=bt2020nc:color_trc=smpte2084:color_primaries=bt2020")),
		x265("yuv420p10le", master))
}

// multiAudio holds three sound tracks in three languages, each channel with its own tone.
func multiAudio() Recipe {
	langs := []string{"eng", "spa", "fra"}
	freqs := [][]float64{{300, 400}, {500, 600}, {700, 800}}
	truth := smallTruth()
	parts := [][]string{lavfi(smallPicture())}
	maps := []string{"-map", "0"}
	for i, lang := range langs {
		truth.Audio = append(truth.Audio, AudioTrack{
			Codec: "flac", Language: lang, Channels: 2, SampleRate: sampleRate, Tones: freqs[i],
		})
		parts = append(parts, lavfi(tones("stereo", clipSeconds, freqs[i]...)))
		maps = append(maps, "-map", fmt.Sprint(i+1))
	}
	parts = append(parts, maps, x264("yuv420p"), []string{"-c:a", "flac"})
	for i, lang := range langs {
		parts = append(parts, []string{fmt.Sprintf("-metadata:s:a:%d", i), "language=" + lang})
	}
	return single("multiaudio", "multiaudio.mkv", truth, parts...)
}

// noAudio holds picture only.
func noAudio() Recipe {
	return single("noaudio", "noaudio.mp4", smallTruth(), lavfi(smallPicture()), x264("yuv420p"))
}

// audioOnly holds sound only.
func audioOnly() Recipe {
	freqs := []float64{440, 880}
	return single("audioonly", "audioonly.flac", Truth{
		Duration: clipLength,
		Audio:    []AudioTrack{{Codec: "flac", Channels: 2, SampleRate: sampleRate, Tones: freqs}},
	}, lavfi(tones("stereo", clipSeconds, freqs...)), []string{"-c:a", "flac"})
}

// subs holds two SubRip tracks and an ASS track with known text at known times.
func subs() Recipe {
	english := []Line{
		{500 * time.Millisecond, 900 * time.Millisecond, "Form is never final."},
		{time.Second, 1500 * time.Millisecond, "Second line."},
	}
	spanish := []Line{
		{250 * time.Millisecond, 750 * time.Millisecond, "La forma nunca es definitiva."},
		{1250 * time.Millisecond, 1750 * time.Millisecond, "Segunda línea."},
	}
	japanese := []Line{
		{300 * time.Millisecond, 1100 * time.Millisecond, "形は決して最終ではない。"},
	}
	truth := smallTruth()
	truth.Subtitles = []SubtitleTrack{
		{Codec: "subrip", Language: "eng", Lines: english},
		{Codec: "subrip", Language: "spa", Lines: spanish},
		{Codec: "ass", Language: "jpn", Lines: japanese},
	}
	return Recipe{
		Name: "subs",
		File: "subs.mkv",
		Sidecars: map[string]string{
			"subs-en.srt": subRip(english),
			"subs-es.srt": subRip(spanish),
			"subs-ja.ass": assScript(japanese),
		},
		Steps: func(dir, out string) [][]string {
			return [][]string{append(slices.Concat(lavfi(smallPicture()),
				[]string{
					"-i", filepath.Join(dir, "subs-en.srt"),
					"-i", filepath.Join(dir, "subs-es.srt"),
					"-i", filepath.Join(dir, "subs-ja.ass"),
					"-map", "0", "-map", "1", "-map", "2", "-map", "3",
				},
				x264("yuv420p"),
				[]string{
					"-c:s:0", "srt", "-c:s:1", "srt", "-c:s:2", "ass",
					"-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=spa",
					"-metadata:s:s:2", "language=jpn",
				},
			), out)}
		},
		Truth: truth,
	}
}

// subRip renders lines as a SubRip file.
func subRip(lines []Line) string {
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtClock(l.Start), srtClock(l.End), l.Text)
	}
	return b.String()
}

// srtClock renders d the way SubRip writes a time.
func srtClock(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// assScript renders lines as a minimal ASS file.
func assScript(lines []Line) string {
	var b strings.Builder
	b.WriteString("[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\n")
	b.WriteString("Format: Name, Fontname, Fontsize, PrimaryColour\n" +
		"Style: Default,Arial,20,&H00FFFFFF\n\n[Events]\n")
	b.WriteString("Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")
	for _, l := range lines {
		fmt.Fprintf(&b, "Dialogue: 0,%s,%s,Default,,0,0,0,,%s\n", //nolint:misspell // ASS keyword.
			assClock(l.Start), assClock(l.End), l.Text)
	}
	return b.String()
}

// assClock renders d the way ASS writes a time, in hundredths of a second.
func assClock(d time.Duration) string {
	cs := d.Milliseconds() / 10
	return fmt.Sprintf("%d:%02d:%02d.%02d", cs/360000, cs/6000%60, cs/100%60, cs%100)
}

// chapters holds four chapters of half a second each.
func chapters() Recipe {
	marks := []Chapter{
		{0, 500 * time.Millisecond, "One"},
		{500 * time.Millisecond, time.Second, "Two"},
		{time.Second, 1500 * time.Millisecond, "Three"},
		{1500 * time.Millisecond, clipLength, "Four"},
	}
	var meta strings.Builder
	meta.WriteString(";FFMETADATA1\n")
	for _, c := range marks {
		fmt.Fprintf(&meta, "[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=%s\n",
			c.Start.Milliseconds(), c.End.Milliseconds(), c.Title)
	}
	truth := smallTruth()
	truth.Chapters = marks
	truth.Audio = []AudioTrack{{Codec: "flac", Channels: 1, SampleRate: sampleRate}}
	return Recipe{
		Name:     "chapters",
		File:     "chapters.mkv",
		Sidecars: map[string]string{"chapters.txt": meta.String()},
		Steps: func(dir, out string) [][]string {
			sound := fmt.Sprintf("sine=frequency=440:sample_rate=%d:duration=%d", sampleRate, clipSeconds)
			return [][]string{append(slices.Concat(lavfi(smallPicture()), lavfi(sound),
				[]string{"-i", filepath.Join(dir, "chapters.txt"), "-map", "0", "-map", "1", "-map_chapters", "2"},
				x264("yuv420p"), []string{"-c:a", "flac"},
			), out)}
		},
		Truth: truth,
	}
}

// metadata holds a title, an artist, and a language tag on its sound track.
func metadata() Recipe {
	truth := smallTruth()
	truth.Audio = []AudioTrack{{Codec: "aac", Language: "eng", Channels: 1, SampleRate: sampleRate}}
	truth.Tags = map[string]string{"title": "Form is never final 形", "artist": "The Dream God"}
	return single("metadata", "metadata.mp4", truth,
		lavfi(smallPicture()),
		lavfi(fmt.Sprintf("sine=frequency=440:sample_rate=%d:duration=%d", sampleRate, clipSeconds)),
		x264("yuv420p"),
		[]string{
			"-c:a", "aac", "-b:a", "128k",
			"-metadata", "title=" + truth.Tags["title"], "-metadata", "artist=" + truth.Tags["artist"],
			"-metadata:s:a:0", "language=eng",
		})
}

// Long is a small clip of the given length in seconds, for the slower test tiers. Its picture
// and sound are the plain ones, since its point is length.
func Long(name string, seconds int) Recipe {
	const rate = 12
	return single(name, name+".mkv", Truth{
		Duration:     time.Duration(seconds) * time.Second,
		VideoCodec:   "h264",
		Segments:     []Segment{{FPS: rate, Frames: seconds * rate}},
		Width:        320,
		Height:       240,
		SampleAspect: "1:1",
		FieldOrder:   "progressive",
		PixFmt:       "yuv420p",
		Audio:        []AudioTrack{{Codec: "aac", Channels: 1, SampleRate: sampleRate}},
	},
		lavfi(picture(320, 240, rate, seconds)),
		lavfi(fmt.Sprintf("sine=frequency=440:sample_rate=%d:duration=%d", sampleRate, seconds)),
		[]string{"-c:v", "libx264", "-preset", "ultrafast", "-crf", "30", "-pix_fmt", "yuv420p"},
		[]string{"-c:a", "aac", "-b:a", "48k"})
}

// truncatedTail is an MP4 cut before its index, so nothing can read it.
func truncatedTail() Recipe {
	r := single("truncated-tail", "truncated-tail.mp4", Truth{Damage: DamageProbe},
		lavfi(picture(1280, 720, clipRate, 4)), x264("yuv420p"))
	r.Finish = func(out string) error {
		return cutTo(out, truncatedCut)
	}
	return r
}

// truncatedMiddle is an MP4 with its index first, cut halfway through its data, so it probes and
// then fails during decode.
func truncatedMiddle() Recipe {
	r := single("truncated-middle", "truncated-middle.mp4", Truth{Damage: DamageDecode, VideoCodec: "h264"},
		lavfi(picture(640, 360, clipRate, 4)), x264("yuv420p"), []string{"-movflags", "+faststart"})
	r.Finish = func(out string) error {
		info, err := os.Stat(out)
		if err != nil {
			return fmt.Errorf("stat: %w", err)
		}
		return cutTo(out, info.Size()/2)
	}
	return r
}

// cutTo shortens the file at path to size bytes, and fails when it is already no longer than
// that, since the cut would then damage nothing.
func cutTo(path string, size int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() <= size {
		return fmt.Errorf("file holds %d bytes, too short to cut at %d", info.Size(), size)
	}
	if err := os.Truncate(path, size); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	return nil
}

// zero is an empty file with a media name.
func zero() Recipe {
	return Recipe{
		Name: "zero",
		File: "zero.mp4",
		Finish: func(out string) error {
			return os.WriteFile(out, nil, 0o600)
		},
		Truth: Truth{Damage: DamageProbe},
	}
}

// notMedia is a text file with a media name.
func notMedia() Recipe {
	return Recipe{
		Name: "notmedia",
		File: "notmedia.mp4",
		Finish: func(out string) error {
			return os.WriteFile(out, []byte("This file holds words, not media.\n"), 0o600)
		},
		Truth: Truth{Damage: DamageProbe},
	}
}
