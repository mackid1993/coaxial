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
