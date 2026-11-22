package main

import "testing"

// A stream asked for again, numbered afresh (its clock on another PID), that begins just past the old
// stream's last clock with a keyframe (a provider resending from the keyframe where it stopped): it joins at
// that keyframe, not at the next one a whole keyframe interval later.
func TestSpliceRenumberedAtFirstKeyframe(t *testing.T) {
	key := func(clock float64) []byte { // a keyframe's first packet on 0x200, carrying the clock
		p := splicePkts(0x200, clock, 1)
		p[1] |= 0x40
		p[5] |= 0x40
		return p
	}
	gop := func(clock float64) []byte {
		out := tablePkt(0, []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf1, 0x00})
		out = append(out, tablePkt(0x1100, []byte{0x02, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe2, 0x00, 0xf0, 0, 0x1b, 0xe2, 0x00, 0xf0, 0})...)
		out = append(out, key(clock)...)
		return append(out, splicePkts(0x200, clock+0.04, 49)...)
	}
	again := append(gop(40.04), gop(42.04)...) // the old stream's last clock: 40 s, on 0x100
	if at, ok := spliceAt(again, 0x100, 40); !ok || at != 0 {
		t.Errorf("joined at packet %d (%v), want 0, the keyframe it begins with", at/188, ok)
	}
}
