package verify

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dcadolph/icelos-ffmpeg/probe"
)

const (
	// alignRead is how many seconds of each track the alignment search reads, so a quiet opening
	// still leaves sound to align.
	alignRead = 30
	// alignSeconds is how many seconds of sound the alignment search compares.
	alignSeconds = 4
	// alignReach is the longest offset the alignment search tries, as a divisor of the sample rate.
	// A tenth of a second covers the encoder delay of every catalog codec.
	alignReach = 10
	// alignQuiet is the sample level below which a track's opening counts as silence.
	alignQuiet = 0.001
	// alignStep is the coarse search's step in samples, whose result the fine search refines.
	alignStep = 6
	// alignTie is how much better a longer offset must correlate to beat a shorter one, since a
	// pure tone correlates the same at every one of its periods.
	alignTie = 1.001
	// minToneShare is the least share of a channel's power its generated tone must hold.
	minToneShare = 0.5
	// toneEdge is how much of each end of a track the tone proof leaves out, in seconds.
	toneEdge = 0.2
)

// Sound level limits for footage with no floor. On the corpus the mean level moves at most 0.1 dB
// when the channel count holds and 2.8 dB through a downmix. A downmix gets more room, since a mix
// that leans on the channels it drops can lose far more than the corpus shows.
const (
	// levelSlack is how many decibels a track's mean level may move at the same channel count.
	levelSlack = 6.0
	// downmixLevelSlack is how many decibels a mean level may move when the channel count changes.
	downmixLevelSlack = 12.0
	// silentLevel is the mean level in decibels at or under which a source track counts as silent.
	silentLevel = -80.0
)

// meanVolume reads volumedetect's mean level, which is -inf on digital silence.
var meanVolume = regexp.MustCompile(`mean_volume: (-?inf|-?[0-9.]+) dB`)

// errSilent reports that every channel of a source track is silent.
var errSilent = errors.New("sound score: the source's track is silent")

var (
	// sdrChannel matches one channel's score on the closing lines of asdr and asisdr.
	sdrChannel = regexp.MustCompile(`SDR ch\d+: (-?[0-9.]+|-?inf|-?nan) dB`)
)

// monoWindow returns the start of an audio stream decoded to one channel at the given rate.
func (t Tools) monoWindow(ctx context.Context, path string, index, rate int) ([]float32, error) {
	out, err := t.output(ctx, "-hide_banner", "-nostdin", "-v", "error", "-i", path,
		"-map", fmt.Sprintf("0:%d", index), "-t", strconv.Itoa(alignRead),
		"-af", fmt.Sprintf("aresample=%d,aformat=sample_fmts=flt:channel_layouts=mono", rate),
		"-f", "f32le", "-")
	if err != nil {
		return nil, fmt.Errorf("read samples: %w", err)
	}
	samples := make([]float32, len(out)/4)
	for i := range samples {
		samples[i] = math.Float32frombits(binary.LittleEndian.Uint32(out[i*4:]))
	}
	return samples, nil
}

// bestLag returns how many samples later out runs than ref, trying offsets out to reach in steps
// of step around center and keeping the shorter offset unless a longer one correlates clearly
// better. Output that runs early gives a negative result.
func bestLag(ref, out []float32, center, reach, step int) int {
	best, bestScore := center, math.Inf(-1)
	try := func(lag int) {
		score := correlation(ref, out, lag)
		switch {
		case bestScore <= 0 && score > bestScore:
			best, bestScore = lag, score
		case bestScore > 0 && score > bestScore*alignTie:
			best, bestScore = lag, score
		}
	}
	try(center)
	for offset := step; offset <= reach; offset += step {
		try(center + offset)
		try(center - offset)
	}
	return best
}

// correlation returns the normalized correlation of ref against out moved earlier by lag samples,
// zero when they do not overlap or one is silent over the overlap.
func correlation(ref, out []float32, lag int) float64 {
	var sum, refEnergy, outEnergy float64
	for i, r := range ref {
		j := i + lag
		if j < 0 {
			continue
		}
		if j >= len(out) {
			break
		}
		a, b := float64(r), float64(out[j])
		sum += a * b
		refEnergy += a * a
		outEnergy += b * b
	}
	if refEnergy == 0 || outEnergy == 0 {
		return 0
	}
	return sum / math.Sqrt(refEnergy*outEnergy)
}

// decimate returns samples averaged in runs of step.
func decimate(samples []float32, step int) []float32 {
	out := make([]float32, 0, len(samples)/step)
	for start := 0; start+step <= len(samples); start += step {
		var sum float32
		for _, s := range samples[start : start+step] {
			sum += s
		}
		out = append(out, sum/float32(step))
	}
	return out
}

