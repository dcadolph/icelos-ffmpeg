// Package verify proves a converted file against its source. Every proof measures the real files
// through ffmpeg and ffprobe and lands on a Receipt, so the tests and the receipt a user reads are
// the same code, and a proof that could not run is listed as such rather than claimed.
package verify

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dcadolph/icelos-ffmpeg/probe"
)

const (
	// minDurationSlack is the least a stream's length may differ from its source's.
	minDurationSlack = 50 * time.Millisecond
	// audioDurationSlack is how far decoded audio may run from its source's length. Encoders add a
	// delay and padding that a file without an edit list keeps: AudioToolbox AAC plays 75 ms longer.
	audioDurationSlack = 100 * time.Millisecond
	// timingSlack is how far a frame may sit from the source frame's time.
	timingSlack = 5 * time.Millisecond
	// aspectSlack is the share a shown picture shape may differ from the source's.
	aspectSlack = 0.005
	// rateSlack is how many frames a set frame rate may land from its expected count.
	rateSlack = 1
	// maxCombShare is the share of frames idet may read as interlaced after a clean encode.
	maxCombShare = 0.05
	// audioEdgeFrames is how many audio frames a copy may shift by and differ by at each end, since
	// an edit list can drop a priming frame that a container without edit lists keeps.
	audioEdgeFrames = 2
)

// Expect states what the plan says the output should be. Callers fill it from their plan, so this
// package stays below the engine and its tests can use it. A receipt carries it, so the plan can be
// checked again later. Durations serialize as integer nanoseconds.
type Expect struct {
	// Streams lists the output's video, audio, and subtitle streams as "kind:codec", in order within
	// each kind, leaving out cover art. Nil means the plan states none and the check does not run.
	Streams []string `json:"streams,omitempty"`
	// VideoCopied reports that the first video stream was copied rather than encoded.
	VideoCopied bool `json:"videoCopied,omitempty"`
	// AudioCopied reports that the audio streams were copied rather than encoded.
	AudioCopied bool `json:"audioCopied,omitempty"`
	// LosslessVideo reports that the video encode must decode to the source's exact frames.
	LosslessVideo bool `json:"losslessVideo,omitempty"`
	// LosslessAudio reports that the audio encode must decode to the source's exact samples.
	LosslessAudio bool `json:"losslessAudio,omitempty"`
	// Rate is the frame rate the plan sets in frames per second, zero when it keeps the source's.
	Rate float64 `json:"rate,omitempty"`
	// PixFmt is the pixel format the output decodes to, empty when the plan states none.
	PixFmt string `json:"pixFmt,omitempty"`
	// Range is the sample range the codec always writes, empty when it keeps the source's.
	Range string `json:"range,omitempty"`
	// Scaled reports that the plan changes the frame size, so size and pixel shape may differ.
	Scaled bool `json:"scaled,omitempty"`
	// CombProof reports that the source reads true in idet, so combing can be measured. Only
	// generated gradients do: idet misreads clean testsrc2 and real footage as interlaced.
	CombProof bool `json:"combProof,omitempty"`
	// MinSSIM is the lowest picture score an encode may reach, zero when the plan states no floor.
	// A plan states RealPictureFloor, and a test that knows its footage holds it to more.
	MinSSIM float64 `json:"minSSIM,omitempty"`
	// MinSDR is the lowest sound score in decibels an encode may reach. Zero, which a plan always
	// states, compares each track's level instead, since the score holds only on generated sound.
	MinSDR float64 `json:"minSDR,omitempty"`
	// AudioTracks holds, for each output audio track, its position among the source's audio
	// tracks. Nil pairs the tracks in order, which needs equal counts.
	AudioTracks []int `json:"audioTracks,omitempty"`
	// Channels is how many channels each audio track must hold, zero when the plan states none.
	Channels int `json:"channels,omitempty"`
	// SampleRate is the sample rate each audio track must hold, zero when the plan states none.
	SampleRate int `json:"sampleRate,omitempty"`
	// Tones holds each output audio track's tone in hertz per channel, nil when not pure tones.
	Tones [][]float64 `json:"tones,omitempty"`
	// Markers lists when a generated source's flash and burst begin, nil for other sources.
	Markers []time.Duration `json:"markersNs,omitempty"`
	// Caption says where a burned caption must show, nil when the plan burns none.
	Caption *Caption `json:"caption,omitempty"`
	// NoTags lists the file tags, title or artist, that the output's container cannot hold.
	NoTags []string `json:"noTags,omitempty"`
	// NoTrackLanguage reports that the output's container cannot hold a track's language.
	NoTrackLanguage bool `json:"noTrackLanguage,omitempty"`
	// DropChapters reports that the plan leaves the source's chapters out, or the output's container
	// cannot hold them.
	DropChapters bool `json:"dropChapters,omitempty"`
	// DropMetadata reports that the plan leaves the source's tags out.
	DropMetadata bool `json:"dropMetadata,omitempty"`
	// Waive maps a check name to why it does not apply, which turns that check into a Note.
	Waive map[string]string `json:"waive,omitempty"`
}

