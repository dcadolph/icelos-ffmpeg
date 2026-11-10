package verify

import (
	"fmt"
	"testing"
)

// TestSoundLevel covers how a bitrate and channel counts name a floor row.
func TestSoundLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Bitrate   int
		Source    int
		Output    int
		WantLevel string
	}{{ // Test 0: Stereo at a bitrate.
		Bitrate: 128_000, Source: 2, Output: 2, WantLevel: "128k",
	}, { // Test 1: Surround kept.
		Bitrate: 192_000, Source: 6, Output: 6, WantLevel: "192k surround",
	}, { // Test 2: Surround downmixed.
		Bitrate: 96_000, Source: 6, Output: 2, WantLevel: "96k downmix",
	}, { // Test 3: A fractional bitrate.
		Bitrate: 12_200, Source: 1, Output: 1, WantLevel: "12.2k",
	}, { // Test 4: No bitrate setting.
		Source: 1, Output: 1, WantLevel: "default",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := SoundLevel(test.Bitrate, test.Source, test.Output); got != test.WantLevel {
				t.Errorf("SoundLevel() = %q, want %q", got, test.WantLevel)
			}
		})
	}
}

// TestFloorTables holds every row in its range, since a floor outside it would pass or fail
// everything. A score is lowered by headroom from what ran, so each row must have run.
func TestFloorTables(t *testing.T) {
	t.Parallel()
	for _, row := range PictureFloors() {
		if row.Floor <= 0 || row.Floor >= 1 {
			t.Errorf("%s/%s: SSIM floor %v outside its range", row.Codec, row.Level, row.Floor)
		}
		if row.Software == 0 && row.Hardware == 0 {
			t.Errorf("%s/%s: nothing measured", row.Codec, row.Level)
		}
	}
	for _, row := range SoundFloors() {
		if row.Floor <= 0 {
			t.Errorf("%s/%s: SDR floor %v is no floor", row.Codec, row.Level, row.Floor)
		}
		if row.Software == 0 {
			t.Errorf("%s/%s: nothing measured", row.Codec, row.Level)
		}
	}
	if _, ok := PictureFloor("not-a-codec", "high"); ok {
		t.Error("found a floor for an unknown codec")
	}
	if _, ok := SoundFloor("not-a-codec", "128k"); ok {
		t.Error("found a floor for an unknown codec")
	}
}
