package main

import (
	"bytes"
	"testing"
)

// A provider asked for again usually sends the same stream from its buffer, seconds back. The stream carries
// on from exactly where the old one stopped: no video lost (joined at the next keyframe, up to a whole
// keyframe interval was), none repeated.
func TestResumeWhereItStopped(t *testing.T) {
	full := splicePkts(0x100, 38, 400)     // one stream, a clock on every packet; no keyframe marks
	for _, extra := range []int{0, 1, 5} { // packets the old stream sent after its last clock
		old := append([]byte{}, full[50*188:150*188]...)
		for k := 0; k < extra; k++ {
			old = append(old, 0x47, 0x01, 0x01, 0x10) // (a packet without a clock)
			old = append(old, make([]byte, 184)...)
		}
		_, clock, at, ok := lastPCR(old, -1)
		if !ok {
			t.Fatal("no clock in the old stream")
		}
		past := len(old) - at
		again := append([]byte{}, full[100*188:]...) // asked again: 2 s back
		for k := 0; k < extra; k++ {                 // (the same packets, in the same place)
			copy(again[(50+k)*188:], old[(100+k)*188:(101+k)*188])
		}
		if at, ok := resumeAt(again, clock, past, nil); !ok || at != (50+extra)*188 {
			t.Errorf("%d packets after the last clock: carries on at packet %d (%v), want %d", extra, at/188, ok, 50+extra)
		} else if !bytes.Equal(again[at:at+188], full[(150+extra)*188:(151+extra)*188]) {
			t.Errorf("%d packets after the last clock: not the packet after the old stream's last", extra)
		}
	}
	// numbered afresh (its clock on another PID): the same clock still says where it carries on
	_, clock, at, _ := lastPCR(full[:150*188], -1)
	again := append([]byte{}, full[100*188:]...)
	for i := 0; i < len(again); i += 188 {
		again[i+1], again[i+2] = 0x02, 0x00
	}
	if at, ok := resumeAt(again, clock, 150*188-at, nil); !ok || at != 50*188 {
		t.Errorf("numbered afresh: carries on at packet %d (%v), want 50", at/188, ok)
	}
	// another clock: not found here (spliceAt decides)
	if _, ok := resumeAt(splicePkts(0x100, 1000, 300), clock, 188, nil); ok {
		t.Error("a stream on another clock was taken to carry on")
	}
}

// clocked: n packets of a stream, a clock (PCR) on every tenth (40 ms apart from start s), each packet told
// apart by its number; padded (a null packet after every third) when pad.
func clocked(start float64, n int, pad bool) []byte {
	var out []byte
	for k := 0; k < n; k++ {
		p := splicePkts(0x100, start+0.04*float64(k/10), 1)
		if k%10 != 0 {
			p[3] = 0x10
			copy(p[4:], []byte{0, 0, 0, 0, 0, 0, 0, 0})
		}
		p[30], p[31] = byte(k>>8), byte(k)
		out = append(out, p...)
		if pad && k%3 == 2 {
			out = append(out, cutPkt(0x1fff, false, nil, nil)...)
		}
	}
	return out
}

// Another server may send the same stream on the same clock laid out otherwise: padded to a constant rate
// (or the padding taken out), its tables elsewhere. The stream asked for again carries on just after the
// last packet passed on, not past bytes on from the clock: no packet lost or repeated.
func TestResumeOtherLayout(t *testing.T) {
	unpad := func(b []byte) (out []byte) {
		for i := 0; i < len(b); i += 188 {
			if b[i+1]&0x1f != 0x1f || b[i+2] != 0xff {
				out = append(out, b[i:i+188]...)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		old, new bool // padded
	}{{"padding taken out", true, false}, {"padding put in", false, true}, {"the same layout", true, true}} {
		full, fresh := clocked(38, 600, tc.old), clocked(38, 600, tc.new)
		cut := len(full) / 2 / 188 * 188
		cut += 5 * 188 // (some packets after a clock)
		old := full[:cut]
		_, clock, at, _ := lastPCR(old, -1)
		last := old[len(old)-188:]
		if last[1]&0x1f == 0x1f {
			last = old[len(old)-376 : len(old)-188]
		}
		again := fresh[len(fresh)/4/188*188:] // asked again, seconds back
		from, ok := resumeAt(again, clock, len(old)-at, last)
		if !ok || from > len(again) {
			t.Fatalf("%s: where it stopped was not found (%d, %v)", tc.name, from, ok)
		}
		got, want := unpad(append(append([]byte{}, old...), again[from:]...)), unpad(clocked(38, 600, false))
		if !bytes.Equal(got, want) {
			n := 0
			for n < min(len(got), len(want)) && got[n] == want[n] {
				n++
			}
			t.Errorf("%s: %d packets handed on, want %d; they differ from packet %d", tc.name, len(got)/188, len(want)/188, n/188)
		}
	}
}
