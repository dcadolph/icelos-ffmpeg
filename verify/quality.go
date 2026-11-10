package verify

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// deinterlaceReference matches the filter the engine deinterlaces with.
const deinterlaceReference = "yadif=mode=send_frame"

// minCaptionLift is how much more a burned caption must change its box than the encode alone does,
// in 8-bit luma steps.
const minCaptionLift = 5

// Caption says where and when a burned caption shows.
type Caption struct {
	// At is a time when the caption is on screen.
	At time.Duration `json:"atNs,omitempty"`
	// Clear is a time when no caption is on screen.
	Clear time.Duration `json:"clearNs,omitempty"`
	// X is the box's left edge as a share of the shown width.
	X float64 `json:"x,omitempty"`
	// Y is the box's top edge as a share of the shown height.
	Y float64 `json:"y,omitempty"`
	// W is the box's width as a share of the shown width.
	W float64 `json:"w,omitempty"`
	// H is the box's height as a share of the shown height.
	H float64 `json:"h,omitempty"`
}

var (
	// ssimAll matches the overall score on the ssim filter's closing line.
	ssimAll = regexp.MustCompile(`SSIM .*All:([0-9.]+)`)
	// psnrAverage matches the average on the psnr filter's closing line.
	psnrAverage = regexp.MustCompile(`PSNR .*average:([0-9.a-z]+)`)
	// lumaAverage matches the mean luma signalstats prints for a frame.
	lumaAverage = regexp.MustCompile(`signalstats\.YAVG=([0-9.]+)`)
)

// pictureScore returns the SSIM and PSNR of a stream of output against one of source, both scaled
// to w by h. The two pair frame for frame, since a container that rounds its times, as Matroska does
// to the millisecond, would otherwise pair a frame with its neighbor. A rate above zero first resamples
// both to that rate, for an output whose frame count differs on purpose: a new frame rate, or an
// encoder that skips repeats, as Theora does. A deinterlaced output scores against the source put
// through the same filter, since combing it no longer has is not a loss.
func (t Tools) pictureScore(ctx context.Context, source, output string, srcIndex, outIndex, w, h int,
	deinterlaced bool, rate float64) (float64, float64, error) {
	side := "[%d:%d]%ssettb=1/1000,setpts=N,scale=%d:%d:flags=bicubic,format=yuv420p,split[%s1][%s2]"
	resample := ""
	if rate > 0 {
		resample = fmt.Sprintf("fps=%v,", rate)
	}
	reference := resample
	if deinterlaced {
		reference = deinterlaceReference + "," + resample
	}
	graph := strings.Join([]string{
		fmt.Sprintf(side, 0, outIndex, resample, w, h, "d", "d"),
		fmt.Sprintf(side, 1, srcIndex, reference, w, h, "r", "r"),
		"[d1][r1]ssim", "[d2][r2]psnr",
	}, ";")
	log, err := t.logged(ctx, "-hide_banner", "-nostdin", "-nostats", "-i", output, "-i", source,
		"-filter_complex", graph, "-f", "null", "-")
	if err != nil {
		return 0, 0, fmt.Errorf("picture score: %w", err)
	}
	ssim, err := lastFloat(ssimAll, log)
	if err != nil {
		return 0, 0, fmt.Errorf("picture score: ssim: %w", err)
	}
	psnr, err := lastFloat(psnrAverage, log)
	if err != nil {
		return 0, 0, fmt.Errorf("picture score: psnr: %w", err)
	}
	return ssim, psnr, nil
}

// lastFloat returns the number in the last match of re in log.
func lastFloat(re *regexp.Regexp, log []byte) (float64, error) {
	all := re.FindAllSubmatch(log, -1)
	if len(all) == 0 {
		return 0, fmt.Errorf("no match for %s", re)
	}
	value, err := strconv.ParseFloat(string(all[len(all)-1][1]), 64)
	if err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	return value, nil
}

