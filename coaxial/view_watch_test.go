package main

import (
	"strings"
	"testing"
)

// clockChunk: three MPEG-TS packets on stream 0x100, each carrying the program clock at the given second.
func clockChunk(at float64) []byte {
	out := make([]byte, 0, 3*188)
	base := int64(at * 90000)
	for i := 0; i < 3; i++ {
		p := make([]byte, 188)
		p[0], p[1], p[2], p[3], p[4], p[5] = 0x47, 0x01, 0x00, 0x30|byte(i), 7, 0x10
		p[6], p[7], p[8], p[9], p[10] = byte(base>>25), byte(base>>17), byte(base>>9), byte(base>>1), byte(base&1)<<7
		out = append(out, p...)
	}
	return out
}

// A reader of live MPEG-TS is watched for its whole stay. Video that comes in lumps, as it does, with enough
// in hand says nothing; a wait that outlasts what the player has is a warning with the numbers; a send the
// player would not take is a warning; and the line at the end carries the lowest and the longest of the stay.
func TestViewWatchSaysWhenAPlayerRunsOut(t *testing.T) {
	log := captureLog(t)
	rd := &reader{who: "dev-a", id: "7", came: 1000}
	vw := &viewWatch{pid: -1}
	send := func(at, clock float64) { vw.see(clockChunk(clock), at, at+0.01, rd, "p") }

	send(1000.0, 0) // the opening lump: 10 s of video at once
	send(1000.1, 10)
	for i := 1; i <= 5; i++ { // then 6 s of video every 6 s
		send(1000+6*float64(i), 10+6*float64(i))
	}
	if got := log.String(); strings.Contains(got, "WRN") || vw.dry != 0 {
		t.Fatalf("video in lumps with 4 s always in hand: ran out %d time(s), log:\n%s", vw.dry, got)
	}
	if vw.low < 3.8 || vw.low > 4.2 {
		t.Errorf("lowest spare %.2f s, want about 4 (10 s at the start, 6 s waits)", vw.low)
	}

	send(1042, 46) // 12 s after the last lump, with 4 s in hand at the usual wait: out for 2 s
	lines := linesWith(log.String(), "WRN", "dev-a ran out of video on channel 7")
	if len(lines) != 1 || vw.dry != 1 {
		t.Fatalf("a 12 s wait with 4 s in hand: %d warning(s), ran out %d time(s), want 1 and 1; log:\n%s", len(lines), vw.dry, log.String())
	}
	if !strings.Contains(lines[0], "out_for_s=2") || !strings.Contains(lines[0], "waited_s=12") {
		t.Errorf("the warning should say out for 2 s after a wait of 12 s: %s", lines[0])
	}

	vw.see(clockChunk(52), 1048, 1051.5, rd, "p") // a send the player took 3.5 s to accept
	if n := len(linesWith(log.String(), "WRN", "dev-a did not take video for 3.5 s")); n != 1 {
		t.Errorf("a send that took 3.5 s: %d warning(s), want 1; log:\n%s", n, log.String())
	}

	vw.left(rd, "p", mono()-3, 2<<20)
	end := linesWith(log.String(), "when dev-a left channel 7")
	if len(end) != 1 {
		t.Fatalf("the line at the end: %d, want 1; log:\n%s", len(end), log.String())
	}
	for _, want := range []string{"provider_quiet_s=3", "waiting_mb=2", "lowest_spare_s=-2", "longest_wait_s=12", "slowest_send_s=3.5", "ran_out=1"} {
		if !strings.Contains(end[0], want) {
			t.Errorf("the line at the end lacks %s: %s", want, end[0])
		}
	}

	quiet := &viewWatch{pid: -1}
	quiet.left(rd, "p", 0, 0) // a reader that was sent nothing: no line
	if n := len(linesWith(log.String(), "when dev-a left channel 7")); n != 1 {
		t.Errorf("a reader that was sent no video got a line at the end")
	}
}
