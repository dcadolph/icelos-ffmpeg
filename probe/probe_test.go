package probe

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/icelos-ffmpeg/tooltest"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want     error
		In       string
		WantInfo Info
	}{{ // Test 0: Typical mp4 with video and audio.
		In: `{"format":{"format_name":"mov,mp4","duration":"12.5","size":"1000"},` +
			`"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},` +
			`{"index":1,"codec_type":"audio","codec_name":"aac","channels":2}]}`,
		WantInfo: Info{
			Format: "mov,mp4", Duration: 12500 * time.Millisecond, Size: 1000,
			Streams: []Stream{
				{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080},
				{Index: 1, Kind: "audio", Codec: "aac", Channels: 2},
			},
		},
	}, { // Test 1: Invalid JSON.
		In: `{`, Want: ErrProbe,
	}, { // Test 2: Bad duration.
		In: `{"format":{"duration":"abc"}}`, Want: ErrProbe,
	}, { // Test 3: Missing duration and size are zero.
		In: `{"format":{"format_name":"wav"}}`, WantInfo: Info{Format: "wav"},
	}, { // Test 4: Sample rate, frame rates, and B-frame delay.
		In: `{"streams":[{"index":0,"codec_type":"video","avg_frame_rate":"30000/1001",` +
			`"r_frame_rate":"30000/1001","has_b_frames":2},{"index":1,"codec_type":"audio",` +
			`"sample_rate":"44100","avg_frame_rate":"0/0","r_frame_rate":"0/0"}]}`,
		WantInfo: Info{Streams: []Stream{
			{Index: 0, Kind: "video", AvgFrameRate: "30000/1001", BaseFrameRate: "30000/1001", BFrames: 2},
			{Index: 1, Kind: "audio", SampleRate: 44100, AvgFrameRate: "0/0", BaseFrameRate: "0/0"},
		}},
	}, { // Test 5: A sample rate that is not a number.
		In: `{"streams":[{"index":0,"codec_type":"audio","sample_rate":"fast"}]}`, Want: ErrProbe,
	}, { // Test 6: Stream tags and chapters.
		In: `{"streams":[{"index":0,"codec_type":"audio","tags":{"language":"jpn","title":"Main",` +
			`"handler_name":"SoundHandler"}}],"chapters":[{"id":0,"time_base":"1/1000","start":0,` +
			`"start_time":"0.000000","end_time":"61.500000","tags":{"title":"Opening"}},` +
			`{"id":1,"start_time":"61.500000","end_time":"90.000000"}]}`,
		WantInfo: Info{
			Streams: []Stream{{Index: 0, Kind: "audio", Tags: Tags{Language: "jpn", Title: "Main"}}},
			Chapters: []Chapter{
				{Start: 0, End: 61500 * time.Millisecond, Title: "Opening"},
				{Start: 61500 * time.Millisecond, End: 90 * time.Second},
			},
		},
	}, { // Test 7: A chapter start that is not a number.
		In: `{"chapters":[{"start_time":"soon","end_time":"1.0"}]}`, Want: ErrProbe,
	}, { // Test 8: A chapter end that is not a number.
		In: `{"chapters":[{"start_time":"0.0","end_time":"later"}]}`, Want: ErrProbe,
	}, { // Test 9: Stream bitrates, durations, and the Matroska duration tag.
		In: `{"streams":[{"index":0,"codec_type":"video","bit_rate":"451019","duration":"2.125000"},` +
			`{"index":1,"codec_type":"audio","bit_rate":"640000","tags":{"DURATION":"00:00:02.028000000"}}]}`,
		WantInfo: Info{Streams: []Stream{
			{Index: 0, Kind: "video", Bitrate: 451_019, Duration: "2.125000"},
			{Index: 1, Kind: "audio", Bitrate: 640_000, Tags: Tags{Duration: "00:00:02.028000000"}},
		}},
	}, { // Test 10: A bitrate that is not a number.
		In: `{"streams":[{"index":0,"codec_type":"audio","bit_rate":"fast"}]}`, Want: ErrProbe,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := Parse([]byte(test.In))
			if !errors.Is(err, test.Want) {
				t.Fatalf("error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantInfo, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStreamHelpers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantVideo  Stream
		WantAudio  Stream
		WantWidth  int
		WantHeight int
		WantFound  [2]bool
	}{{ // Test 0: Portrait phone video stored landscape with a rotation.
		In: `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","width":1920,"height":1080,` +
			`"side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}]},` +
			`{"index":1,"codec_type":"audio","codec_name":"aac","channels":2}]}`,
		WantVideo: Stream{Index: 0, Kind: "video", Codec: "hevc", Width: 1920, Height: 1080,
			SideData: []SideData{{Rotation: -90}}},
		WantAudio: Stream{Index: 1, Kind: "audio", Codec: "aac", Channels: 2},
		WantWidth: 1080, WantHeight: 1920, WantFound: [2]bool{true, true},
	}, { // Test 1: MP3 with cover art has no motion video.
		In: `{"streams":[{"index":0,"codec_type":"audio","codec_name":"mp3","channels":2},` +
			`{"index":1,"codec_type":"video","codec_name":"mjpeg","width":600,"height":600,` +
			`"disposition":{"attached_pic":1}}]}`,
		WantAudio: Stream{Index: 0, Kind: "audio", Codec: "mp3", Channels: 2},
		WantFound: [2]bool{false, true},
	}, { // Test 2: Half-turn rotation keeps the size.
		In: `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":640,"height":480,` +
			`"side_data_list":[{"rotation":180}]}]}`,
		WantVideo: Stream{Index: 0, Kind: "video", Codec: "h264", Width: 640, Height: 480,
			SideData: []SideData{{Rotation: 180}}},
		WantWidth: 640, WantHeight: 480, WantFound: [2]bool{true, false},
	}, { // Test 3: No streams.
		In: `{}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			info, err := Parse([]byte(test.In))
			if err != nil {
				t.Fatal(err)
			}
			v, vok := info.FirstVideo()
			a, aok := info.FirstAudio()
			if diff := cmp.Diff(test.WantFound, [2]bool{vok, aok}); diff != "" {
				t.Errorf("found mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantVideo, v); diff != "" {
				t.Errorf("video mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantAudio, a); diff != "" {
				t.Errorf("audio mismatch (-want +got):\n%s", diff)
			}
			w, h := v.DisplaySize()
			if diff := cmp.Diff([2]int{test.WantWidth, test.WantHeight}, [2]int{w, h}); diff != "" {
				t.Errorf("size mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSampleDepth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   Stream
		Want int
	}{{ // Test 0: Sixteen bit samples.
		In: Stream{SampleFmt: "s16"}, Want: 16,
	}, { // Test 1: Planar eight bit samples.
		In: Stream{SampleFmt: "u8p"}, Want: 8,
	}, { // Test 2: Thirty two bit containers holding 24 bits say so.
		In: Stream{SampleFmt: "s32", RawBits: 24}, Want: 24,
	}, { // Test 3: Thirty two bit containers that do not say hold 32.
		In: Stream{SampleFmt: "s32p"}, Want: 32,
	}, { // Test 4: Floating point samples have no whole depth.
		In: Stream{SampleFmt: "fltp"}, Want: 0,
	}, { // Test 5: A stream that does not say.
		In: Stream{}, Want: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.In.SampleDepth(); got != test.Want {
				t.Errorf("SampleDepth() = %d, want %d", got, test.Want)
			}
		})
	}
}

func TestTag(t *testing.T) {
	t.Parallel()
	ogg := Info{Format: "ogg", Streams: []Stream{{Tags: Tags{}}, {Tags: Tags{Title: "T", Artist: "A"}}}}
	tests := []struct {
		In   Info
		Name string
		Want string
	}{{ // Test 0: A container tag.
		In: Info{Format: "matroska,webm", Tags: map[string]string{"title": "T"}}, Name: "title", Want: "T",
	}, { // Test 1: A track's title outside Ogg is the track's own.
		In:   Info{Format: "matroska,webm", Streams: []Stream{{Tags: Tags{Title: "Track"}}}},
		Name: "title", Want: "",
	}, { // Test 2: Ogg keeps the title on a stream.
		In: ogg, Name: "title", Want: "T",
	}, { // Test 3: Ogg keeps the artist on a stream.
		In: ogg, Name: "artist", Want: "A",
	}, { // Test 4: Ogg with neither.
		In: Info{Format: "ogg", Streams: []Stream{{}}}, Name: "artist", Want: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.In.Tag(test.Name); got != test.Want {
				t.Errorf("Tag(%q) = %q, want %q", test.Name, got, test.Want)
			}
		})
	}
}

func TestMediaDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In           string
		WantDuration time.Duration
	}{{ // Test 0: MP4 streams carry durations and the longest wins.
		In: `{"format":{"duration":"2.015333"},"streams":[{"codec_type":"video","duration":"2.000000"},` +
			`{"codec_type":"audio","duration":"2.015333"}]}`,
		WantDuration: 2015333 * time.Microsecond,
	}, { // Test 1: The chaptered mkv, whose subtitles stretch the container to 4.05 s.
		In: `{"format":{"duration":"4.050000"},"streams":[` +
			`{"codec_type":"video","tags":{"DURATION":"00:00:02.000000000"}},` +
			`{"codec_type":"audio","tags":{"DURATION":"00:00:02.028000000"}},` +
			`{"codec_type":"subtitle","tags":{"DURATION":"00:00:04.050000000"}}]}`,
		WantDuration: 2028 * time.Millisecond,
	}, { // Test 2: No stream records a duration, so the container's stands.
		In:           `{"format":{"duration":"12.5"},"streams":[{"codec_type":"video"},{"codec_type":"audio"}]}`,
		WantDuration: 12500 * time.Millisecond,
	}, { // Test 3: Cover art does not count as playing video.
		In: `{"format":{"duration":"3.0"},"streams":[{"codec_type":"audio","duration":"3.000000"},` +
			`{"codec_type":"video","duration":"90.0","disposition":{"attached_pic":1}}]}`,
		WantDuration: 3 * time.Second,
	}, { // Test 4: Durations that do not parse fall back to the container.
		In: `{"format":{"duration":"5.0"},"streams":[{"codec_type":"video","duration":"soon"},` +
			`{"codec_type":"audio","tags":{"DURATION":"2.5"}},{"codec_type":"audio","tags":{"DURATION":"x:00:01"}},` +
			`{"codec_type":"audio","tags":{"DURATION":"00:-1:01"}}]}`,
		WantDuration: 5 * time.Second,
	}, { // Test 5: Hours and minutes in a duration tag.
		In:           `{"streams":[{"codec_type":"audio","tags":{"DURATION":"01:02:03.500000000"}}]}`,
		WantDuration: time.Hour + 2*time.Minute + 3500*time.Millisecond,
	}, { // Test 6: Nothing records a duration.
		In: `{}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			info, err := Parse([]byte(test.In))
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(test.WantDuration, info.MediaDuration()); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestKind(t *testing.T) {
	t.Parallel()
	info := Info{Streams: []Stream{
		{Index: 0, Kind: "video"}, {Index: 1, Kind: "audio"}, {Index: 2, Kind: "subtitle"},
		{Index: 3, Kind: "audio"},
	}}
	tests := []struct {
		Kind      string
		WantIndex []int
	}{
		{Kind: "audio", WantIndex: []int{1, 3}}, // Test 0: Audio in file order.
		{Kind: "subtitle", WantIndex: []int{2}}, // Test 1: One subtitle.
		{Kind: "data", WantIndex: nil},          // Test 2: None of the kind.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got []int
			for _, s := range info.Kind(test.Kind) {
				got = append(got, s.Index)
			}
			if diff := cmp.Diff(test.WantIndex, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFrameRate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In      Stream
		WantFPS float64
	}{
		{In: Stream{AvgFrameRate: "30000/1001"}, WantFPS: 30000.0 / 1001},               // Test 0: NTSC.
		{In: Stream{AvgFrameRate: "0/0", BaseFrameRate: "25/1"}, WantFPS: 25},           // Test 1: Base rate fallback.
		{In: Stream{AvgFrameRate: "24"}, WantFPS: 24},                                   // Test 2: Plain number.
		{In: Stream{AvgFrameRate: "30/0"}, WantFPS: 0},                                  // Test 3: Zero denominator.
		{In: Stream{AvgFrameRate: "x/1", BaseFrameRate: "-5/1"}, WantFPS: 0},            // Test 4: Garbage.
		{In: Stream{}, WantFPS: 0},                                                      // Test 5: Unknown.
		{In: Stream{AvgFrameRate: "60000/1001", BaseFrameRate: "60/1"}, WantFPS: 59.94}, // Test 6: Average wins.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.In.FrameRate(); math.Abs(got-test.WantFPS) > 0.01 {
				t.Errorf("fps = %g, want %g", got, test.WantFPS)
			}
		})
	}
}

func TestRunMissingFile(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffprobe")
	_, err := File(t.Context(), filepath.Join(t.TempDir(), "missing.mp4"))
	if !errors.Is(err, ErrProbe) {
		t.Errorf("error = %v, want %v", err, ErrProbe)
	}
}

func TestRunChaptersAndTags(t *testing.T) {
	t.Parallel()
	tooltest.Require(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	meta := filepath.Join(dir, "chapters.txt")
	chapters := ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1000\ntitle=One\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=1000\nEND=2000\ntitle=Two\n"
	if err := os.WriteFile(meta, []byte(chapters), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "in.mkv")
	out, err := exec.CommandContext(t.Context(), "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=d=2", "-i", meta, "-map", "0:a", "-map_chapters", "1",
		"-metadata:s:a:0", "language=fra", "-metadata:s:a:0", "title=Commentary", src).CombinedOutput()
	if err != nil {
		t.Fatalf("make fixture: %v: %s", err, out)
	}
	info, err := File(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	want := []Chapter{{Start: 0, End: time.Second, Title: "One"},
		{Start: time.Second, End: 2 * time.Second, Title: "Two"}}
	if diff := cmp.Diff(want, info.Chapters); diff != "" {
		t.Errorf("chapters mismatch (-want +got):\n%s", diff)
	}
	a, _ := info.FirstAudio()
	if diff := cmp.Diff(Tags{Language: "fra", Title: "Commentary"}, a.Tags,
		cmpopts.IgnoreFields(Tags{}, "Duration")); diff != "" {
		t.Errorf("tags mismatch (-want +got):\n%s", diff)
	}
	if d := info.MediaDuration(); a.Tags.Duration == "" || d < 1950*time.Millisecond || d > 2050*time.Millisecond {
		t.Errorf("media duration = %s from tag %q, want about 2s", d, a.Tags.Duration)
	}
}

// TestRunHidden proves a file from the MP4 family is read twice to find what its edit lists hide,
// and that any other file is read once.
func TestRunHidden(t *testing.T) {
	t.Parallel()
	doc := func(format, duration string) string {
		return `{"format":{"format_name":"` + format + `"},"streams":[{"index":0,"codec_type":"video",` +
			`"duration":"` + duration + `"}]}`
	}
	tests := []struct {
		Name       string
		Normal     string
		Raw        string
		WantHidden time.Duration
		WantError  error
	}{{ // Test 0: A list that hides footage.
		Name: "hidden", Normal: doc("mov,mp4", "2.0"), Raw: doc("mov,mp4", "4.5"),
		WantHidden: 2500 * time.Millisecond,
	}, { // Test 1: A list that hides nothing.
		Name: "same", Normal: doc("mov,mp4", "2.0"), Raw: doc("mov,mp4", "2.0"),
	}, { // Test 2: A list that plays longer than its media hides nothing.
		Name: "longer", Normal: doc("mov,mp4", "2.0"), Raw: doc("mov,mp4", "1.0"),
	}, { // Test 3: Another container is read once, so a second answer cannot count.
		Name: "matroska", Normal: doc("matroska,webm", "2.0"), Raw: doc("matroska,webm", "9.0"),
	}, { // Test 4: A second read that fails fails the probe.
		Name: "second read fails", Normal: doc("mov,mp4", "2.0"), WantError: ErrProbe,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			normal, raw := filepath.Join(dir, "normal.json"), filepath.Join(dir, "raw.json")
			for path, text := range map[string]string{normal: test.Normal, raw: test.Raw} {
				if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			second := "cat " + raw
			if test.Raw == "" {
				second = "echo 'cannot read' >&2; exit 1"
			}
			fake := filepath.Join(dir, "ffprobe")
			tooltest.WriteScript(t, fake, "#!/bin/sh\ncase \"$*\" in\n*ignore_editlist*) "+second+
				";;\n*) cat "+normal+";;\nesac\n")
			info, err := Run(t.Context(), fake, "in")
			if !errors.Is(err, test.WantError) {
				t.Fatalf("Run() = %v, want %v", err, test.WantError)
			}
			if test.WantError != nil {
				return
			}
			if got := info.Streams[0].Hidden; got != test.WantHidden {
				t.Errorf("Hidden = %s, want %s", got, test.WantHidden)
			}
		})
	}
}