// audioLag finds how many samples a track of output runs behind one of source at the given rate.
// A codec's delay moves the samples, which sample by sample scoring would read as a total loss,
// and whether the delay reached the viewer is the sync proof's question.
func (t Tools) audioLag(ctx context.Context, source, output string, srcIndex, outIndex, rate int) (int, error) {
	ref, err := t.monoWindow(ctx, source, srcIndex, rate)
	if err != nil {
		return 0, fmt.Errorf("source: %w", err)
	}
	out, err := t.monoWindow(ctx, output, outIndex, rate)
	if err != nil {
		return 0, fmt.Errorf("output: %w", err)
	}
	if len(ref) == 0 || len(out) == 0 {
		return 0, errors.New("a track holds no samples")
	}
	start := slices.IndexFunc(ref, func(s float32) bool { return math.Abs(float64(s)) > alignQuiet })
	if start < 0 {
		return 0, nil
	}
	window := ref[start:min(start+alignSeconds*rate, len(ref))]
	coarse := bestLag(decimate(window, alignStep), decimate(out, alignStep), start/alignStep,
		rate/alignReach/alignStep, 1)
	return bestLag(window, out, coarse*alignStep, alignStep, 1) - start, nil
}

// soundScore returns the lowest channel's signal to distortion ratio in decibels of a track of
// output against one of source, and how many samples the output was moved to align them. A channel
// the source leaves silent, as surround mixes often leave one, has no signal to score against. The
// source is brought to the output's rate and channel layout, so a planned downmix scores against
// the same downmix of the source. A downmix scores free of overall gain through asisdr, since the
// matrix is scaled by the sample format the encoder happens to take, and that scale is no loss.
func (t Tools) soundScore(ctx context.Context, source, output string, src, out probe.Stream) (float64, int, error) {
	rate := out.SampleRate
	if rate == 0 {
		rate = src.SampleRate
	}
	if rate == 0 {
		return 0, 0, errors.New("sound score: no sample rate")
	}
	lag, err := t.audioLag(ctx, source, output, src.Index, out.Index, rate)
	if err != nil {
		return 0, 0, fmt.Errorf("sound score: %w", err)
	}
	format := fmt.Sprintf("aresample=%d,aformat=sample_fmts=fltp", rate)
	if out.ChannelLayout != "" {
		format += ":channel_layouts=" + out.ChannelLayout
	}
	trim := func(samples int) string {
		return fmt.Sprintf(",atrim=start_sample=%d,asetpts=PTS-STARTPTS", max(samples, 0))
	}
	metric := "asdr"
	if src.Channels != out.Channels {
		metric = "asisdr"
	}
	graph := fmt.Sprintf("[0:%d]%s%s[d];[1:%d]%s%s[r];[r][d]%s",
		out.Index, format, trim(lag), src.Index, format, trim(-lag), metric)
	log, err := t.logged(ctx, "-hide_banner", "-nostdin", "-nostats", "-i", output, "-i", source,
		"-filter_complex", graph, "-f", "null", "-")
	if err != nil {
		return 0, 0, fmt.Errorf("sound score: %w", err)
	}
	worst, err := lowestChannel(log)
	if err != nil {
		return 0, 0, err
	}
	return worst, lag, nil
}

// lowestChannel reads the lowest channel's score from the closing lines of asdr or asisdr. A channel
// the source leaves silent reads as not a number or as minus infinity, in the form the platform's C
// library prints, and has no signal to score against.
func lowestChannel(log []byte) (float64, error) {
	all := sdrChannel.FindAllSubmatch(log, -1)
	if len(all) == 0 {
		return 0, errors.New("sound score: asdr printed no score")
	}
	worst, scored := math.Inf(1), false
	for _, m := range all {
		text := string(m[1])
		if strings.HasSuffix(text, "nan") {
			continue
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, fmt.Errorf("sound score: parse: %w", err)
		}
		if math.IsInf(value, -1) {
			continue
		}
		worst, scored = min(worst, value), true
	}
	if !scored {
		return 0, errSilent
	}
	return worst, nil
}

