package main

import "testing"

// splicePkts: n packets of a stream whose clock (PCR) is on pcrPID, from clock seconds on, 40 ms apart.
func splicePkts(pcrPID int, clock float64, n int) []byte {
	var out []byte
	for k := 0; k < n; k++ {
		b := int64((clock + 0.04*float64(k)) * 90000)
		p := []byte{0x47, byte(pcrPID >> 8), byte(pcrPID), 0x30, 7, 0x10,
			byte(b >> 25), byte(b >> 17), byte(b >> 9), byte(b >> 1), byte(b&1)<<7 | 0x7e, 0}
		for len(p) < 188 {
			p = append(p, 0xff)
		}
		out = append(out, p...)
	}
	return out
}

// A stream asked for again whose clock moved to another PID is compared on that PID, not waited on for 48 MB.
func TestSpliceClockOnAnotherPID(t *testing.T) {
	for _, pid := range []int{0x100, 0x1ff} { // the first place its clock passes the old one (then a keyframe is waited for there)
		if at, ok := spliceAt(splicePkts(pid, 40, 200), 0x100, 45); ok || at != 124*188 {
			t.Errorf("clock on PID %#x: packet %d (%v), want packet 124, waiting for a keyframe", pid, at/188, ok)
		}
	}
	if at, ok := spliceAt(splicePkts(0x1ff, 1000, 500), 0x100, 45); !ok || at != 0 { // another clock entirely: at once
		t.Errorf("another clock on another PID: packet %d (%v), want 0 at once", at/188, ok)
	}
}

// A stream asked for again that marks no keyframes joins where its clock passed the old one, once it has gone
// keyWait seconds on without a mark: not after 48 MB.
func TestSpliceNoKeyframeMarks(t *testing.T) {
	if at, ok := spliceAt(splicePkts(0x100, 40, 300), 0x100, 45); !ok || at != 124*188 {
		t.Errorf("no keyframe marks, 7 s past the old clock: packet %d (%v), want packet 124 at once", at/188, ok)
	}
}
