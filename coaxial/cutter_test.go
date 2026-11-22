package main

import "testing"

// cutPkt: one MPEG-TS packet, its payload padded out with 0xff (adaptation field af, if any).
func cutPkt(pid int, start bool, af, payload []byte) []byte {
	p := []byte{0x47, byte(pid >> 8 & 0x1f), byte(pid), 0x10}
	if start {
		p[1] |= 0x40
	}
	if af != nil {
		p[3] = 0x30
		p = append(p, byte(len(af)))
		p = append(p, af...)
	}
	p = append(p, payload...)
	for len(p) < 188 {
		p = append(p, 0xff)
	}
	return p[:188]
}

// A piece that no frame ends (the video stopped, the stream went on with padding) does not grow past segBytes.
func TestCutterPieceBounded(t *testing.T) {
	var c tsCutter
	pat := cutPkt(0, true, nil, []byte{0, 0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0x00, 0, 0, 0, 0})
	pmt := cutPkt(0x1000, true, nil, []byte{0, 0x02, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0, 0x1b, 0xe1, 0x00, 0xf0, 0, 0, 0, 0, 0})
	pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5, 0x21, 0x00, 0x37, 0x77, 0x41} // PTS 10 s
	frame := cutPkt(0x100, true, []byte{0x40}, pes)                                 // a keyframe
	c.feed(append(append(pat, pmt...), frame...))
	null := make([]byte, 0, 188*1000)
	for i := 0; i < 1000; i++ {
		null = append(null, cutPkt(0x1fff, false, nil, nil)...)
	}
	for i := 0; i < 260; i++ { // about 49 MB of padding and no frame
		c.feed(null)
		if len(c.cur) > segBytes {
			t.Fatalf("the piece being built holds %d MB", len(c.cur)>>20)
		}
	}
}
