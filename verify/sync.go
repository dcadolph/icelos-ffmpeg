package verify

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// syncSlack is how far past one frame a flash or a burst may sit from its schedule.
	syncSlack = 5 * time.Millisecond
	// soundLateLimit is how far sound may trail the picture, EBU R37's limit. A container that
	// cannot state an encoder's start delay, such as AVI, holds AAC's 44 ms in the file.
	soundLateLimit = 60 * time.Millisecond
	// soundEarlyLimit is how far sound may lead the picture, EBU R37's limit.
	soundEarlyLimit = 40 * time.Millisecond
	// syncReach is how far from its scheduled time an onset may be and still pair with it.
	syncReach = 500 * time.Millisecond
	// flashFilter finds a white flash as a black run in the negated picture.
	flashFilter = "negate,blackdetect=d=0:pic_th=0.9:pix_th=0.15"
	// chapterSlack is how far a kept chapter's start may move.
	chapterSlack = 10 * time.Millisecond
	// burstFilter finds a burst as the end of a run of silence.
	burstFilter = "silencedetect=n=-30dB:d=0.05"
)

var (
	// flashStart matches where blackdetect saw a flash begin.
	flashStart = regexp.MustCompile(`black_start:([0-9.]+)`)
	// burstStart matches where silencedetect saw a burst begin.
	burstStart = regexp.MustCompile(`silence_end: ([0-9.]+)`)
)

