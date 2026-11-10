package verify

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestReceiptSummary proves the summary names the first failed proof.
func TestReceiptSummary(t *testing.T) {
	t.Parallel()
	pass := Check{Name: CheckDecode, Pass: true}
	fail := Check{Name: CheckPicture, Measured: "SSIM 0.4", Threshold: "SSIM at least 0.8"}
	tests := []struct {
		In         Receipt
		WantResult string
		WantPassed bool
	}{{ // Test 0: Nothing ran.
		WantResult: "No checks ran.",
	}, { // Test 1: Every proof passed.
		In:         Receipt{Checks: []Check{pass, pass}, Unchecked: []Note{{Name: CheckColor}}},
		WantResult: "Verified: 2 checks passed, 1 not checked.", WantPassed: true,
	}, { // Test 2: A failed proof is named.
		In:         Receipt{Checks: []Check{pass, fail, {Name: CheckSound}}},
		WantResult: "Check failed: picture, measured SSIM 0.4, needs SSIM at least 0.8.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, test.In.Summary()); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
			if got := test.In.Passed(); got != test.WantPassed {
				t.Errorf("Passed() = %v, want %v", got, test.WantPassed)
			}
		})
	}
}

// fullExpect returns an Expect with every field set, so a field that does not survive JSON shows.
func fullExpect() Expect {
	return Expect{
		Streams: []string{"video:h264", "audio:aac", "subtitle:mov_text"}, VideoCopied: true,
		AudioCopied: true, LosslessVideo: true, LosslessAudio: true, Rate: 23.976, PixFmt: "yuv420p10le",
		Range: "tv", Scaled: true, CombProof: true, MinSSIM: 0.93, MinSDR: 12.5,
		AudioTracks: []int{1, 0}, Channels: 6, SampleRate: 48000,
		Tones:   [][]float64{{440, 880}, {1000}},
		Markers: []time.Duration{500 * time.Millisecond, 1500*time.Millisecond + 7},
		Caption: &Caption{
			At: 700 * time.Millisecond, Clear: 200 * time.Millisecond, X: 0.2, Y: 0.85, W: 0.6, H: 0.13,
		},
		NoTags: []string{"title", "artist"}, NoTrackLanguage: true, DropChapters: true,
		DropMetadata: true, Waive: map[string]string{CheckColor: "The source states none."},
	}
}

// TestExpectRoundTrip proves a plan survives a receipt's JSON field for field, durations included.
func TestExpectRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In       Expect
		WantKeys []string
	}{{ // Test 0: Every field set, including the caption, tones, markers, and waivers.
		In: fullExpect(),
		WantKeys: []string{
			"streams", "videoCopied", "audioCopied", "losslessVideo", "losslessAudio", "rate", "pixFmt",
			"range", "scaled", "combProof", "minSSIM", "minSDR", "audioTracks", "channels", "sampleRate",
			"tones", "markersNs", "caption", "noTags", "noTrackLanguage", "dropChapters", "dropMetadata",
			"waive",
		},
	}, { // Test 1: An empty plan writes no fields.
		In: Expect{},
	}, { // Test 2: A caption alone keeps its times as nanoseconds.
		In:       Expect{Caption: &Caption{At: time.Second, Clear: 1, X: 0.5, Y: 0.5, W: 0.1, H: 0.1}},
		WantKeys: []string{"caption"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(Receipt{Schema: SchemaVersion, Expect: test.In})
			if err != nil {
				t.Fatal(err)
			}
			var got Receipt
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(test.In, got.Expect, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Expect mismatch (-want +got):\n%s", diff)
			}
			if got.Schema != SchemaVersion {
				t.Errorf("Schema = %d, want %d", got.Schema, SchemaVersion)
			}
			var raw struct {
				Expect map[string]json.RawMessage `json:"expect"`
			}
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			var keys []string
			for key := range raw.Expect {
				keys = append(keys, key)
			}
			sortStrings := cmpopts.SortSlices(func(a, b string) bool { return a < b })
			if diff := cmp.Diff(test.WantKeys, keys, sortStrings, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("keys mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestExpectDurationsAsNanoseconds proves markers and caption times serialize as plain integers.
func TestExpectDurationsAsNanoseconds(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(Expect{
		Markers: []time.Duration{time.Second}, Caption: &Caption{At: 2 * time.Second, Clear: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"markersNs":[1000000000],"caption":{"atNs":2000000000,"clearNs":3}}`
	if diff := cmp.Diff(want, string(data)); diff != "" {
		t.Errorf("JSON mismatch (-want +got):\n%s", diff)
	}
}