// Input is one output to prove.
type Input struct {
	// Source is the file the output was made from.
	Source string
	// Output is the file to prove.
	Output string
	// Expect states what the plan says the output should be.
	Expect Expect
	// Tools names the binaries to run.
	Tools Tools
}

// Verify runs every proof on in.Output and returns the receipt. A failed proof is a failed check
// in the receipt. The error is set, wrapping ErrVerify, only when the proofs could not run: a
// missing tool, an unreadable source, or a canceled context.
func Verify(ctx context.Context, in Input) (Receipt, error) {
	receipt := Receipt{Schema: SchemaVersion, Source: in.Source, Output: in.Output, Expect: in.Expect}
	tool, err := in.Tools.version(ctx)
	if err != nil {
		return receipt, fmt.Errorf("%w: %w", ErrVerify, err)
	}
	receipt.Tool = tool
	src, err := in.Tools.probe(ctx, in.Source)
	if err != nil {
		return receipt, fmt.Errorf("%w: source: %w", ErrVerify, err)
	}
	r := &proof{ctx: ctx, in: in, src: src, receipt: receipt}
	if err := r.run(); err != nil {
		return r.receipt, fmt.Errorf("%w: %w", ErrVerify, err)
	}
	return r.receipt, nil
}

// proof holds one Verify run's inputs and what it has measured so far.
type proof struct {
	// ctx ends every tool the run starts.
	ctx context.Context
	// in is the output to prove.
	in Input
	// src is the source's metadata.
	src probe.Info
	// out is the output's metadata.
	out probe.Info
	// srcVideo is the source's first motion video stream.
	srcVideo probe.Stream
	// outVideo is the output's first motion video stream.
	outVideo probe.Stream
	// hasVideo reports that both files have a motion video stream.
	hasVideo bool
	// srcScan is the decode pass over srcVideo.
	srcScan frameScan
	// outScan is the decode pass over outVideo.
	outScan frameScan
	// receipt collects the results.
	receipt Receipt
}

// record adds c to the receipt, or a note when the caller waived it.
func (p *proof) record(c Check) {
	if reason, ok := p.in.Expect.Waive[c.Name]; ok {
		p.note(c.Name, reason)
		return
	}
	p.receipt.Checks = append(p.receipt.Checks, c)
}

// note adds a proof that did not run to the receipt.
func (p *proof) note(name, reason string) {
	p.receipt.Unchecked = append(p.receipt.Unchecked, Note{Name: name, Reason: reason})
}

// run measures the files and runs each check. An unparsable output is the one failed check.
func (p *proof) run() error {
	if !p.container() {
		return p.ctx.Err()
	}
	if err := p.scan(); err != nil {
		return err
	}
	if err := p.decode(); err != nil {
		return err
	}
	p.streams()
	if err := p.duration(); err != nil {
		return err
	}
	if p.hasVideo {
		p.frames()
		p.timing()
		p.display()
		p.aspect()
		p.rotation()
		p.color()
		p.bitDepth()
		p.fieldOrder()
		p.combing()
	}
	p.channels()
	p.chapters()
	p.metadata()
	return errors.Join(
		p.copiedVideo(), p.copiedAudio(), p.losslessVideo(), p.losslessAudio(),
		p.picture(), p.burned(), p.sound(), p.channelTones(), p.sync(),
	)
}

