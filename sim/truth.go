package sim

import "time"

// Damage says how a broken recipe fails when read.
type Damage string

// The ways a broken recipe fails.
const (
	// DamageNone marks a whole file.
	DamageNone Damage = ""
	// DamageProbe marks a file ffprobe cannot read at all.
	DamageProbe Damage = "probe"
	// DamageDecode marks a file ffprobe reads but ffmpeg cannot decode to the end.
	DamageDecode Damage = "decode"
)

// Segment is a run of evenly spaced video frames.
type Segment struct {
	// FPS is the run's frame rate.
	FPS int
	// Frames is how many frames the run holds.
	Frames int
}

// Color holds the color tags a video stream states.
type Color struct {
	// Range is the sample range tag.
	Range string
	// Space is the matrix tag.
	Space string
	// Transfer is the transfer curve tag.
	Transfer string
	// Primaries is the primaries tag.
	Primaries string
}

// AudioTrack describes one audio stream.
type AudioTrack struct {
	// Codec is the codec short name.
	Codec string
	// Language is the stream's language tag, empty when untagged.
	Language string
	// Channels is the channel count.
	Channels int
	// SampleRate is the sample rate in hertz.
	SampleRate int
	// Tones holds each channel's tone in hertz, in channel order, empty when not pure tones.
	Tones []float64
}

// Line is one subtitle cue.
type Line struct {
	// Start is when the cue appears.
	Start time.Duration
	// End is when the cue goes away.
	End time.Duration
	// Text is what the cue says.
	Text string
}

// SubtitleTrack describes one subtitle stream.
type SubtitleTrack struct {
	// Codec is the codec short name.
	Codec string
	// Language is the stream's language tag.
	Language string
	// Lines holds the cues in order.
	Lines []Line
}

// Chapter is one chapter mark.
type Chapter struct {
	// Start is where the chapter begins.
	Start time.Duration
	// End is where the chapter ends.
	End time.Duration
	// Title is the chapter's name.
	Title string
}

// Truth holds what a recipe's file was built to contain.
type Truth struct {
	// Damage says how the file fails when read, DamageNone for a whole file.
	Damage Damage
	// Duration is how long the file plays.
	Duration time.Duration
	// VideoCodec is the video codec short name, empty without video.
	VideoCodec string
	// Segments lists the video's runs of evenly spaced frames, in order.
	Segments []Segment
	// Width is the coded frame width.
	Width int
	// Height is the coded frame height.
	Height int
	// SampleAspect is the shape of the pixels as ffprobe states it, empty when not asserted.
	SampleAspect string
	// Rotation is the display rotation as probe.Stream.Rotation reads it.
	Rotation int
	// FieldOrder is the field tag, empty when not asserted.
	FieldOrder string
	// CombProof marks footage idet reads true, so its reading proves combing or its absence.
	CombProof bool
	// PixFmt is the pixel format, empty when not asserted.
	PixFmt string
	// Color holds the color tags, zero when not asserted.
	Color Color
	// Mastering marks HDR mastering display metadata on the decoded frames.
	Mastering bool
	// Audio lists the audio streams in order.
	Audio []AudioTrack
	// Markers lists the times of a white flash and a tone burst that begin together.
	Markers []time.Duration
	// MarkerLength is how long each flash and burst lasts.
	MarkerLength time.Duration
	// Subtitles lists the subtitle streams in order.
	Subtitles []SubtitleTrack
	// Chapters lists the chapter marks in order.
	Chapters []Chapter
	// Tags holds the container tags, with lowercase names.
	Tags map[string]string
}

// Frames returns how many video frames the file holds.
func (t Truth) Frames() int {
	var total int
	for _, s := range t.Segments {
		total += s.Frames
	}
	return total
}

// FrameTimes returns each video frame's presentation time, relative to the first frame.
func (t Truth) FrameTimes() []time.Duration {
	times := make([]time.Duration, 0, t.Frames())
	var base time.Duration
	for _, s := range t.Segments {
		for i := range s.Frames {
			times = append(times, base+time.Duration(i)*time.Second/time.Duration(s.FPS))
		}
		base += time.Duration(s.Frames) * time.Second / time.Duration(s.FPS)
	}
	return times
}

// DisplaySize returns the frame size as shown to the viewer.
func (t Truth) DisplaySize() (int, int) {
	if t.Rotation == 90 || t.Rotation == 270 {
		return t.Height, t.Width
	}
	return t.Width, t.Height
}
