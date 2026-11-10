package ffmsg

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestPlain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantResult string
	}{{ // Test 0: A cut or empty file.
		In:         "[mov,mp4,m4a,3gp,3g2,mj2 @ 0x712c18000] moov atom not found\n/x/a.mp4: Invalid data found when processing input",
		WantResult: "the file is damaged or is not a media file",
	}, { // Test 1: A folder that refuses the write.
		In:         "[out#0/mp4 @ 0x796c143c0] Error opening output /x/.a.mp4: Permission denied\nError opening output files: Permission denied",
		WantResult: "permission to the file or folder was denied",
	}, { // Test 2: A full disk, in any case.
		In: "[matroska @ 0x1] Error writing: No space left on device", WantResult: "the disk is full",
	}, { // Test 3: A file that is gone.
		In: "/x/a.mp4: No such file or directory", WantResult: "the file was not found",
	}, { // Test 4: Something not in the table keeps its last line without the prefix.
		In: "first line\n[libx264 @ 0xabc123] width not divisible by 2", WantResult: "width not divisible by 2",
	}, { // Test 5: Nothing printed.
		In: "  \n", WantResult: Stopped,
	}, { // Test 6: A bare prefix says nothing either.
		In: "[libx264 @ 0xabc123] ", WantResult: Stopped,
	}, { // Test 7: A read-only volume.
		In: "/x/a.mp4: Read-only file system", WantResult: "the folder cannot be written to",
	}, { // Test 8: A decoder stopped on a fault in the source.
		In:         "[h264 @ 0x1] Invalid NAL unit size\n[vist#0:0/h264 @ 0x2] [dec:h264 @ 0x3] Decoding error: Invalid data found when processing input",
		WantResult: Damaged,
	}, { // Test 9: The demuxer stopped partway.
		In: "[in#0/mov,mp4,m4a,3gp,3g2,mj2 @ 0x1] stream 1, offset 0x37164: partial file", WantResult: Damaged,
	}, { // Test 10: An input that never opened is not a file that ended early.
		In:         "[in#0 @ 0x1] Error opening input: Invalid data found when processing input",
		WantResult: "the file is damaged or is not a media file",
	}, { // Test 11: A full disk outranks whatever else was logged.
		In: "[in#0/mp4 @ 0x1] partial file\n[out#0/mp4 @ 0x2] Error: No space left on device", WantResult: "the disk is full",
	}, { // Test 12: An encoder failure stays an encoder failure.
		In: "[vost#0:0/libx264 @ 0x1] [enc:libx264 @ 0x2] Error submitting video frame", WantResult: "Error submitting video frame",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, Plain(test.In)); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