// container probes the output and reports whether the other checks can run on it.
func (p *proof) container() bool {
	c := Check{
		Name: CheckContainer, Threshold: "ffprobe parses the file and finds streams", Method: "ffprobe",
	}
	out, err := p.in.Tools.probe(p.ctx, p.in.Output)
	switch {
	case err != nil:
		c.Measured = err.Error()
	case len(out.Streams) == 0:
		c.Measured = "no streams"
	default:
		c.Pass = true
		c.Measured = fmt.Sprintf("%s with %d streams", out.Format, len(out.Streams))
	}
	p.record(c)
	p.out = out
	return c.Pass
}

// scan decodes the first motion video stream of each file. A source that logs errors is still
// counted as far as it decodes, and the receipt says so, since a damaged source is not the
// output's fault.
func (p *proof) scan() error {
	var srcOK, outOK bool
	p.srcVideo, srcOK = p.src.FirstVideo()
	p.outVideo, outOK = p.out.FirstVideo()
	p.hasVideo = srcOK && outOK
	var err error
	if outOK {
		comb := p.in.Expect.CombProof
		p.outScan, err = p.in.Tools.scanVideo(p.ctx, p.in.Output, p.outVideo.Index, comb, true)
		if err != nil {
			return fmt.Errorf("scan output: %w", err)
		}
	}
	if p.hasVideo {
		p.srcScan, err = p.in.Tools.scanVideo(p.ctx, p.in.Source, p.srcVideo.Index, false, false)
		if err != nil {
			return fmt.Errorf("scan source: %w", err)
		}
		if len(p.srcScan.Errors) > 0 {
			p.note(NoteSource, "The source logged decode errors, so frames compare to what it decoded.")
		}
	}
	return nil
}

// decode proves every video and audio stream decodes without an error.
func (p *proof) decode() error {
	skip := -1
	if _, ok := p.out.FirstVideo(); ok {
		skip = p.outVideo.Index
	}
	lines, err := p.in.Tools.decodeStreams(p.ctx, p.in.Output, p.out, skip)
	if err != nil {
		return fmt.Errorf("decode output: %w", err)
	}
	lines = append(slices.Clone(p.outScan.Errors), lines...)
	c := Check{
		Name: CheckDecode, Pass: len(lines) == 0, Measured: "no errors", Threshold: "no errors",
		Method: "ffmpeg full decode with -xerror",
	}
	if !c.Pass {
		c.Measured = fmt.Sprintf("%d errors, first: %s", len(lines), lines[0])
	}
	p.record(c)
	return nil
}

// byKind returns "kind:codec" entries with each kind together, keeping their order within a kind.
// Some muxers write streams as they first arrive, so the order between kinds says nothing.
func byKind(entries []string) []string {
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(a, b string) int {
		kindA, _, _ := strings.Cut(a, ":")
		kindB, _, _ := strings.Cut(b, ":")
		return strings.Compare(kindA, kindB)
	})
	return sorted
}

// streams proves the output holds the planned streams, in order within each kind.
func (p *proof) streams() {
	if p.in.Expect.Streams == nil {
		p.note(CheckStreams, "The plan states no stream list.")
		return
	}
	var got []string
	for _, s := range p.out.Streams {
		if (s.Kind == "video" && !s.IsCover()) || s.Kind == "audio" || s.Kind == "subtitle" {
			got = append(got, s.Kind+":"+s.Codec)
		}
	}
	p.record(Check{
		Name: CheckStreams, Pass: slices.Equal(byKind(got), byKind(p.in.Expect.Streams)),
		Measured: strings.Join(got, ", "), Threshold: strings.Join(p.in.Expect.Streams, ", "),
		Method: "ffprobe stream list",
	})
}

