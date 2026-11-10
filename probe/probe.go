// Package probe reads media file metadata through ffprobe.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dcadolph/icelos-ffmpeg/ffmsg"
	"github.com/dcadolph/icelos-ffmpeg/proc"
)

// Stream describes one stream inside a media file.
type Stream struct {
	// Index is the stream position in the container.
	Index int `json:"index"`
	// Kind is "video", "audio", or "subtitle".
	Kind string `json:"codec_type"`
	// Codec is the codec short name, e.g. "h264".
	Codec string `json:"codec_name"`
	// Width is the frame width in pixels for video streams.
	Width int `json:"width"`
	// Height is the frame height in pixels for video streams.
	Height int `json:"height"`
	// Channels is the channel count for audio streams.
	Channels int `json:"channels"`
	// ChannelLayout names how an audio stream's channels are arranged, such as "5.1(side)".
	ChannelLayout string `json:"channel_layout,omitempty"`
	// SampleRate is the sample rate in hertz for audio streams, zero when unknown.
	SampleRate int `json:"sample_rate,string,omitempty"`
	// SampleFmt is the sample format the decoder gives, such as s16 or fltp.
	SampleFmt string `json:"sample_fmt,omitempty"`
	// RawBits is how many bits of each sample the stream holds, zero when it does not say.
	RawBits int `json:"bits_per_raw_sample,string,omitempty"`
	// AvgFrameRate is the average frame rate of a video stream as a fraction, such as "30000/1001".
	AvgFrameRate string `json:"avg_frame_rate,omitempty"`
	// BaseFrameRate is the lowest frame rate that represents every timestamp, as a fraction.
	BaseFrameRate string `json:"r_frame_rate,omitempty"`
	// BFrames is how many frames the decoder holds back to reorder B-frames, zero without them.
	BFrames int `json:"has_b_frames,omitempty"`
	// Bitrate is the stream's bits per second where the container records it, zero otherwise.
	Bitrate int `json:"bit_rate,string,omitempty"`
	// Duration is the stream's playing time in seconds as a decimal string, empty when the
	// container records none, as Matroska does.
	Duration string `json:"duration,omitempty"`
	// PixFmt is the pixel format of a video stream, such as "yuv420p10le".
	PixFmt string `json:"pix_fmt,omitempty"`
	// FieldOrder is how a video stream stores its fields, such as "progressive" or "tt".
	FieldOrder string `json:"field_order,omitempty"`
	// SampleAspect is the shape of a video stream's pixels as a ratio, such as "64:45".
	SampleAspect string `json:"sample_aspect_ratio,omitempty"`
	// ColorRange is the tagged sample range of a video stream, such as "tv".
	ColorRange string `json:"color_range,omitempty"`
	// ColorSpace is the tagged matrix of a video stream, such as "bt2020nc".
	ColorSpace string `json:"color_space,omitempty"`
	// ColorTransfer is the tagged transfer curve of a video stream, such as "arib-std-b67".
	ColorTransfer string `json:"color_transfer,omitempty"`
	// ColorPrimaries is the tagged primaries of a video stream, such as "bt2020".
	ColorPrimaries string `json:"color_primaries,omitempty"`
	// Disposition carries the stream role flags.
	Disposition Disposition `json:"disposition"`
	// Hidden is how much footage an edit list keeps this stream from playing, zero without one.
	Hidden time.Duration `json:"hiddenNs,omitempty"`
	// Tags carries the stream's language and title.
	Tags Tags `json:"tags,omitzero"`
	// SideData carries per-stream extras such as the display matrix.
	SideData []SideData `json:"side_data_list"`
}

// Disposition holds the ffprobe role flags for a stream.
type Disposition struct {
	// AttachedPic is 1 when the stream is embedded cover art rather than motion video.
	AttachedPic int `json:"attached_pic"`
}

// Tags holds the stream tags icelos reads: the language and title shown when choosing tracks,
// and the duration Matroska records in place of a stream duration.
type Tags struct {
	// Language is the ISO 639-2 language code, such as "eng", empty when untagged.
	Language string `json:"language,omitempty"`
	// Title is the track name the file gives, empty when untagged.
	Title string `json:"title,omitempty"`
	// Artist is the artist the track names, which Ogg files keep on the stream, empty when untagged.
	Artist string `json:"artist,omitempty"`
	// Duration is the stream's playing time as hours, minutes, and seconds, such as
	// "00:00:02.028000000", empty when untagged.
	Duration string `json:"DURATION,omitempty"`
}

// SideData is one ffprobe side data entry.
type SideData struct {
	// Rotation is the display rotation in degrees from a display matrix entry.
	Rotation int `json:"rotation"`
}