// sound proves encoded audio tracks score at or above the plan's floor against the source.
func (p *proof) sound() error {
	if p.in.Expect.AudioCopied || p.in.Expect.LosslessAudio {
		return nil
	}
	floor := p.in.Expect.MinSDR
	if floor <= 0 {
		if len(p.out.Kind("audio")) == 0 {
			return nil
		}
		return p.soundLevel()
	}
	pairs, ok := p.audioPairs(CheckSound)
	if !ok {
		return nil
	}
	threshold := fmt.Sprintf("SDR at least %.1f dB", floor)
	method := "ffmpeg asdr after aligning the samples by correlation"
	for _, pair := range pairs {
		src, out := streamByIndex(p.src, pair[0]), streamByIndex(p.out, pair[1])
		score, lag, err := p.in.Tools.soundScore(p.ctx, p.in.Source, p.in.Output, src, out)
		if errors.Is(err, errSilent) {
			p.note(CheckSound, "The source's track is silent, so there is no sound to score against.")
			continue
		}
		if err != nil {
			if unread := p.outputUnread(CheckSound, threshold, method, err); unread != nil {
				return unread
			}
			continue
		}
		p.record(Check{
			Name: CheckSound, Pass: score >= floor, Score: score,
			Measured:  fmt.Sprintf("SDR %s, aligned by %d samples", decibels(score), lag),
			Threshold: threshold, Method: method,
		})
	}
	return nil
}

// streamByIndex returns the stream of info at index, or an empty stream.
func streamByIndex(info probe.Info, index int) probe.Stream {
	for _, s := range info.Streams {
		if s.Index == index {
			return s
		}
	}
	return probe.Stream{}
}

// soundLevel proves encoded audio tracks hold the source's sound at its level, for footage with no
// floor. A lossy codec keeps real sound while moving its waveform, so the score that holds generated
// tones to a floor says nothing here: Opus to AAC reads -5 dB and sounds the same.
func (p *proof) soundLevel() error {
	pairs, ok := p.audioPairs(CheckSound)
	if !ok {
		return nil
	}
	method := "ffmpeg volumedetect on each whole track"
	for _, pair := range pairs {
		src, out := streamByIndex(p.src, pair[0]), streamByIndex(p.out, pair[1])
		slack := levelSlack
		if src.Channels != out.Channels {
			slack = downmixLevelSlack
		}
		threshold := fmt.Sprintf("mean level within %.0f dB of the source's", slack)
		source, err := p.in.Tools.meanLevel(p.ctx, p.in.Source, src.Index)
		if err != nil {
			return fmt.Errorf("source level: %w", err)
		}
		if source <= silentLevel {
			p.note(CheckSound, "The source's track is silent, so there is no sound to compare.")
			continue
		}
		level, err := p.in.Tools.meanLevel(p.ctx, p.in.Output, out.Index)
		if err != nil {
			if unread := p.outputUnread(CheckSound, threshold, method, err); unread != nil {
				return unread
			}
			continue
		}
		p.record(Check{
			Name: CheckSound, Pass: math.Abs(level-source) <= slack,
			Measured:  fmt.Sprintf("mean level %.1f dB, source %.1f dB", level, source),
			Threshold: threshold, Method: method,
		})
	}
	p.note(CheckSound, "How close the sound stays to the source is not scored, since a lossy codec moves "+
		"a waveform without changing what is heard. Only its level is compared.")
	return nil
}

// meanLevel returns the mean level of one track in decibels, minus infinity for digital silence.
func (t Tools) meanLevel(ctx context.Context, path string, index int) (float64, error) {
	log, err := t.logged(ctx, "-hide_banner", "-nostdin", "-nostats", "-i", path,
		"-map", fmt.Sprintf("0:%d", index), "-af", "volumedetect", "-f", "null", "-")
	if err != nil {
		return 0, fmt.Errorf("mean level: %w", err)
	}
	m := meanVolume.FindSubmatch(log)
	if m == nil {
		return 0, errors.New("mean level: volumedetect printed no level")
	}
	level, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		return 0, fmt.Errorf("mean level: parse: %w", err)
	}
	return level, nil
}

// channels proves each audio track holds the planned channels and sample rate, and the source's
// layout when the count is the source's. A layout's side and back forms carry the same channels.
func (p *proof) channels() {
	want := p.in.Expect
	if want.Channels == 0 && want.SampleRate == 0 {
		return
	}
	source, hasSource := p.src.FirstAudio()
	for _, track := range p.out.Kind("audio") {
		measured := fmt.Sprintf("%d channels, %s, %d Hz",
			track.Channels, track.ChannelLayout, track.SampleRate)
		var threshold []string
		pass := true
		if want.Channels > 0 {
			threshold = append(threshold, fmt.Sprintf("%d channels", want.Channels))
			pass = pass && track.Channels == want.Channels
			if hasSource && source.Channels == want.Channels && source.ChannelLayout != "" &&
				track.ChannelLayout != "" {
				threshold = append(threshold, source.ChannelLayout)
				pass = pass && layoutClass(track.ChannelLayout) == layoutClass(source.ChannelLayout)
			}
		}
		if want.SampleRate > 0 {
			threshold = append(threshold, fmt.Sprintf("%d Hz", want.SampleRate))
			pass = pass && track.SampleRate == want.SampleRate
		}
		p.record(Check{
			Name: CheckChannels, Pass: pass, Measured: measured,
			Threshold: strings.Join(threshold, ", "), Method: "ffprobe channel layout and sample rate",
		})
	}
}