// duration proves the output's first video stream, else its first audio stream, plays as long as
// the same kind of stream in the source, both from their decoded frames. A container's duration or
// a stream's tag can state more than the frames hold, and many audio files state only an estimate
// from their bitrate, so those are used only when nothing decoded.
func (p *proof) duration() error {
	var got, want time.Duration
	kind := "video"
	method := "decoded video frames from ffmpeg"
	switch {
	case p.hasVideo:
		got, want = playTime(p.outVideo, p.outScan), playTime(p.srcVideo, p.srcScan)
	default:
		kind = "audio"
		method = "decoded audio frames from ffmpeg"
		outAudio, outOK := p.out.FirstAudio()
		srcAudio, srcOK := p.src.FirstAudio()
		if !outOK || !srcOK {
			p.note(CheckDuration, "The source and output share no stream kind to compare.")
			return nil
		}
		var err error
		if want, err = p.in.Tools.audioSpan(p.ctx, p.in.Source, srcAudio.Index); err != nil {
			return p.sourceUnread(CheckDuration, err)
		}
		if got, err = p.in.Tools.audioSpan(p.ctx, p.in.Output, outAudio.Index); err != nil {
			return p.outputUnread(CheckDuration, "the source's length", method, err)
		}
	}
	if got <= 0 {
		got = p.out.Duration
	}
	if want <= 0 {
		want = p.src.Duration
	}
	if got <= 0 || want <= 0 {
		p.note(CheckDuration, "The files record no duration to compare.")
		return nil
	}
	slack := audioDurationSlack
	if kind == "video" {
		slack = minDurationSlack
		if fps := p.srcVideo.FrameRate(); fps > 0 {
			slack = max(slack, time.Duration(float64(time.Second)/fps))
		}
	}
	p.record(Check{
		Name: CheckDuration, Pass: (got - want).Abs() <= slack,
		Measured: fmt.Sprintf("%s stream plays %s", kind, got.Round(time.Millisecond)),
		Threshold: fmt.Sprintf("within %s of the source's %s",
			slack.Round(time.Millisecond), want.Round(time.Millisecond)),
		Method: method,
	})
	return nil
}

// playTime returns how long a stream plays from its decoded frames, else from its metadata, which
// can state more than the frames hold, as a Matroska track's duration tag can.
func playTime(s probe.Stream, scan frameScan) time.Duration {
	if d := scan.Span(); d > 0 {
		return d
	}
	return s.PlayTime()
}

// frames proves the decoded frame count. A kept rate must keep every frame, and a set rate must
// land within one frame of the source's length at that rate.
func (p *proof) frames() {
	got, source := len(p.outScan.Times), len(p.srcScan.Times)
	c := Check{
		Name: CheckFrames, Pass: got == source, Measured: fmt.Sprintf("%d frames", got),
		Threshold: fmt.Sprintf("exactly the source's %d", source),
		Method:    "decoded frames counted with ffmpeg",
	}
	if rate := p.in.Expect.Rate; rate > 0 {
		span := playTime(p.srcVideo, p.srcScan)
		want := int(math.Round(span.Seconds() * rate))
		c.Pass = got >= want-rateSlack && got <= want+rateSlack
		c.Threshold = fmt.Sprintf("%d within %d, %s at %g fps",
			want, rateSlack, span.Round(time.Millisecond), rate)
	}
	p.record(c)
}

// timing proves each frame shows when the source's frame shows, measured from each stream's first
// frame. It applies only when the plan keeps the source's rate, and needs equal frame counts since
// the frames cannot be paired otherwise.
func (p *proof) timing() {
	if p.in.Expect.Rate > 0 {
		return
	}
	if len(p.outScan.Times) != len(p.srcScan.Times) {
		p.note(CheckTiming, "The frame counts differ, so frames cannot be paired.")
		return
	}
	var worst time.Duration
	for i, t := range p.outScan.Times {
		worst = max(worst, (t - p.srcScan.Times[i]).Abs())
	}
	p.record(Check{
		Name: CheckTiming, Pass: worst <= timingSlack,
		Measured: fmt.Sprintf("worst frame %.1f ms from the source",
			float64(worst)/float64(time.Millisecond)),
		Threshold: fmt.Sprintf("at most %s", timingSlack),
		Method:    "decoded frame times from ffmpeg, from each stream's first frame",
	})
}

