package verify

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dcadolph/icelos-ffmpeg/probe"
	"github.com/dcadolph/icelos-ffmpeg/proc"
)

// Tools names the binaries the proofs run. An empty field means the one found on PATH.
type Tools struct {
	// FFmpeg is the ffmpeg binary.
	FFmpeg string
	// FFprobe is the ffprobe binary.
	FFprobe string
}

// ffmpeg returns the ffmpeg binary to run.
func (t Tools) ffmpeg() string {
	if t.FFmpeg != "" {
		return t.FFmpeg
	}
	return "ffmpeg"
}

// ffprobe returns the ffprobe binary to run.
func (t Tools) ffprobe() string {
	if t.FFprobe != "" {
		return t.FFprobe
	}
	return "ffprobe"
}

// version returns the first line of ffmpeg's version output, which also proves ffmpeg runs.
func (t Tools) version(ctx context.Context) (string, error) {
	out, err := proc.Command(ctx, t.ffmpeg(), "-hide_banner", "-version").Output()
	if err != nil {
		return "", fmt.Errorf("ffmpeg version: %w", err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line), nil
}

// probe reads the metadata of path.
func (t Tools) probe(ctx context.Context, path string) (probe.Info, error) {
	return probe.Run(ctx, t.ffprobe(), path)
}

// frameScan is what one decode pass over a video stream found.
type frameScan struct {
	// Times holds each decoded frame's presentation time, relative to the first frame.
	Times []time.Duration
	// Errors holds the error lines the decode logged.
	Errors []string
	// Comb holds the idet totals, zero unless the pass asked for them.
	Comb idetCounts
}

// Span returns how long the scanned frames play, counting the last frame as long as the one
// before it, and zero with fewer than two frames.
func (s frameScan) Span() time.Duration {
	n := len(s.Times)
	if n < 2 {
		return 0
	}
	return s.Times[n-1] + s.Times[n-1] - s.Times[n-2]
}

// idetCounts holds the totals of the idet filter's multi frame detection.
type idetCounts struct {
	// TFF counts frames read as top field first.
	TFF int
	// BFF counts frames read as bottom field first.
	BFF int
	// Progressive counts frames read as progressive.
	Progressive int
	// Undetermined counts frames idet could not place.
	Undetermined int
}

// Share returns the fraction of frames read as interlaced, and zero when idet read none.
func (c idetCounts) Share() float64 {
	total := c.TFF + c.BFF + c.Progressive + c.Undetermined
	if total == 0 {
		return 0
	}
	return float64(c.TFF+c.BFF) / float64(total)
}

// scanVideo decodes one video stream once and collects its frame times, errors, and, when comb is
// set, idet totals. Packing frames with wrapped_avframe costs no pixel hashing, and the time base
// follows the filter so a variable rate reads true. With strict set the decode stops at the first
// error. The returned error is set only when the pass could not start or the context ended.
func (t Tools) scanVideo(ctx context.Context, path string, index int, comb, strict bool) (frameScan, error) {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "level+info"}
	if strict {
		args = append(args, "-xerror")
	}
	args = append(args, "-noautorotate", "-i", path, "-map", fmt.Sprintf("0:%d", index))
	if comb {
		args = append(args, "-vf", "idet")
	}
	args = append(args, "-fps_mode", "passthrough", "-enc_time_base", "filter",
		"-c:v", "wrapped_avframe", "-f", "framecrc", "pipe:1")
	var stdout, stderr bytes.Buffer
	cmd := proc.Command(ctx, t.ffmpeg(), args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if err := ctx.Err(); err != nil {
		return frameScan{}, err
	}
	if runErr != nil && !isExit(runErr) {
		return frameScan{}, runErr
	}
	times, err := parseFrames(stdout.Bytes())
	if err != nil {
		return frameScan{}, err
	}
	scan := frameScan{Times: times, Errors: errorLines(stderr.Bytes())}
	if comb {
		scan.Comb = parseIdet(stderr.Bytes())
	}
	if runErr != nil && len(scan.Errors) == 0 {
		scan.Errors = []string{runErr.Error()}
	}
	return scan, nil
}

// isExit reports whether err is the tool exiting with a failure, rather than failing to start.
func isExit(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// audioSpan returns how long an audio stream's decoded frames play, from the start of the first to
// the end of the last.
func (t Tools) audioSpan(ctx context.Context, path string, index int) (time.Duration, error) {
	out, err := t.output(ctx, "-hide_banner", "-nostdin", "-v", "error", "-i", path,
		"-map", fmt.Sprintf("0:%d", index), "-f", "framecrc", "-")
	if err != nil {
		return 0, fmt.Errorf("decoded audio: %w", err)
	}
	return parseSpan(out)
}

// parseSpan reads framecrc output into the time from the first frame's start to the last one's end.
func parseSpan(data []byte) (time.Duration, error) {
	num, den, first, end := 0.0, 0.0, 0.0, 0.0
	seen := false
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "#tb 0: "); ok {
			n, d, _ := strings.Cut(strings.TrimSpace(rest), "/")
			var nerr, derr error
			num, nerr = strconv.ParseFloat(n, 64)
			den, derr = strconv.ParseFloat(d, 64)
			if nerr != nil || derr != nil || num <= 0 || den <= 0 {
				return 0, fmt.Errorf("frame time base: %q", strings.TrimSpace(rest))
			}
			continue
		}
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			return 0, fmt.Errorf("frame line: %q", strings.TrimSpace(line))
		}
		pts, perr := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
		length, lerr := strconv.ParseFloat(strings.TrimSpace(fields[3]), 64)
		if perr != nil || lerr != nil || den == 0 {
			return 0, fmt.Errorf("frame line: %q", strings.TrimSpace(line))
		}
		if !seen {
			first, seen = pts, true
		}
		end = pts + length
	}
	if !seen {
		return 0, errors.New("no frames")
	}
	return time.Duration((end - first) * num / den * float64(time.Second)), nil
}

