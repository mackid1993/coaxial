package main

import (
	"bytes"
	"io"
	"net/url"
	"strings"
	"testing"
)

// seedTS: a small valid stream (PAT, PMT, a keyframe with PCR and PTS, more frames, padding) to start from.
func seedTS() []byte {
	pat := cutPkt(0, true, nil, []byte{0, 0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0x00, 0, 0, 0, 0})
	pmt := cutPkt(0x1000, true, nil, []byte{0, 0x02, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0, 0x1b, 0xe1, 0x00, 0xf0, 0, 0, 0, 0, 0})
	var out []byte
	for k := 0; k < 12; k++ {
		pts := int64(10+k) * 90000
		pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5, byte(0x21 | (pts>>29)&0x0e), byte(pts >> 22), byte(pts>>14) | 1, byte(pts >> 7), byte(pts<<1) | 1}
		b := pts
		af := []byte{0x50, byte(b >> 25), byte(b >> 17), byte(b >> 9), byte(b >> 1), byte(b&1)<<7 | 0x7e, 0}
		if k%3 != 0 {
			af[0] = 0x10
		}
		out = append(out, pat...)
		out = append(out, pmt...)
		out = append(out, cutPkt(0x100, true, af, pes)...)
		out = append(out, cutPkt(0x100, false, nil, nil)...)
		out = append(out, cutPkt(0x1fff, false, nil, nil)...)
	}
	return out
}

func FuzzTSReaders(f *testing.F) {
	f.Add(seedTS(), 0x100, 12.5)
	f.Add(seedTS()[5:], -1, -1.0)
	f.Add(append([]byte{0x47, 0x47}, seedTS()...), 0x1fff, 1e9)
	f.Fuzz(func(t *testing.T, b []byte, pid int, clock float64) {
		if at := cleanStart(b); at < 0 || at > len(b) {
			t.Fatalf("cleanStart %d of %d", at, len(b))
		}
		if at, _ := keyFound(b); at < 0 || at > len(b) {
			t.Fatalf("keyFound %d of %d", at, len(b))
		}
		lastPCR(b, pid&0x1fff)
		lastPCR(b, -1)
		if at, _ := spliceAt(b, pid&0x1fff, clock); at < 0 || at > len(b) {
			t.Fatalf("spliceAt %d of %d", at, len(b))
		}
		patPMT(b)
		pickStream(b)
		pesPTS(b)
		tableStart(b)
	})
}

func FuzzCutter(f *testing.F) {
	f.Add(seedTS(), uint16(1000))
	f.Add(seedTS(), uint16(7))
	f.Fuzz(func(t *testing.T, b []byte, step uint16) {
		var c tsCutter
		n := int(step%4000) + 1
		for i := 0; i < len(b); i += n {
			for _, s := range c.feed(b[i:min(i+n, len(b))]) {
				if s.dur <= 0 || len(s.data) == 0 || len(s.data)%188 != 0 {
					t.Fatalf("piece of %.3f s, %d bytes", s.dur, len(s.data))
				}
			}
			if len(c.buf) > 188+376 || len(c.cur) > segBytes+188 {
				t.Fatalf("held %d bytes unparsed, %d in the piece", len(c.buf), len(c.cur))
			}
		}
		if s := c.flush(); s != nil && (s.dur <= 0 || len(s.data)%188 != 0) {
			t.Fatalf("last piece of %.3f s, %d bytes", s.dur, len(s.data))
		}
	})
}

// smallReads hands out what it holds a few bytes at a time.
type smallReads struct {
	b    []byte
	step int
}

func (s *smallReads) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), s.step, len(s.b))
	copy(p, s.b[:n])
	s.b = s.b[n:]
	return n, nil
}

func FuzzAPIStream(f *testing.F) {
	f.Add([]byte(`[{"stream_icon":"http:\/\/logo.example\/a.png","u":"https://x.example/b"}]`), uint8(3))
	f.Add([]byte(`"http://"https://"http:\/\/`), uint8(1))
	f.Add([]byte(`{"user_info":{"username":"u","password":"realPassw0rd","auth":1},"x":"http://a/b"}`), uint8(7))
	f.Fuzz(func(t *testing.T, b []byte, step uint8) {
		var whole, parts bytes.Buffer
		link := []byte(`"http://me:1/__cdn/`)
		login := [][2]string{{`"password"\s{0,4}:\s{0,4}"realPassw0rd"`, `"password":"easy"`}}
		if err := apiStream(&whole, bytes.NewReader(b), link, login, false); err != nil {
			t.Fatal(err)
		}
		if err := apiStream(&parts, &smallReads{b, int(step%40) + 1}, link, login, false); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(whole.Bytes(), parts.Bytes()) {
			t.Fatalf("read in pieces of %d: %q, read whole: %q", int(step%40)+1, parts.Bytes(), whole.Bytes())
		}
	})
}

func FuzzPlaylist(f *testing.F) {
	f.Add("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:7\n#EXTINF:4,\na.ts\n#EXTINF:4.5,\nhttp://x.example/b.ts?t=1\n", "http://provider.example:8080/live/u/p/1.m3u8")
	f.Add("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=10\nlow.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=99\n//h/high.m3u8\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n", "https://cdn.example/a/")
	f.Add("#EXTM3U\r\n#EXTINF:,\r\nchunk_23:00:00.ts\r\n#EXT-X-MAP:URI=\"/live/realU/realP/i.mp4\"\r\n", "http://provider.example:8080/x")
	fr := newFront(provider{Name: "P", URL: "http://provider.example:8080", User: "realU", Password: "realP", Port: "18502", LiveTV: "hls"},
		player{User: "tv", Password: "easy"})
	f.Fuzz(func(t *testing.T, text, base string) {
		readPlaylist(text)
		out, err := rewritePlaylist(text, base, fr)
		// (a stray control character is dropped from an address, as players read it: "rea\x11lP" is written "realP")
		plain := strings.Map(func(r rune) rune {
			if r < 0x20 && r != '\n' || r == 0x7f {
				return -1
			}
			return r
		}, text)
		if err == nil && !strings.Contains(plain, "realP") && strings.Contains(out, "realP") {
			t.Fatalf("a login appeared from nowhere")
		}
		if err == nil && strings.Contains(base, "provider.example:8080") && strings.HasPrefix(base, "http://") {
			for _, line := range strings.Split(out, "\n") {
				l := strings.TrimSpace(line)
				own := strings.HasPrefix(l, "/") && !strings.HasPrefix(l, "/__cdn/") || strings.HasPrefix(l, "/__cdn/http/provider.example:8080/")
				if own && strings.Contains(l, "realU/realP") {
					t.Fatalf("the provider's login in the player's playlist: %q", l)
				}
			}
		}
		if u, err := url.Parse(base); err == nil {
			fr.easyForm(u)
			fr.swap(u)
		}
		cdnTarget(base, base, text)
		realAddress(base)
	})
}