// display proves the shown size and pixel shape equal the source's, unless the plan scales.
func (p *proof) display() {
	if p.in.Expect.Scaled {
		return
	}
	gw, gh := p.outVideo.DisplaySize()
	sw, sh := p.srcVideo.DisplaySize()
	p.record(Check{
		Name: CheckDisplay, Pass: gw == sw && gh == sh, Measured: fmt.Sprintf("%dx%d", gw, gh),
		Threshold: fmt.Sprintf("the source's %dx%d", sw, sh), Method: "ffprobe size and rotation",
	})
	got, source := shownSampleAspect(p.outVideo), shownSampleAspect(p.srcVideo)
	p.record(Check{
		Name: CheckSampleAspect, Pass: got.equal(source), Measured: got.String(),
		Threshold: "the source's " + source.String(), Method: "ffprobe sample aspect and rotation",
	})
}

// aspect proves the shape of the picture as shown is the source's, scaled or not.
func (p *proof) aspect() {
	got, want := shownAspect(p.outVideo), shownAspect(p.srcVideo)
	if want == 0 {
		p.note(CheckAspect, "The source reports no frame size.")
		return
	}
	p.record(Check{
		Name: CheckAspect, Pass: math.Abs(got-want)/want <= aspectSlack,
		Measured:  fmt.Sprintf("%.4f", got),
		Threshold: fmt.Sprintf("within %.1f%% of the source's %.4f", aspectSlack*100, want),
		Method:    "width times sample aspect over height, inverted on a quarter turn",
	})
}

// rotation proves an encode turned the pixels and left no rotation, and a copy kept the source's.
func (p *proof) rotation() {
	want := 0
	if p.in.Expect.VideoCopied {
		want = p.srcVideo.Rotation()
	}
	got := p.outVideo.Rotation()
	p.record(Check{
		Name: CheckRotation, Pass: got == want, Measured: fmt.Sprintf("%d degrees", got),
		Threshold: fmt.Sprintf("%d degrees", want), Method: "ffprobe display matrix",
	})
}

// knownColor reports whether a color tag states something.
func knownColor(tag string) bool {
	switch tag {
	case "", "unknown", "unspecified", "reserved":
		return false
	}
	return true
}

// statedRange returns the sample range a stream means. Every player reads an unstated range as tv,
// and encoders routinely leave it out, so only an unstated full range counts as a change.
func statedRange(tag string) string {
	if !knownColor(tag) {
		return "tv"
	}
	return tag
}

// color proves every color tag the source states came through unchanged, apart from a sample range
// the codec always writes, which the encode converts the picture to.
func (p *proof) color() {
	sourceRange := p.srcVideo.ColorRange
	if p.in.Expect.Range != "" {
		sourceRange = p.in.Expect.Range
	}
	tags := []struct{ name, src, out string }{
		{"range", sourceRange, statedRange(p.outVideo.ColorRange)},
		{"matrix", p.srcVideo.ColorSpace, p.outVideo.ColorSpace},
		{"transfer", p.srcVideo.ColorTransfer, p.outVideo.ColorTransfer},
		{"primaries", p.srcVideo.ColorPrimaries, p.outVideo.ColorPrimaries},
	}
	var measured, threshold []string
	pass := true
	for _, tag := range tags {
		if !knownColor(tag.src) {
			continue
		}
		measured = append(measured, tag.name+" "+tag.out)
		threshold = append(threshold, tag.name+" "+tag.src)
		pass = pass && tag.out == tag.src
	}
	if len(threshold) == 0 {
		p.note(CheckColor, "The source states no color tags.")
		return
	}
	p.record(Check{
		Name: CheckColor, Pass: pass, Measured: strings.Join(measured, ", "),
		Threshold: strings.Join(threshold, ", "), Method: "ffprobe color tags",
	})
}

// bitDepth proves the pixel format is the planned one, or the source's on a copy.
func (p *proof) bitDepth() {
	want := p.in.Expect.PixFmt
	if want == "" && p.in.Expect.VideoCopied {
		want = p.srcVideo.PixFmt
	}
	if want == "" {
		p.note(CheckBitDepth, "The plan states no pixel format.")
		return
	}
	p.record(Check{
		Name: CheckBitDepth, Pass: p.outVideo.PixFmt == want, Measured: p.outVideo.PixFmt,
		Threshold: want, Method: "ffprobe pixel format",
	})
}