// parseFrames reads framecrc output into frame times relative to the first frame, scaling the
// presentation time column by the time base header.
func parseFrames(data []byte) ([]time.Duration, error) {
	var times []time.Duration
	num, den := 0.0, 0.0
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "#tb 0: "); ok {
			n, d, found := strings.Cut(strings.TrimSpace(rest), "/")
			var nerr, derr error
			num, nerr = strconv.ParseFloat(n, 64)
			den, derr = strconv.ParseFloat(d, 64)
			if !found || nerr != nil || derr != nil || num <= 0 || den <= 0 {
				return nil, fmt.Errorf("frame time base: %q", strings.TrimSpace(rest))
			}
			continue
		}
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			return nil, fmt.Errorf("frame line: %q", strings.TrimSpace(line))
		}
		pts, err := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
		if err != nil {
			return nil, fmt.Errorf("frame time: %w", err)
		}
		if den == 0 {
			return nil, errors.New("frame line before the time base")
		}
		times = append(times, time.Duration(pts*num/den*float64(time.Second)))
	}
	if len(times) > 0 {
		first := times[0]
		for i := range times {
			times[i] -= first
		}
	}
	return times, nil
}

// idetLine matches the totals idet logs when it closes.
var idetLine = regexp.MustCompile(
	`Multi frame detection: TFF:\s*(\d+)\s+BFF:\s*(\d+)\s+` +
		`Progressive:\s*(\d+)\s+Undetermined:\s*(\d+)`)

// parseIdet returns the totals on the last multi frame detection line of log, zero without one.
func parseIdet(log []byte) idetCounts {
	all := idetLine.FindAllSubmatch(log, -1)
	if len(all) == 0 {
		return idetCounts{}
	}
	m := all[len(all)-1]
	num := func(b []byte) int {
		n, err := strconv.Atoi(string(b))
		if err != nil {
			return 0
		}
		return n
	}
	return idetCounts{TFF: num(m[1]), BFF: num(m[2]), Progressive: num(m[3]), Undetermined: num(m[4])}
}

