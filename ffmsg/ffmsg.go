// Package ffmsg turns what ffmpeg prints when it fails into a line a person can read.
package ffmsg

import (
	"regexp"
	"strings"
)

// The lines known failures read as.
const (
	// Stopped is the line for an encoder that ended without saying why, as when something kills it.
	Stopped = "the encoder stopped unexpectedly"
	// Damaged is the line for a source that opened and then could not be read.
	Damaged = "the file is damaged and cannot be read to the end"
)

// known maps what ffmpeg prints to what it means, the first match winning.
var known = []struct { //nolint:gochecknoglobals // Read-only table.
	// Printed is lowercase text ffmpeg prints.
	Printed string
	// Means is the line to show instead.
	Means string
}{
	{"no space left", "the disk is full"},
	{"read-only file system", "the folder cannot be written to"},
	{"permission denied", "permission to the file or folder was denied"},
	{"operation not permitted", "permission to the file or folder was denied"},
	{"no such file", "the file was not found"},
	{"moov atom not found", "the file is damaged or is not a media file"},
	{"error opening input", "the file is damaged or is not a media file"},
	{"[dec:", Damaged},
	{"[in#", Damaged},
	{"invalid data found", "the file is damaged or is not a media file"},
}

// logPrefix matches the component and address ffmpeg puts before a line.
var logPrefix = regexp.MustCompile(`\[[^\]]* @ 0x[0-9a-f]+\] ?`)

// Plain returns a one-line reason for what ffmpeg printed: what it means when known, else its last
// line without the component prefix. Nothing printed gives Stopped.
func Plain(printed string) string {
	lower := strings.ToLower(printed)
	for _, k := range known {
		if strings.Contains(lower, k.Printed) {
			return k.Means
		}
	}
	lines := strings.Split(strings.TrimSpace(printed), "\n")
	last := strings.TrimSpace(logPrefix.ReplaceAllString(lines[len(lines)-1], ""))
	if last == "" {
		return Stopped
	}
	return last
}