// interlacedTag reports whether a field order tag says the stream is interlaced.
func interlacedTag(tag string) bool {
	switch tag {
	case "tt", "bb", "tb", "bt":
		return true
	}
	return false
}

// firstField reads a field order tag as which field comes first, since muxers report the same
// content as tt or tb, and as bb or bt.
func firstField(tag string) string {
	switch tag {
	case "tt", "tb":
		return "top"
	case "bb", "bt":
		return "bottom"
	}
	return tag
}

// fieldOrder proves the output is not tagged interlaced after an encode, and that a copy kept the
// source's tag.
func (p *proof) fieldOrder() {
	got, source := p.outVideo.FieldOrder, p.srcVideo.FieldOrder
	if p.in.Expect.VideoCopied {
		if source == "" || source == "unknown" {
			p.note(CheckFieldOrder, "The source states no field order.")
			return
		}
		p.record(Check{
			Name: CheckFieldOrder, Pass: firstField(got) == firstField(source), Measured: got,
			Threshold: source, Method: "ffprobe field order, tt and tb both meaning top first",
		})
		return
	}
	p.record(Check{
		Name: CheckFieldOrder, Pass: !interlacedTag(got), Measured: got, Threshold: "not interlaced",
		Method: "ffprobe field order",
	})
}

// combing proves the decoded picture holds no combing when the caller knows idet reads the source
// true. On any other interlaced source it says the proof did not run, since idet misreads clean
// footage and a pass or fail from it would mean nothing.
func (p *proof) combing() {
	if !p.in.Expect.CombProof {
		if interlacedTag(p.srcVideo.FieldOrder) {
			p.note(CheckCombing, "idet does not read this source reliably.")
		}
		return
	}
	counts := p.outScan.Comb
	p.record(Check{
		Name: CheckCombing, Pass: counts.Progressive > 0 && counts.Share() <= maxCombShare,
		Measured: fmt.Sprintf("TFF %d, BFF %d, progressive %d, undetermined %d",
			counts.TFF, counts.BFF, counts.Progressive, counts.Undetermined),
		Threshold: fmt.Sprintf("at most %.0f%% interlaced", maxCombShare*100),
		Method:    "ffmpeg idet multi frame detection",
	})
}

// copiedVideo proves a copied video stream holds the source's packets, or decodes to the source's
// frames when the copy reshaped them.
func (p *proof) copiedVideo() error {
	if !p.in.Expect.VideoCopied {
		return nil
	}
	if !p.hasVideo {
		p.record(Check{
			Name: CheckCopiedVideo, Measured: "no video stream to compare",
			Threshold: "the source's packets",
			Method:    "ffprobe stream list",
		})
		return nil
	}
	return p.copied(CheckCopiedVideo, p.srcVideo.Index, p.outVideo.Index, 0)
}

// copiedAudio proves copied audio tracks by position. A selection that drops tracks cannot be
// paired this way, and the receipt says so.
func (p *proof) copiedAudio() error {
	if !p.in.Expect.AudioCopied {
		return nil
	}
	pairs, ok := p.audioPairs(CheckCopiedAudio)
	if !ok {
		return nil
	}
	var errs []error
	for _, pair := range pairs {
		errs = append(errs, p.copied(CheckCopiedAudio, pair[0], pair[1], audioEdgeFrames))
	}
	return errors.Join(errs...)
}