// errorLevel matches a log line at error severity or worse under -loglevel level+info. It reads
// the level tag only where ffmpeg writes one, so a file name holding the word does not count.
var errorLevel = regexp.MustCompile(`^(\[[^\]]+ @ [^\]]+\] )?\[(error|fatal|panic)\] `)

// errorLines returns the lines of log at error severity or worse.
func errorLines(log []byte) []string {
	var lines []string
	for line := range strings.Lines(string(log)) {
		if errorLevel.MatchString(line) {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

// decodeStreams decodes every video and audio stream of info in path except the one at skip, and
// returns what ffmpeg logged at error level. The returned error is set only when the pass could
// not start or the context ended.
func (t Tools) decodeStreams(ctx context.Context, path string, info probe.Info, skip int) ([]string, error) {
	var maps []string
	for _, s := range info.Streams {
		if s.Index != skip && (s.Kind == "video" || s.Kind == "audio") {
			maps = append(maps, "-map", fmt.Sprintf("0:%d", s.Index))
		}
	}
	if len(maps) == 0 {
		return nil, nil
	}
	args := append([]string{"-hide_banner", "-nostdin", "-v", "error", "-xerror", "-i", path}, maps...)
	args = append(args, "-f", "null", "-")
	var stderr bytes.Buffer
	cmd := proc.Command(ctx, t.ffmpeg(), args...)
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runErr != nil && !isExit(runErr) {
		return nil, runErr
	}
	var lines []string
	for line := range strings.Lines(stderr.String()) {
		if text := strings.TrimSpace(line); text != "" {
			lines = append(lines, text)
		}
	}
	if runErr != nil && len(lines) == 0 {
		lines = []string{runErr.Error()}
	}
	return lines, nil
}

// output runs ffmpeg with args and returns its standard output, naming the first line ffmpeg
// logged on a failure, since a damaged file can log thousands.
func (t Tools) output(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := proc.Command(ctx, t.ffmpeg(), args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if first, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); first != "" {
			return nil, fmt.Errorf("%w: %s", err, first)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// logged runs ffmpeg with args and returns what it logged, for the filters that report through the
// log.
func (t Tools) logged(ctx context.Context, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := proc.Command(ctx, t.ffmpeg(), args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		lines := errorLines(stderr.Bytes())
		if len(lines) > 0 {
			return nil, fmt.Errorf("%w: %s", err, lines[0])
		}
		return nil, err
	}
	return stderr.Bytes(), nil
}

// packetHash returns the SHA-256 of one stream's packets, without decoding them.
func (t Tools) packetHash(ctx context.Context, path string, index int) (string, error) {
	out, err := t.output(ctx, "-hide_banner", "-nostdin", "-v", "error", "-i", path,
		"-map", fmt.Sprintf("0:%d", index), "-c", "copy", "-f", "streamhash", "-hash", "sha256", "-")
	if err != nil {
		return "", fmt.Errorf("packet hash: %w", err)
	}
	_, sum, found := strings.Cut(string(out), "SHA256=")
	if !found {
		return "", fmt.Errorf("packet hash: no sum in %q", strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(sum), nil
}

// frameSums returns the MD5 of each decoded frame of one stream, with timestamps left out. The
// sums hold still when a copy reshaped its packets.
func (t Tools) frameSums(ctx context.Context, path string, index int) ([]string, error) {
	out, err := t.output(ctx, "-hide_banner", "-nostdin", "-v", "error", "-i", path,
		"-map", fmt.Sprintf("0:%d", index), "-fps_mode", "passthrough", "-f", "framemd5", "-")
	if err != nil {
		return nil, fmt.Errorf("frame hash: %w", err)
	}
	var sums []string
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(line, ",")
		if strings.HasPrefix(line, "#") || len(fields) != 6 {
			continue
		}
		sums = append(sums, strings.TrimSpace(fields[5]))
	}
	if len(sums) == 0 {
		return nil, errors.New("frame hash: no frames")
	}
	return sums, nil
}

// pcmSampleBytes is how many bytes one sample of the 32-bit decode holds.
const pcmSampleBytes = 4

// pcmHash returns the SHA-256 of one audio stream decoded to 32-bit samples.
func (t Tools) pcmHash(ctx context.Context, path string, index int) (string, error) {
	out, err := t.output(ctx, "-hide_banner", "-nostdin", "-v", "error", "-i", path,
		"-map", fmt.Sprintf("0:%d", index), "-c:a", "pcm_s32le", "-f", "hash", "-hash", "sha256", "-")
	if err != nil {
		return "", fmt.Errorf("sample hash: %w", err)
	}
	_, sum, found := strings.Cut(string(out), "SHA256=")
	if !found {
		return "", fmt.Errorf("sample hash: no sum in %q", strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(sum), nil
}

// paddedPCM decodes one audio stream of source and of output to 32-bit samples, and returns how many
// bytes follow the source's end in the output. It returns zero unless the output holds the source's
// exact samples followed by zeros, at most limit bytes of them.
func (t Tools) paddedPCM(ctx context.Context, source string, sourceIndex int, output string, outputIndex int,
	limit int64) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	src, waitSrc, err := t.pcmStream(ctx, source, sourceIndex)
	if err != nil {
		return 0, err
	}
	out, waitOut, err := t.pcmStream(ctx, output, outputIndex)
	if err != nil {
		cancel()
		waitSrc() //nolint:errcheck // Canceled, so its result says nothing.
		return 0, err
	}
	padded, err := paddedBytes(src, out, limit)
	cancel()
	waitSrc() //nolint:errcheck // Canceled, so its result says nothing.
	waitOut() //nolint:errcheck // Canceled, so its result says nothing.
	return padded, err
}

// pcmStream starts a decode of one audio stream to 32-bit samples and returns the samples and the
// function that waits for it.
func (t Tools) pcmStream(ctx context.Context, path string, index int) (io.Reader, func() error, error) {
	cmd := proc.Command(ctx, t.ffmpeg(), "-hide_banner", "-nostdin", "-v", "error", "-i", path,
		"-map", fmt.Sprintf("0:%d", index), "-c:a", "pcm_s32le", "-f", "s32le", "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("sample decode: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("sample decode: %w", err)
	}
	return bufio.NewReaderSize(out, 1<<20), cmd.Wait, nil
}

// paddedBytes reads src and out to src's end and returns how many bytes out holds past it, or zero
// unless they match and those bytes are all zero and no more than limit.
func paddedBytes(src, out io.Reader, limit int64) (int64, error) {
	a, b := make([]byte, 1<<16), make([]byte, 1<<16)
	for {
		n, err := io.ReadFull(src, a)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fmt.Errorf("sample read: %w", err)
		}
		end := err != nil
		if n > 0 {
			if _, err := io.ReadFull(out, b[:n]); err != nil {
				return 0, nil //nolint:nilerr // A shorter output is not padded, which zero says.
			}
			if !bytes.Equal(a[:n], b[:n]) {
				return 0, nil
			}
		}
		if end {
			break
		}
	}
	var padded int64
	for {
		n, err := out.Read(b)
		if slices.ContainsFunc(b[:n], func(c byte) bool { return c != 0 }) {
			return 0, nil
		}
		if padded += int64(n); padded > limit {
			return 0, nil
		}
		if errors.Is(err, io.EOF) {
			return padded, nil
		}
		if err != nil {
			return 0, fmt.Errorf("sample read: %w", err)
		}
	}
}