// layoutClass reads a channel layout with its side and back forms as one.
func layoutClass(layout string) string {
	return strings.TrimSuffix(strings.TrimSuffix(layout, "(side)"), "(back)")
}

// channelSamples returns one stream's samples by channel, leaving toneEdge seconds out of each end,
// since encoder delay and padding change what surrounds the tone.
func (t Tools) channelSamples(ctx context.Context, path string, s probe.Stream) ([][]float32, error) {
	filter := fmt.Sprintf("atrim=start=%g,areverse,atrim=start=%g,areverse,aformat=sample_fmts=flt",
		toneEdge, toneEdge)
	out, err := t.output(ctx, "-hide_banner", "-nostdin", "-v", "error", "-i", path,
		"-map", fmt.Sprintf("0:%d", s.Index), "-af", filter, "-f", "f32le", "-")
	if err != nil {
		return nil, fmt.Errorf("read samples: %w", err)
	}
	if s.Channels <= 0 {
		return nil, errors.New("read samples: no channels")
	}
	channels := make([][]float32, s.Channels)
	for i := 0; (i+1)*4 <= len(out); i++ {
		ch := i % s.Channels
		channels[ch] = append(channels[ch], math.Float32frombits(binary.LittleEndian.Uint32(out[i*4:])))
	}
	return channels, nil
}

// toneShare returns the share of a channel's power that sits at one frequency, through the
// Goertzel algorithm over the whole channel.
func toneShare(samples []float32, freq float64, rate int) float64 {
	if len(samples) == 0 || rate <= 0 {
		return 0
	}
	coeff := 2 * math.Cos(2*math.Pi*freq/float64(rate))
	var prev, prev2, power float64
	for _, sample := range samples {
		value := float64(sample)
		next := value + coeff*prev - prev2
		prev2, prev = prev, next
		power += value * value
	}
	if power == 0 {
		return 0
	}
	n := float64(len(samples))
	return 2 * (prev*prev + prev2*prev2 - coeff*prev*prev2) / (n * power)
}

// channelTones proves each channel of each audio track is led by the tone generated for that
// channel, so a swapped or lost channel fails. Each channel's power goes through a narrow filter at
// its tone, since quantizing noise moves a tone's zero crossings far more than it moves its power.
func (p *proof) channelTones() error {
	want := p.in.Expect.Tones
	if want == nil {
		return nil
	}
	method := "ffmpeg decoded samples, Goertzel power at each channel's tone"
	tracks := p.out.Kind("audio")
	if len(tracks) != len(want) {
		p.record(Check{
			Name: CheckChannelTones, Measured: fmt.Sprintf("%d audio tracks", len(tracks)),
			Threshold: fmt.Sprintf("%d audio tracks", len(want)), Method: method,
		})
		return nil
	}
	for i, track := range tracks {
		threshold := fmt.Sprintf("at least %.0f%% of each channel's power at %s",
			minToneShare*100, hertz(want[i]))
		samples, err := p.in.Tools.channelSamples(p.ctx, p.in.Output, track)
		if err != nil {
			if unread := p.outputUnread(CheckChannelTones, threshold, method, err); unread != nil {
				return unread
			}
			continue
		}
		pass := len(samples) == len(want[i])
		shares := make([]string, 0, len(samples))
		for ch, channel := range samples {
			if ch >= len(want[i]) {
				break
			}
			share := toneShare(channel, want[i][ch], track.SampleRate)
			shares = append(shares, fmt.Sprintf("%.0f%%", share*100))
			pass = pass && share >= minToneShare
		}
		p.record(Check{
			Name: CheckChannelTones, Pass: pass, Measured: strings.Join(shares, ", "),
			Threshold: threshold, Method: method,
		})
	}
	return nil
}

// hertz formats a list of frequencies.
func hertz(freqs []float64) string {
	parts := make([]string, 0, len(freqs))
	for _, f := range freqs {
		parts = append(parts, strconv.FormatFloat(f, 'f', 1, 64))
	}
	return strings.Join(parts, ", ") + " Hz"
}
