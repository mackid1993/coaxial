package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

// served: answers from memory, by path (404 for others).
type served map[string][]byte

func (s served) RoundTrip(r *http.Request) (*http.Response, error) {
	b, ok := s[r.URL.Path]
	status := 200
	if !ok {
		status = 404
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(b)), Request: r}, nil
}

// HLS pieces after a discontinuity may come numbered afresh (another packager, an advert). Joined end to end
// into one MPEG-TS stream they are handed on under the numbers readers were given first, as a TS stream
// asked for again is: recorders built on ffmpeg took the new numbers for new streams and recorded nothing
// more until the numbers came back.
func TestHLSPieceNumberedAfresh(t *testing.T) {
	a := progPkts(0x1000, 0x100, [2]int{0x1b, 0x100}, [2]int{0x0f, 0x101})
	b := progPkts(0x1100, 0x200, [2]int{0x1b, 0x200}, [2]int{0x0f, 0x201})
	list := "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1,\n0.ts\n#EXTINF:1,\n1.ts\n" +
		"#EXTINF:1,\n2.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:1,\n3.ts\n#EXTINF:1,\n4.ts\n#EXT-X-ENDLIST\n"
	src := hlsSource("http://hls.test/p.m3u8", "", served{"/p.m3u8": []byte(list), "/0.ts": a, "/1.ts": a, "/2.ts": a, "/3.ts": b, "/4.ts": b})
	var out []byte
	if err := src(context.Background(), &channel{}, func(d []byte) bool { out = append(out, d...); return true }); err != nil {
		t.Fatal(err)
	}
	if len(out) != len(a)+2*len(b) {
		t.Fatalf("%d bytes handed on, want pieces 2 to 4 (%d)", len(out), len(a)+2*len(b))
	}
	pids := map[int]int{}
	for i := 0; i < len(out); i += 188 {
		pids[int(out[i+1]&0x1f)<<8|int(out[i+2])]++
	}
	for pid, n := range pids {
		if pid != 0 && pid != 0x1000 && pid != 0x100 && pid != 0x101 {
			t.Errorf("%d packets on PID %#x, not one of the readers' numbers", n, pid)
		}
	}
	if got, ok := programOf(out[len(a):]); !ok || got.pmt != 0x1000 || got.es[0] != [2]int{0x1b, 0x100} {
		t.Errorf("after the discontinuity readers see %+v", got)
	}
}

// tablePkt: a PAT (pid 0) or PMT packet with a good CRC.
func tablePkt(pid int, section []byte) []byte {
	c := crcMPEG(section)
	return cutPkt(pid, true, nil, append(append([]byte{0}, section...), byte(c>>24), byte(c>>16), byte(c>>8), byte(c)))
}

// progPkts: a stream whose program table is on pmt and lists es (type, PID), its clock on pcr (on a PID of
// its own: clock-only packets there), a few packets of each stream.
func progPkts(pmt, pcr int, es ...[2]int) []byte {
	pat := tablePkt(0, []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xe0 | byte(pmt>>8), byte(pmt)})
	sec := []byte{0x02, 0xb0, 0, 0, 1, 0xc1, 0, 0, 0xe0 | byte(pcr>>8), byte(pcr), 0xf0, 0}
	for _, e := range es {
		sec = append(sec, byte(e[0]), 0xe0|byte(e[1]>>8), byte(e[1]), 0xf0, 0)
	}
	sec[2] = byte(len(sec) - 3 + 4)
	out := append(append([]byte{}, pat...), tablePkt(pmt, sec)...)
	for k := 0; k < 20; k++ {
		if pcr != es[0][1] { // a clock-only packet (an adaptation field and no payload)
			p := splicePkts(pcr, 40+0.04*float64(k), 1)
			p[3], p[4] = 0x20, 183
			out = append(out, p...)
		}
		for _, e := range es {
			out = append(out, cutPkt(e[1], k == 0, nil, []byte{0, 0, 1, 0xe0})...)
		}
	}
	return out
}
