package main

import "testing"

// framePkt: a video frame's first packet on PID 0x100 with its PTS, marked as a keyframe or not.
func framePkt(pts float64, key bool) []byte {
	p := int64(pts * 90000)
	pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5, byte(0x21 | (p>>29)&0x0e), byte(p >> 22), byte(p>>14) | 1, byte(p >> 7), byte(p<<1) | 1}
	af := []byte{0x00}
	if key {
		af[0] = 0x40
	}
	return cutPkt(0x100, true, af, pes)
}

// HLS built from MPEG-TS: a stream that does not begin at a keyframe (most provider streams begin wherever
// they are) gives a first piece that starts at its first keyframe, like every other piece, not at whatever
// frame came first.
func TestFirstPieceAtKeyframe(t *testing.T) {
	var c tsCutter
	pat := cutPkt(0, true, nil, []byte{0, 0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0x00, 0, 0, 0, 0})
	pmt := cutPkt(0x1000, true, nil, []byte{0, 0x02, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0, 0x1b, 0xe1, 0x00, 0xf0, 0, 0, 0, 0, 0})
	stream := append(append([]byte{}, pat...), pmt...)
	for k := 0; k <= 300; k++ { // 12 s at 25 frames a second, PTS from 1000 s; keyframes every 4 s from 1001 s
		pts := 1000 + 0.04*float64(k)
		stream = append(stream, framePkt(pts, k >= 25 && (k-25)%100 == 0)...)
	}
	var segs []*segment
	for i := 0; i < len(stream); i += 1000 {
		segs = append(segs, c.feed(stream[i:min(i+1000, len(stream))])...)
	}
	if len(segs) == 0 {
		t.Fatal("no piece")
	}
	first := segs[0].data[2*188:] // (each piece starts with the PAT and the PMT)
	if len(first) < 188 || first[3]&0x20 == 0 || first[5]&0x40 == 0 {
		t.Errorf("the first piece (%.2f s) starts at a frame that is not a keyframe", segs[0].dur)
	}
}