// Tag returns the file's tag of the given name, title or artist. Ogg files keep these on the stream
// rather than the container.
func (i Info) Tag(name string) string {
	if value := i.Tags[name]; value != "" || !slices.Contains(strings.Split(i.Format, ","), "ogg") {
		return value
	}
	for _, s := range i.Streams {
		switch {
		case name == "title" && s.Tags.Title != "":
			return s.Tags.Title
		case name == "artist" && s.Tags.Artist != "":
			return s.Tags.Artist
		}
	}
	return ""
}

// IsCover reports whether the stream is embedded cover art.
func (s Stream) IsCover() bool {
	return s.Disposition.AttachedPic == 1
}

// SampleDepth returns how many bits an audio stream's samples hold when they are whole numbers, and
// zero for floating point samples, which a lossy decoder gives and no lossless codec keeps exactly.
func (s Stream) SampleDepth() int {
	switch s.SampleFmt {
	case "u8", "u8p":
		return 8
	case "s16", "s16p":
		return 16
	case "s32", "s32p":
		if s.RawBits > 0 {
			return s.RawBits
		}
		return 32
	}
	return 0
}

// DisplaySize returns the frame size as shown to the viewer, swapping width and height when
// the stream carries a quarter-turn rotation.
func (s Stream) DisplaySize() (int, int) {
	if r := s.Rotation(); r == 90 || r == 270 {
		return s.Height, s.Width
	}
	return s.Width, s.Height
}

// Rotation returns the display rotation in degrees from 0 up to 360, zero when the stream carries
// none.
func (s Stream) Rotation() int {
	for _, sd := range s.SideData {
		if r := ((sd.Rotation % 360) + 360) % 360; r != 0 {
			return r
		}
	}
	return 0
}

// FrameRate returns the average frame rate in frames per second, falling back to the base rate,
// or zero when ffprobe reported neither.
func (s Stream) FrameRate() float64 {
	for _, r := range []string{s.AvgFrameRate, s.BaseFrameRate} {
		if fps := parseRate(r); fps > 0 {
			return fps
		}
	}
	return 0
}

// parseRate parses a frame rate fraction such as "30000/1001" or a plain number, returning zero
// for anything else.
func parseRate(r string) float64 {
	num, den, found := strings.Cut(r, "/")
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n <= 0 {
		return 0
	}
	if !found {
		return n
	}
	d, err := strconv.ParseFloat(den, 64)
	if err != nil || d <= 0 {
		return 0
	}
	return n / d
}

// PlayTime returns the stream's playing time from its duration or its Matroska duration tag,
// and zero when it records neither in a form that parses.
func (s Stream) PlayTime() time.Duration {
	if d, err := seconds(s.Duration); err == nil && d > 0 {
		return d
	}
	return clock(s.Tags.Duration)
}

// clock parses an hours, minutes, and seconds time such as "00:00:02.028000000", returning zero
// for anything else.
func clock(s string) time.Duration {
	h, rest, ok := strings.Cut(s, ":")
	if !ok {
		return 0
	}
	m, sec, ok := strings.Cut(rest, ":")
	if !ok {
		return 0
	}
	hours, herr := strconv.Atoi(h)
	mins, merr := strconv.Atoi(m)
	secs, serr := strconv.ParseFloat(sec, 64)
	if herr != nil || merr != nil || serr != nil || hours < 0 || mins < 0 || secs < 0 {
		return 0
	}
	return time.Duration(hours)*time.Hour + time.Duration(mins)*time.Minute +
		time.Duration(math.Round(secs*float64(time.Second)))
}

// MediaDuration returns the longest playing time among the motion video and audio streams, the
// span a bitrate spreads over. A subtitle track can run past the picture and sound and stretch
// the container's duration, so the container value is used only when no video or audio stream
// records one.
func (i Info) MediaDuration() time.Duration {
	var longest time.Duration
	for _, s := range i.Streams {
		if (s.Kind == "video" && !s.IsCover()) || s.Kind == "audio" {
			longest = max(longest, s.PlayTime())
		}
	}
	if longest > 0 {
		return longest
	}
	return i.Duration
}

// FirstVideo returns the first motion video stream.
func (i Info) FirstVideo() (Stream, bool) {
	for _, s := range i.Streams {
		if s.Kind == "video" && !s.IsCover() {
			return s, true
		}
	}
	return Stream{}, false
}

// FirstAudio returns the first audio stream.
func (i Info) FirstAudio() (Stream, bool) {
	for _, s := range i.Streams {
		if s.Kind == "audio" {
			return s, true
		}
	}
	return Stream{}, false
}