// picture proves an encoded picture scores at or above the plan's floor against the source.
func (p *proof) picture() error {
	if !p.hasVideo || p.in.Expect.VideoCopied || p.in.Expect.LosslessVideo {
		return nil
	}
	floor := p.in.Expect.MinSSIM
	if floor <= 0 {
		p.note(CheckPicture, "The plan states no picture floor for this encode.")
		return nil
	}
	threshold := fmt.Sprintf("SSIM at least %.4f", floor)
	method := "ffmpeg ssim and psnr at the output's shown size"
	w, h := p.outVideo.DisplaySize()
	rate := p.in.Expect.Rate
	if len(p.outScan.Times) == len(p.srcScan.Times) {
		rate = 0
	}
	ssim, psnr, err := p.in.Tools.pictureScore(p.ctx, p.in.Source, p.in.Output,
		p.srcVideo.Index, p.outVideo.Index, w, h,
		interlacedTag(p.srcVideo.FieldOrder) && !interlacedTag(p.outVideo.FieldOrder),
		rate)
	if err != nil {
		return p.outputUnread(CheckPicture, threshold, method, err)
	}
	p.record(Check{
		Name: CheckPicture, Pass: ssim >= floor, Score: ssim,
		Measured:  fmt.Sprintf("SSIM %.4f, PSNR %s", ssim, decibels(psnr)),
		Threshold: threshold, Method: method,
	})
	return nil
}

// decibels formats a decibel value, naming a perfect score the way a person would read it.
func decibels(value float64) string {
	if math.IsInf(value, 1) {
		return "perfect"
	}
	return fmt.Sprintf("%.1f dB", value)
}

// boxDifference returns the mean luma difference between a box of the output's frame at time at
// and the same box of the source's frame.
func (t Tools) boxDifference(ctx context.Context, source, output string, srcIndex, outIndex int,
	at time.Duration, c Caption) (float64, error) {
	seek := strconv.FormatFloat(at.Seconds(), 'f', 3, 64)
	side := "[%d:%d]crop=iw*%g:ih*%g:iw*%g:ih*%g,format=yuv420p,setpts=PTS-STARTPTS[%s]"
	graph := strings.Join([]string{
		fmt.Sprintf(side, 0, outIndex, c.W, c.H, c.X, c.Y, "d"),
		fmt.Sprintf(side, 1, srcIndex, c.W, c.H, c.X, c.Y, "r"),
		"[d][r]blend=all_mode=difference,signalstats," +
			"metadata=print:key=lavfi.signalstats.YAVG:file=-",
	}, ";")
	out, err := t.output(ctx, "-hide_banner", "-nostdin", "-v", "error",
		"-ss", seek, "-i", output, "-ss", seek, "-i", source,
		"-filter_complex", graph, "-frames:v", "1", "-f", "null", "-")
	if err != nil {
		return 0, fmt.Errorf("caption box: %w", err)
	}
	diff, err := lastFloat(lumaAverage, out)
	if err != nil {
		return 0, fmt.Errorf("caption box: %w", err)
	}
	return diff, nil
}

// burned proves a burned caption changed its box against the source while it shows, by more than
// the encode changed the same box while it does not. Thin text on bright footage barely moves the
// box's mean brightness, so the proof compares each frame to the source's instead.
func (p *proof) burned() error {
	caption := p.in.Expect.Caption
	if caption == nil {
		return nil
	}
	threshold := fmt.Sprintf("at least %d luma steps more than without the caption", minCaptionLift)
	method := "ffmpeg difference blend and signalstats over the caption's box"
	if !p.hasVideo {
		p.record(Check{
			Name: CheckBurned, Measured: "no video stream", Threshold: threshold, Method: method,
		})
		return nil
	}
	shown, err := p.in.Tools.boxDifference(p.ctx, p.in.Source, p.in.Output, p.srcVideo.Index,
		p.outVideo.Index, caption.At, *caption)
	if err != nil {
		return p.outputUnread(CheckBurned, threshold, method, err)
	}
	clear, err := p.in.Tools.boxDifference(p.ctx, p.in.Source, p.in.Output, p.srcVideo.Index,
		p.outVideo.Index, caption.Clear, *caption)
	if err != nil {
		return p.outputUnread(CheckBurned, threshold, method, err)
	}
	lift := shown - clear
	p.record(Check{
		Name: CheckBurned, Pass: lift >= minCaptionLift, Score: lift,
		Measured:  fmt.Sprintf("%.1f luma steps with the caption, %.1f without", shown, clear),
		Threshold: threshold, Method: method,
	})
	return nil
}
