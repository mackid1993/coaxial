package main

import "testing"

// A stream asked for again on another clock entirely (the provider started it afresh) joins at its first
// keyframe, with the program table before it: joined wherever it began, the players' decoders went on from
// the old stream's pictures and showed broken ones for up to a keyframe interval.
func TestSpliceAnotherClockAtKeyframe(t *testing.T) {
	pat := cutPkt(0, true, nil, []byte{0, 0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0x00, 0, 0, 0, 0})
	pmt := cutPkt(0x1000, true, nil, []byte{0, 0x02, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0, 0x1b, 0xe1, 0x00, 0xf0, 0, 0, 0, 0, 0})
	var stream []byte
	for k := 0; k < 150; k++ { // 6 s at 25 frames a second on a clock near 1000 s; the first keyframe 1.2 s in
		if k%10 == 0 {
			stream = append(append(stream, pat...), pmt...)
		}
		pts := int64((1000 + 0.04*float64(k)) * 90000)
		af := []byte{0x10, byte(pts >> 25), byte(pts >> 17), byte(pts >> 9), byte(pts >> 1), byte(pts&1)<<7 | 0x7e, 0}
		if k == 30 {
			af[0] |= 0x40
		}
		pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5, byte(0x21 | (pts>>29)&0x0e), byte(pts >> 22), byte(pts>>14) | 1, byte(pts >> 7), byte(pts<<1) | 1}
		stream = append(stream, cutPkt(0x100, true, af, pes)...)
	}
	if at, ok := spliceAt(stream, 0x100, 45); !ok || at != (30+2*3)*188 {
		t.Errorf("another clock: joins at packet %d (%v), want packet 36 (the program table before the keyframe)", at/188, ok)
	}
}
