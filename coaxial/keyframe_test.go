package main

import "testing"

// kfPMT: a program table whose video, on PID 0x100, is of stream type typ.
func kfPMT(typ byte) []byte {
	return cutPkt(0x1000, true, nil, []byte{0, 0x02, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0, typ, 0xe1, 0x00, 0xf0, 0, 0, 0, 0, 0})
}

var kfPAT = cutPkt(0, true, nil, []byte{0, 0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0x00, 0, 0, 0, 0})

// kfFrame: a video frame on PID 0x100 with its PES header, as many packets as es takes, marked as a
// keyframe if mark.
func kfFrame(mark bool, es ...[]byte) []byte {
	b := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5, 0x21, 0, 1, 0, 1}
	for _, e := range es {
		b = append(b, e...)
	}
	var af []byte
	if mark {
		af = []byte{0x40}
	}
	var out []byte
	for start := true; len(b) > 0; start, af = false, nil {
		n := 184
		if af != nil {
			n -= 1 + len(af)
		}
		n = min(n, len(b))
		out = append(out, cutPkt(0x100, start, af, b[:n])...)
		b = b[n:]
	}
	return out
}

// nal: an Annex B unit: a start code, its header and some data.
func nal(header []byte, size int) []byte {
	b := append([]byte{0, 0, 0, 1}, header...)
	for len(b) < size {
		b = append(b, 0x5a)
	}
	return b
}

func TestKeyFoundUnmarked(t *testing.T) {
	aud, sps, pps := nal([]byte{0x09, 0xf0}, 6), nal([]byte{0x67, 0x64, 0x00, 0x28}, 30), nal([]byte{0x68, 0xee}, 8)
	// (the SEI pushes the picture into the frame's second packet; 0x21, not IDR, read as HEVC would be type 16)
	sei := nal([]byte{0x06, 0x05}, 200)
	idr, nonIDR := nal([]byte{0x65, 0x88}, 100), nal([]byte{0x21, 0x9a}, 100)
	hAUD, hVPS, hSPS, hPPS := nal([]byte{0x46, 0x01, 0x50}, 7), nal([]byte{0x40, 0x01}, 24), nal([]byte{0x42, 0x01}, 40), nal([]byte{0x44, 0x01}, 10)
	cra, idrRADL, trail := nal([]byte{0x2a, 0x01}, 100), nal([]byte{0x26, 0x01}, 100), nal([]byte{0x02, 0x01}, 100)
	join := func(parts ...[]byte) []byte {
		var b []byte
		for _, p := range parts {
			b = append(b, p...)
		}
		return b
	}
	head := func(typ byte) []byte { return join(kfPAT, kfPMT(typ)) }
	second := 3 * 188 // the second PAT, after the program tables and a one-packet frame
	for _, c := range []struct {
		name  string
		buf   []byte
		at    int
		found bool
	}{
		{"H.264 IDR, not marked, after SPS, PPS and SEI", join(head(0x1b), kfFrame(false, aud, nonIDR), kfPAT, kfFrame(false, aud, sps, pps, sei, idr)), second, true},
		{"H.264 frames that are not IDR", join(head(0x1b), kfFrame(false, aud, nonIDR), kfPAT, kfFrame(false, aud, sei, nonIDR), kfPAT), 0, false},
		{"H.264 IDR past the packets read", join(head(0x1b), kfFrame(false, aud, sei, sei, sei, idr), kfPAT), 0, false},
		{"HEVC CRA, not marked", join(head(0x24), kfFrame(false, hAUD, trail), kfPAT, kfFrame(false, hAUD, hVPS, hSPS, hPPS, cra)), second, true},
		{"HEVC IDR, not marked", join(head(0x24), kfFrame(false, hAUD, trail), kfPAT, kfFrame(false, hAUD, idrRADL)), second, true},
		{"HEVC frames that are not IRAP", join(head(0x24), kfFrame(false, hAUD, trail), kfPAT, kfFrame(false, hAUD, trail), kfPAT), 0, false},
		{"MPEG-2 video is not read for pictures", join(head(0x02), kfFrame(false, idr), kfPAT, kfFrame(false, idr), kfPAT), 0, false},
		{"a marked frame counts whatever its pictures", join(head(0x1b), kfFrame(true, aud, nonIDR), kfPAT, kfFrame(false, idr)), 0, true},
		{"a marked frame before an IDR is found first", join(head(0x1b), kfFrame(false, aud, nonIDR), kfPAT, kfFrame(true, aud, nonIDR), kfPAT, kfFrame(false, idr)), second, true},
	} {
		if at, found := keyFound(c.buf); at != c.at || found != c.found {
			t.Errorf("%s: keyFound %d %v, want %d %v", c.name, at, found, c.at, c.found)
		}
	}
}

// A stream asked for again that has no keyframe at all joins where its clock passed the old one, once
// keyWait seconds of it are in hand; before that it is waited for.
func TestSpliceWithoutKeyframes(t *testing.T) {
	// packets are 40 ms apart from 9 s on: packet 26 is the first past the clock
	const clock = 10.02
	if _, ok := spliceAt(splicePkts(0x100, 9, 300), 0x100, clock); ok { // up to about 21 s
		t.Errorf("joined with %.1f s past the old clock, want a wait for keyWait (%d s)", 21-clock, keyWait)
	}
	// (spliceAt reads the clock three packets at a time, so that is found from packet 24)
	if at, ok := spliceAt(splicePkts(0x100, 9, 400), 0x100, clock); !ok || at != 24*188 {
		t.Errorf("spliceAt %d %v, want %d true", at, ok, 24*188)
	}
}
