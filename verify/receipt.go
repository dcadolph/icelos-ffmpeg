package verify

import "fmt"

// Names of the checks a receipt can hold.
const (
	// CheckContainer proves ffprobe can parse the output and finds streams in it.
	CheckContainer = "container"
	// CheckDecode proves every video and audio stream decodes without an error.
	CheckDecode = "decode"
	// CheckStreams proves the output holds the planned streams in order.
	CheckStreams = "streams"
	// CheckDuration proves the output plays as long as its source stream.
	CheckDuration = "duration"
	// CheckFrames proves the decoded frame count is the one the plan implies.
	CheckFrames = "frames"
	// CheckTiming proves each frame shows when the source's frame shows.
	CheckTiming = "timing"
	// CheckDisplay proves the shown width and height equal the source's.
	CheckDisplay = "display size"
	// CheckSampleAspect proves the shape of the pixels as shown equals the source's.
	CheckSampleAspect = "sample aspect"
	// CheckAspect proves the shape of the picture as shown equals the source's.
	CheckAspect = "aspect"
	// CheckRotation proves rotation went into the pixels on encode and stayed as data on copy.
	CheckRotation = "rotation"
	// CheckColor proves every color tag the source states survived.
	CheckColor = "color"
	// CheckBitDepth proves the pixel format is the planned one.
	CheckBitDepth = "bit depth"
	// CheckFieldOrder proves the field tag is progressive on encode and unchanged on copy.
	CheckFieldOrder = "field order"
	// CheckCombing proves the decoded picture holds no combing, on sources known to read true.
	CheckCombing = "combing"
	// CheckCopiedVideo proves copied video holds the source's packets or decoded frames.
	CheckCopiedVideo = "copied video"
	// CheckCopiedAudio proves copied audio holds the source's packets or decoded frames.
	CheckCopiedAudio = "copied audio"
	// CheckLosslessVideo proves an encoded video stream decodes to the source's exact frames.
	CheckLosslessVideo = "lossless video"
	// CheckLosslessAudio proves an encoded audio stream decodes to the source's exact samples.
	CheckLosslessAudio = "lossless audio"
	// CheckPicture proves an encoded picture scores at or above its floor against the source.
	CheckPicture = "picture"
	// CheckSound proves an encoded audio track scores at or above its floor against the source.
	CheckSound = "sound"
	// CheckChannels proves an audio track holds the planned channels and sample rate.
	CheckChannels = "channels"
	// CheckChannelTones proves each channel of a generated source came out on the same channel.
	CheckChannelTones = "channel tones"
	// CheckSync proves generated flashes and bursts land together and on their schedule.
	CheckSync = "sync"
	// CheckBurned proves a burned caption changed its box while it shows.
	CheckBurned = "burned caption"
	// CheckChapters proves the chapters were kept in place, or dropped when the plan drops them.
	CheckChapters = "chapters"
	// CheckMetadata proves the title and language tags were kept, or dropped when the plan drops them.
	CheckMetadata = "metadata"
)

// SchemaVersion is the receipt layout Verify writes. Version 2 carries the plan's Expect, so a
// receipt can be checked again from the files alone.
const SchemaVersion = 2

// NoteSource names the note that says the source itself is damaged.
const NoteSource = "source"

// Check is one proof run against an output.
type Check struct {
	// Name is one of the Check constants.
	Name string `json:"name"`
	// Pass reports whether the measured value met the threshold.
	Pass bool `json:"pass"`
	// Measured is the value found in the output.
	Measured string `json:"measured"`
	// Score is the measured number on a check with a floor, zero on the others.
	Score float64 `json:"score,omitempty"`
	// Threshold is what the value had to be to pass.
	Threshold string `json:"threshold"`
	// Method says how the value was measured.
	Method string `json:"method"`
}

// Note is a proof that did not run, and why, so a receipt never claims more than it measured.
type Note struct {
	// Name is one of the Check constants.
	Name string `json:"name"`
	// Reason says why the proof did not run.
	Reason string `json:"reason"`
}

// Receipt holds what was proven about one output and what was not.
type Receipt struct {
	// Schema is the receipt layout, SchemaVersion on a receipt Verify returns.
	Schema int `json:"schema"`
	// Output is the file the proofs ran on.
	Output string `json:"output"`
	// Source is the file the output was made from.
	Source string `json:"source"`
	// Checks lists every proof that ran.
	Checks []Check `json:"checks"`
	// Unchecked lists every proof that did not run.
	Unchecked []Note `json:"unchecked,omitempty"`
	// Tool is the ffmpeg version line that ran the proofs.
	Tool string `json:"tool"`
	// Expect is what the plan said the output should be, which the proofs ran against.
	Expect Expect `json:"expect"`
}

// Passed reports whether at least one proof ran and none failed.
func (r Receipt) Passed() bool {
	return len(r.Checks) > 0 && len(r.Failed()) == 0
}

// Summary returns one line a person can read, naming the first proof that failed.
func (r Receipt) Summary() string {
	if failed := r.Failed(); len(failed) > 0 {
		return fmt.Sprintf("Check failed: %s, measured %s, needs %s.", failed[0].Name, failed[0].Measured,
			failed[0].Threshold)
	}
	if len(r.Checks) == 0 {
		return "No checks ran."
	}
	return fmt.Sprintf("Verified: %d checks passed, %d not checked.", len(r.Checks), len(r.Unchecked))
}

// Failed returns the proofs that did not pass, in the order they ran.
func (r Receipt) Failed() []Check {
	var failed []Check
	for _, c := range r.Checks {
		if !c.Pass {
			failed = append(failed, c)
		}
	}
	return failed
}