// audioPairs returns source and output audio stream indexes, by Expect.AudioTracks when set and
// else by position, noting when the tracks cannot be paired, and failing the check when the output
// has no audio to compare.
func (p *proof) audioPairs(name string) ([][2]int, bool) {
	src, out := p.src.Kind("audio"), p.out.Kind("audio")
	if len(out) == 0 {
		p.record(Check{
			Name: name, Measured: "no audio stream to compare", Threshold: "the source's audio",
			Method: "ffprobe stream list",
		})
		return nil, false
	}
	if tracks := p.in.Expect.AudioTracks; tracks != nil {
		if len(tracks) != len(out) {
			p.note(name, fmt.Sprintf("The plan pairs %d audio tracks and the output holds %d.",
				len(tracks), len(out)))
			return nil, false
		}
		pairs := make([][2]int, 0, len(out))
		for i, track := range tracks {
			if track < 0 || track >= len(src) {
				p.note(name, fmt.Sprintf("The source has no audio track %d.", track))
				return nil, false
			}
			pairs = append(pairs, [2]int{src[track].Index, out[i].Index})
		}
		return pairs, true
	}
	if len(src) != len(out) {
		p.note(name, fmt.Sprintf("The output holds %d of the source's %d audio tracks.",
			len(out), len(src)))
		return nil, false
	}
	pairs := make([][2]int, 0, len(out))
	for i := range out {
		pairs = append(pairs, [2]int{src[i].Index, out[i].Index})
	}
	return pairs, true
}

// copied compares one stream's packets to the source's, then falls back to its decoded frames, so
// a copy from Annex B to length-prefixed packets still passes when the frames are the same. The
// edge is how many frames the fallback may shift and leave out at each end.
func (p *proof) copied(name string, srcIndex, outIndex, edge int) error {
	want, err := p.in.Tools.packetHash(p.ctx, p.in.Source, srcIndex)
	if err != nil {
		return p.sourceUnread(name, err)
	}
	got, err := p.in.Tools.packetHash(p.ctx, p.in.Output, outIndex)
	if err != nil {
		return p.outputUnread(name, "the source's packets", "ffmpeg streamhash", err)
	}
	if got == want {
		p.record(Check{
			Name: name, Pass: true, Measured: "packets identical", Threshold: "the source's packets",
			Method: "SHA-256 of the stream's packets",
		})
		return nil
	}
	return p.sameFrames(name, srcIndex, outIndex, edge, "packets differ, ")
}

// sameFrames records whether two streams decode to the same frames, allowing edge frames of
// shift and of difference at each end.
func (p *proof) sameFrames(name string, srcIndex, outIndex, edge int, prefix string) error {
	threshold := "the source's decoded frames"
	if edge > 0 {
		threshold += fmt.Sprintf(", shifted at most %d and leaving out %d at each end", edge, edge)
	}
	want, err := p.in.Tools.frameSums(p.ctx, p.in.Source, srcIndex)
	if err != nil {
		return p.sourceUnread(name, err)
	}
	got, err := p.in.Tools.frameSums(p.ctx, p.in.Output, outIndex)
	if err != nil {
		return p.outputUnread(name, threshold, "ffmpeg framemd5", err)
	}
	pass := alignedFrames(want, got, edge)
	measured := fmt.Sprintf("%s%d decoded frames differ from the source's %d",
		prefix, len(got), len(want))
	if pass {
		measured = fmt.Sprintf("%s%d decoded frames match the source's %d", prefix, len(got), len(want))
	}
	p.record(Check{
		Name: name, Pass: pass, Measured: measured, Threshold: threshold,
		Method: "framemd5 of each decoded frame",
	})
	return nil
}

// alignedFrames reports whether got holds want's frames. Both must hold the same frames in the
// same order, apart from a shift of at most edge frames and edge frames at each end of want. A
// clip too short to leave a middle after both ends must match exactly.
func alignedFrames(want, got []string, edge int) bool {
	if len(want) < 2*edge+1 || len(got) < 2*edge+1 {
		return slices.Equal(want, got)
	}
	diff := len(got) - len(want)
	if diff < -edge || diff > edge {
		return false
	}
	middle := want[edge : len(want)-edge]
	for start := 0; start <= 2*edge; start++ {
		if end := start + len(middle); end <= len(got) && slices.Equal(middle, got[start:end]) {
			return true
		}
	}
	return false
}

// losslessVideo proves an encoded video stream decodes to the source's exact frames.
func (p *proof) losslessVideo() error {
	if !p.in.Expect.LosslessVideo || !p.hasVideo {
		return nil
	}
	return p.sameFrames(CheckLosslessVideo, p.srcVideo.Index, p.outVideo.Index, 0, "")
}