// Kind returns the streams of the given kind in file order, so position n is the stream ffmpeg
// selects with the specifier "a:n" or "s:n".
func (i Info) Kind(kind string) []Stream {
	var out []Stream
	for _, s := range i.Streams {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// Chapter is one chapter mark in a media file.
type Chapter struct {
	// Start is where the chapter begins.
	Start time.Duration `json:"startNs"`
	// End is where the chapter ends.
	End time.Duration `json:"endNs"`
	// Title is the chapter name, empty when untitled.
	Title string `json:"title,omitempty"`
}

// Info holds the metadata for one media file.
type Info struct {
	// Format is the container short name list, e.g. "mov,mp4,m4a,3gp,3g2,mj2".
	Format string `json:"format"`
	// Duration is the total playing time.
	Duration time.Duration `json:"durationNs"`
	// Size is the file size in bytes.
	Size int64 `json:"size"`
	// Streams lists every stream in the file.
	Streams []Stream `json:"streams"`
	// Chapters lists the chapter marks in playing order.
	Chapters []Chapter `json:"chapters,omitempty"`
	// Tags holds the container's tags under lowercase names, since Matroska writes them in capitals.
	Tags map[string]string `json:"tags,omitempty"`
}

// rawOutput mirrors the ffprobe JSON document.
type rawOutput struct {
	// Format is the container section.
	Format struct {
		// Name is the container format name.
		Name string `json:"format_name"`
		// Duration is the playing time in seconds as a decimal string.
		Duration string `json:"duration"`
		// Size is the file size in bytes as a decimal string.
		Size string `json:"size"`
		// Tags holds the container's tags as the file names them.
		Tags map[string]string `json:"tags"`
	} `json:"format"`
	// Streams is the stream section.
	Streams []Stream `json:"streams"`
	// Chapters is the chapter section.
	Chapters []rawChapter `json:"chapters"`
}

// rawChapter mirrors one ffprobe chapter entry.
type rawChapter struct {
	// StartTime is the start in seconds as a decimal string.
	StartTime string `json:"start_time"`
	// EndTime is the end in seconds as a decimal string.
	EndTime string `json:"end_time"`
	// Tags carries the chapter title.
	Tags struct {
		// Title is the chapter name.
		Title string `json:"title"`
	} `json:"tags"`
}

// Parse decodes ffprobe JSON output into Info.
func Parse(data []byte) (Info, error) {
	var raw rawOutput
	if err := json.Unmarshal(data, &raw); err != nil {
		return Info{}, fmt.Errorf("%w: decode: %w", ErrProbe, err)
	}
	info := Info{Format: raw.Format.Name, Streams: raw.Streams}
	for name, value := range raw.Format.Tags {
		if info.Tags == nil {
			info.Tags = make(map[string]string, len(raw.Format.Tags))
		}
		info.Tags[strings.ToLower(name)] = value
	}
	if raw.Format.Duration != "" {
		d, err := seconds(raw.Format.Duration)
		if err != nil {
			return Info{}, fmt.Errorf("%w: duration: %w", ErrProbe, err)
		}
		info.Duration = d
	}
	for _, c := range raw.Chapters {
		start, err := seconds(c.StartTime)
		if err != nil {
			return Info{}, fmt.Errorf("%w: chapter start: %w", ErrProbe, err)
		}
		end, err := seconds(c.EndTime)
		if err != nil {
			return Info{}, fmt.Errorf("%w: chapter end: %w", ErrProbe, err)
		}
		info.Chapters = append(info.Chapters, Chapter{Start: start, End: end, Title: c.Tags.Title})
	}
	if raw.Format.Size != "" {
		size, err := strconv.ParseInt(raw.Format.Size, 10, 64)
		if err != nil {
			return Info{}, fmt.Errorf("%w: size: %w", ErrProbe, err)
		}
		info.Size = size
	}
	return info, nil
}

// seconds parses a decimal seconds string such as "12.5" into a duration.
func seconds(s string) (time.Duration, error) {
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// File runs the ffprobe found on PATH against path and returns the parsed metadata.
func File(ctx context.Context, path string) (Info, error) {
	return Run(ctx, "ffprobe", path)
}

// Run runs the ffprobe binary at bin against path and returns the parsed metadata. A file from the
// MP4 family is read a second time with its edit lists ignored, which shows how much footage each
// stream's list hides.
func Run(ctx context.Context, bin, path string) (Info, error) {
	info, err := run(ctx, bin, path)
	if err != nil || !strings.Contains(info.Format, "mov") {
		return info, err
	}
	raw, err := run(ctx, bin, path, "-ignore_editlist", "1")
	if err != nil {
		return Info{}, err
	}
	for i, s := range info.Streams {
		for _, r := range raw.Streams {
			if r.Index == s.Index {
				info.Streams[i].Hidden = max(r.PlayTime()-s.PlayTime(), 0)
			}
		}
	}
	return info, nil
}

// run reads path once, with extra as demuxer options placed before the input.
func run(ctx context.Context, bin, path string, extra ...string) (Info, error) {
	args := []string{
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "-show_chapters",
	}
	args = append(append(args, extra...), "-i", path)
	out, err := proc.Command(ctx, bin, args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return Info{}, fmt.Errorf("%w: %s: %s", ErrProbe, filepath.Base(path), ffmsg.Plain(string(exitErr.Stderr)))
		}
		return Info{}, fmt.Errorf("%w: %s: %w", ErrProbe, filepath.Base(path), err)
	}
	return Parse(out)
}