// onsets returns the times of every match of re in the log of a decode of one stream through
// filter, read from the file's own timestamps, so a video and an audio pass compare whatever each
// stream starts at.
func (t Tools) onsets(ctx context.Context, path string, index int, flag, filter string,
	re *regexp.Regexp) ([]time.Duration, error) {
	log, err := t.logged(ctx, "-hide_banner", "-nostdin", "-nostats", "-copyts", "-i", path,
		"-map", fmt.Sprintf("0:%d", index), flag, filter, "-f", "null", "-")
	if err != nil {
		return nil, err
	}
	var times []time.Duration
	for _, m := range re.FindAllSubmatch(log, -1) {
		secs, err := strconv.ParseFloat(string(m[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("parse: %w", err)
		}
		times = append(times, time.Duration(secs*float64(time.Second)))
	}
	return times, nil
}

// nearest returns the time in times closest to want, and false when none is within syncReach.
func nearest(times []time.Duration, want time.Duration) (time.Duration, bool) {
	var best time.Duration
	found := false
	for _, t := range times {
		if d := (t - want).Abs(); d <= syncReach && (!found || d < (best-want).Abs()) {
			best, found = t, true
		}
	}
	return best, found
}

// sync proves each generated flash and burst land together and on their schedule. A flash may sit
// one output frame plus syncSlack from its schedule, measured from the first flash, since a file
// may start its whole timeline late without moving anything against anything else. Sound may sit
// as far from the picture as EBU R37 allows, or that one frame when it is longer. The marker at the
// very start has no burst to read, since nothing silent comes before it. Without a picture in the
// output the proof holds the sound to the schedule alone, and without sound the picture.
func (p *proof) sync() error {
	markers := p.in.Expect.Markers
	if markers == nil {
		return nil
	}
	audio, hasAudio := p.out.FirstAudio()
	video, hasVideo := p.out.FirstVideo()
	method := "ffmpeg blackdetect on the negated picture and silencedetect on the sound"
	var flashes, bursts []time.Duration
	var err error
	if hasVideo {
		flashes, err = p.in.Tools.onsets(p.ctx, p.in.Output, video.Index, "-vf", flashFilter, flashStart)
		if err != nil {
			return p.outputUnread(CheckSync, "markers on schedule", method, fmt.Errorf("flashes: %w", err))
		}
	}
	if hasAudio {
		bursts, err = p.in.Tools.onsets(p.ctx, p.in.Output, audio.Index, "-af", burstFilter, burstStart)
		if err != nil {
			return p.outputUnread(CheckSync, "markers on schedule", method, fmt.Errorf("bursts: %w", err))
		}
	}
	slack := syncSlack
	if fps := video.FrameRate(); hasVideo && fps > 0 {
		slack += time.Duration(float64(time.Second) / fps)
	}
	late, early := max(soundLateLimit, slack), max(soundEarlyLimit, slack)
	threshold := fmt.Sprintf("picture within %s of the schedule, sound at most %s before it and %s after",
		slack.Round(time.Millisecond), early.Round(time.Millisecond), late.Round(time.Millisecond))
	var start time.Duration
	if len(flashes) > 0 && len(markers) > 0 {
		start = flashes[0] - markers[0]
	}
	var worst time.Duration
	pass := true
	for _, marker := range markers {
		if marker == 0 {
			continue
		}
		flash, flashOK := nearest(flashes, marker+start)
		burst, burstOK := nearest(bursts, marker+start)
		if (hasVideo && !flashOK) || (hasAudio && !burstOK) {
			p.record(Check{
				Name:     CheckSync,
				Measured: fmt.Sprintf("no onset near %s", marker), Threshold: threshold, Method: method,
			})
			return nil
		}
		onSchedule := burst
		if hasVideo {
			onSchedule = flash
		}
		off := (onSchedule - start - marker).Abs()
		pass = pass && off <= slack
		worst = max(worst, off)
		if hasVideo && hasAudio {
			apart := burst - flash
			pass = pass && apart <= late && -apart <= early
			worst = max(worst, apart.Abs())
		}
	}
	p.record(Check{
		Name: CheckSync, Pass: pass,
		Measured:  fmt.Sprintf("worst offset %.1f ms", float64(worst)/float64(time.Millisecond)),
		Threshold: threshold, Method: method,
	})
	return nil
}

// chapters proves the source's chapters kept their places, or are gone when the plan drops them.
func (p *proof) chapters() {
	got, want := p.out.Chapters, p.src.Chapters
	method := "ffprobe chapter list"
	if p.in.Expect.DropChapters {
		p.record(Check{
			Name: CheckChapters, Pass: len(got) == 0, Measured: fmt.Sprintf("%d chapters", len(got)),
			Threshold: "none", Method: method,
		})
		return
	}
	if len(want) == 0 {
		return
	}
	threshold := fmt.Sprintf("%d chapters, each within %s of the source", len(want), chapterSlack)
	if len(got) != len(want) {
		p.record(Check{
			Name: CheckChapters, Measured: fmt.Sprintf("%d chapters", len(got)), Threshold: threshold,
			Method: method,
		})
		return
	}
	var worst time.Duration
	for i := range got {
		worst = max(worst, (got[i].Start - want[i].Start).Abs())
	}
	p.record(Check{
		Name: CheckChapters, Pass: worst <= chapterSlack,
		Measured:  fmt.Sprintf("%d chapters, worst start %s away", len(got), worst),
		Threshold: threshold, Method: method,
	})
}

// statedLanguage reads the tag a muxer writes for no language as no language.
func statedLanguage(tag string) string {
	if tag == "und" {
		return ""
	}
	return tag
}

// metadata proves the container title and artist and each track's language were kept, or are gone
// when the plan drops them. Tracks compare by position when the output kept every one of a kind.
func (p *proof) metadata() {
	type field struct{ name, src, out string }
	var fields []field
	for _, name := range []string{"title", "artist"} {
		if slices.Contains(p.in.Expect.NoTags, name) {
			continue
		}
		fields = append(fields, field{name, p.src.Tag(name), p.out.Tag(name)})
	}
	for _, kind := range []string{"audio", "subtitle"} {
		if p.in.Expect.NoTrackLanguage {
			break
		}
		src, out := p.src.Kind(kind), p.out.Kind(kind)
		if kind == "audio" && p.in.Expect.AudioTracks != nil && len(p.in.Expect.AudioTracks) == len(out) {
			for i, track := range p.in.Expect.AudioTracks {
				if track >= 0 && track < len(src) {
					fields = append(fields, field{
						fmt.Sprintf("%s %d language", kind, i), src[track].Tags.Language, out[i].Tags.Language,
					})
				}
			}
			continue
		}
		if len(src) != len(out) {
			continue
		}
		for i := range out {
			fields = append(fields, field{
				fmt.Sprintf("%s %d language", kind, i), src[i].Tags.Language, out[i].Tags.Language,
			})
		}
	}
	var measured, threshold []string
	pass := true
	for _, f := range fields {
		want, got := statedLanguage(f.src), statedLanguage(f.out)
		if want == "" {
			continue
		}
		if p.in.Expect.DropMetadata {
			threshold = append(threshold, f.name+" absent")
			pass = pass && got == ""
		} else {
			threshold = append(threshold, f.name+" "+want)
			pass = pass && got == want
		}
		measured = append(measured, f.name+" "+quoted(got))
	}
	if len(threshold) == 0 {
		return
	}
	p.record(Check{
		Name: CheckMetadata, Pass: pass, Measured: strings.Join(measured, ", "),
		Threshold: strings.Join(threshold, ", "), Method: "ffprobe container and stream tags",
	})
}

// quoted renders a tag value, naming an absent one.
func quoted(value string) string {
	if value == "" {
		return "absent"
	}
	return value
}