// losslessAudio proves encoded audio tracks decode to the source's exact samples, by position.
func (p *proof) losslessAudio() error {
	if !p.in.Expect.LosslessAudio {
		return nil
	}
	pairs, ok := p.audioPairs(CheckLosslessAudio)
	if !ok {
		return nil
	}
	for _, pair := range pairs {
		want, err := p.in.Tools.pcmHash(p.ctx, p.in.Source, pair[0])
		if err != nil {
			return p.sourceUnread(CheckLosslessAudio, err)
		}
		got, err := p.in.Tools.pcmHash(p.ctx, p.in.Output, pair[1])
		if err != nil {
			unread := p.outputUnread(CheckLosslessAudio, "the source's samples", "ffmpeg hash", err)
			if unread != nil {
				return unread
			}
			continue
		}
		measured := "samples differ"
		same := got == want
		if same {
			measured = "samples identical"
		} else if limit := p.framePadding(pair[1]); limit > 0 {
			padded, err := p.in.Tools.paddedPCM(p.ctx, p.in.Source, pair[0], p.in.Output, pair[1], limit)
			if err != nil {
				return p.sourceUnread(CheckLosslessAudio, err)
			}
			if padded > 0 {
				same = true
				measured = fmt.Sprintf("samples identical, then %d silent bytes that fill the last frame", padded)
			}
		}
		p.record(Check{
			Name: CheckLosslessAudio, Pass: same, Measured: measured,
			Threshold: "the source's samples", Method: "SHA-256 of samples decoded to 32 bits",
		})
	}
	return nil
}

// framePadding returns how many bytes of 32-bit samples one output video frame spans for the audio
// stream at index, the most a muxer that fills out the last frame adds, or zero with no video.
func (p *proof) framePadding(index int) int64 {
	video, ok := p.out.FirstVideo()
	if !ok || video.FrameRate() <= 0 {
		return 0
	}
	for _, s := range p.out.Kind("audio") {
		if s.Index == index {
			samples := math.Ceil(float64(s.SampleRate) / video.FrameRate())
			return int64(samples) * int64(s.Channels) * pcmSampleBytes
		}
	}
	return 0
}

// sourceUnread notes a proof the source could not support, and returns the context's error when
// that is the real cause.
func (p *proof) sourceUnread(name string, err error) error {
	if ctxErr := p.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	p.note(name, "The source could not be read for this proof: "+err.Error())
	return nil
}

// outputUnread fails a proof the output could not support, and returns the context's error when
// that is the real cause.
func (p *proof) outputUnread(name, threshold, method string, err error) error {
	if ctxErr := p.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	p.record(Check{Name: name, Measured: err.Error(), Threshold: threshold, Method: method})
	return nil
}

// ratio is a reduced fraction.
type ratio struct {
	// N is the numerator.
	N int
	// D is the denominator.
	D int
}

// equal reports whether two ratios state the same value.
func (r ratio) equal(o ratio) bool {
	return r.N*o.D == o.N*r.D
}

// String returns the ratio in ffprobe's form.
func (r ratio) String() string {
	return fmt.Sprintf("%d:%d", r.N, r.D)
}

// sampleAspect parses ffprobe's sample aspect, reading missing or zero as square pixels.
func sampleAspect(s string) ratio {
	num, den, ok := strings.Cut(s, ":")
	n, nerr := strconv.Atoi(num)
	d, derr := strconv.Atoi(den)
	if !ok || nerr != nil || derr != nil || n <= 0 || d <= 0 {
		return ratio{N: 1, D: 1}
	}
	return ratio{N: n, D: d}
}

// shownSampleAspect returns a stream's pixel shape as the viewer sees it, turned with the picture.
func shownSampleAspect(s probe.Stream) ratio {
	r := sampleAspect(s.SampleAspect)
	if rot := s.Rotation(); rot == 90 || rot == 270 {
		return ratio{N: r.D, D: r.N}
	}
	return r
}

// shownAspect returns a stream's picture shape as the viewer sees it, zero without a frame size.
func shownAspect(s probe.Stream) float64 {
	w, h := s.DisplaySize()
	if w <= 0 || h <= 0 {
		return 0
	}
	r := shownSampleAspect(s)
	return float64(w) * float64(r.N) / (float64(h) * float64(r.D))
}
