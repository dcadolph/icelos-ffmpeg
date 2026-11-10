package verify

import (
	"fmt"
	"slices"
)

// Headroom kept below the lowest measured score, so encoder jitter passes and a real regression
// fails.
const (
	// PictureHeadroom is how far an SSIM floor sits below the lowest measured score.
	PictureHeadroom = 0.01
	// SoundHeadroom is how many decibels an SDR floor sits below the lowest measured score.
	SoundHeadroom = 3
)

// RealPictureFloor is the lowest SSIM a receipt accepts on footage it knows nothing about. It catches
// a wrong or garbled picture and does not grade the encode, which the generated table cannot do on
// real footage. On the corpus the lowest real encode reads 0.948 and a different film reads 0.37 to
// 0.78.
const RealPictureFloor = 0.80

// Floor is the lowest score one codec at one level may reach, measured rather than chosen. The
// calibration test writes the table: it runs every generated source through every preset and every
// level on every encoder, and the floor is the lowest score less the headroom. A floor never
// lowers without a ledger row.
type Floor struct {
	// Codec is the catalog codec ID.
	Codec string
	// Level is the quality level or bitrate the row covers.
	Level string
	// Floor is the lowest score an output may reach.
	Floor float64
	// Software is the lowest score the software encoder measured, zero when it has not run.
	Software float64
	// Hardware is the lowest score a hardware encoder measured, zero when it has not run.
	Hardware float64
	// Worst names the run that measured the lowest score.
	Worst string
}

// lookup returns the floor of codec at level in a table.
func lookup(table []Floor, codec, level string) (float64, bool) {
	i := slices.IndexFunc(table, func(f Floor) bool { return f.Codec == codec && f.Level == level })
	if i < 0 {
		return 0, false
	}
	return table[i].Floor, true
}

// PictureFloor returns the lowest SSIM an encode of codec at level may reach.
func PictureFloor(codec, level string) (float64, bool) {
	return lookup(pictureFloors, codec, level)
}

// SoundFloor returns the lowest SDR in decibels an encode of codec at level may reach.
func SoundFloor(codec, level string) (float64, bool) {
	return lookup(soundFloors, codec, level)
}

// PictureFloors returns a copy of the picture table.
func PictureFloors() []Floor {
	return slices.Clone(pictureFloors)
}

// SoundFloors returns a copy of the sound table.
func SoundFloors() []Floor {
	return slices.Clone(soundFloors)
}

// SoundLevel names a sound floor's row: the bitrate, marked when the output holds more channels
// than stereo or fewer than the source, since the same bitrate then spreads over more channels or
// more sound.
func SoundLevel(bitrate, sourceChannels, outputChannels int) string {
	level := "default"
	if bitrate > 0 {
		level = fmt.Sprintf("%gk", float64(bitrate)/1000)
	}
	switch {
	case outputChannels < sourceChannels:
		return level + " downmix"
	case outputChannels > 2:
		return level + " surround"
	}
	return level
}
