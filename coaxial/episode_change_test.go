package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A 24/7 channel plays one file per episode. When the next episode starts, the provider's stream carries on
// over the same connection, but its clock (PCR, PTS, DTS) starts over near zero. Players ignore the
// discontinuity flag that comes with it, so a viewer sees time go backwards: the picture freezes or loops.
// These tests send such a stream through the live path as a provider would and check what a player is given:
// the clock runs on (no step back, no leap forward), the bytes are the provider's apart from timestamp
// fields, and nothing before the change is touched.
//
// TestEpisodeChangeSynthetic builds the stream in Go. TestEpisodeChangeReplay sends a recorded real one:
// COAXIAL_REPLAY_EPISODE names an MPEG-TS file holding one episode change; the file of the same name ending in
// .idx (optional) holds the read timing it was recorded with (12 bytes per read: float64 seconds since the
// first byte, uint32 bytes, both little-endian), and the bytes are sent in those pieces at those times.

// ecStamp: one timestamp field in a stream.
type ecStamp struct {
	at    int   // where its packet starts
	field int   // where its 5 bytes start (PCR: the 33-bit base)
	pid   int   // its packet's PID
	kind  byte  // 'C' PCR, 'P' PTS, 'D' DTS
	video bool  // in a video PES packet
	v     int64 // its value, 90 kHz
}

func ecTS(b []byte) int64 {
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}

// ecStamps: every PCR, PTS and DTS in b, in stream order. b starts at a packet.
func ecStamps(b []byte) []ecStamp {
	var out []ecStamp
	for i := 0; i+188 <= len(b); i += 188 {
		p := b[i : i+188]
		if p[0] != 0x47 {
			continue
		}
		pid := int(p[1]&0x1f)<<8 | int(p[2])
		pos := 4
		if p[3]&0x20 != 0 {
			pos = 5 + int(p[4])
			if p[4] >= 7 && p[5]&0x10 != 0 {
				v := int64(p[6])<<25 | int64(p[7])<<17 | int64(p[8])<<9 | int64(p[9])<<1 | int64(p[10])>>7
				out = append(out, ecStamp{at: i, field: i + 6, pid: pid, kind: 'C', v: v})
			}
		}
		if p[1]&0x40 == 0 || p[3]&0x10 == 0 || pos+19 > 188 {
			continue
		}
		e := p[pos:]
		sid := e[3]
		if e[0] != 0 || e[1] != 0 || e[2] != 1 || !(sid >= 0xc0 && sid <= 0xef || sid == 0xbd) || e[6]&0xc0 != 0x80 {
			continue
		}
		flags := e[7] >> 6
		video := sid >= 0xe0
		if flags&2 != 0 {
			out = append(out, ecStamp{at: i, field: i + pos + 9, pid: pid, kind: 'P', video: video, v: ecTS(e[9:14])})
		}
		if flags == 3 {
			out = append(out, ecStamp{at: i, field: i + pos + 14, pid: pid, kind: 'D', video: video, v: ecTS(e[14:19])})
		}
	}
	return out
}

// ecDiff: a-b on the 33-bit clock, in seconds (the clock's own wrap is not a jump).
func ecDiff(a, b int64) float64 {
	const wrap = int64(1) << 33
	d := ((a-b)%wrap + wrap) % wrap
	if d >= wrap/2 {
		d -= wrap
	}
	return float64(d) / 90000
}

// ecLine: the clock a player follows for each stream: the PCR, and per PES stream the decode time (DTS, or
// the PTS when there is no DTS) and the PTS.
type ecLine struct {
	pid  int
	kind byte // 'C' PCR, 'T' decode time, 'P' PTS
}

// ecLines: each stamp's line and value, in stream order. The PCR line is the first PID that carries one.
func ecLines(st []ecStamp) (keys []ecLine, vals []int64, idx []int) {
	pcrPID := -1
	for k, s := range st {
		switch s.kind {
		case 'C':
			if pcrPID < 0 {
				pcrPID = s.pid
			}
			if s.pid == pcrPID {
				keys, vals, idx = append(keys, ecLine{s.pid, 'C'}), append(vals, s.v), append(idx, k)
			}
		case 'P':
			keys, vals, idx = append(keys, ecLine{s.pid, 'P'}), append(vals, s.v), append(idx, k)
			if k+1 >= len(st) || st[k+1].kind != 'D' || st[k+1].at != s.at {
				keys, vals, idx = append(keys, ecLine{s.pid, 'T'}), append(vals, s.v), append(idx, k)
			}
		case 'D':
			keys, vals, idx = append(keys, ecLine{s.pid, 'T'}), append(vals, s.v), append(idx, k)
		}
	}
	return
}

// ecFirstJump: where the input's clock starts over: the first packet whose PCR or decode time is more than
// 1 s behind or 30 s ahead of the one before it on its line. -1: none.
func ecFirstJump(st []ecStamp) (at int, from, to float64) {
	keys, vals, idx := ecLines(st)
	last := map[ecLine]int64{}
	for k, key := range keys {
		if key.kind == 'P' {
			continue
		}
		if p, ok := last[key]; ok {
			if d := ecDiff(vals[k], p); d < -1 || d > 30 {
				return st[idx[k]].at, float64(p) / 90000, float64(vals[k]) / 90000
			}
		}
		last[key] = vals[k]
	}
	return -1, 0, 0
}

// ecProvider serves data as one live channel in the given pieces, each at its time (seconds after the
// connection's start), then holds the connection open as a live channel does.
func ecProvider(data []byte, ends []int, at []float64, asked *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		start, from := time.Now(), 0
		for k, end := range ends {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Until(start.Add(time.Duration(at[k] * float64(time.Second))))):
			}
			if _, err := w.Write(data[from:end]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			from = end
		}
		<-r.Context().Done()
	}
}

// ecThrough sends data through the live path as a provider would and returns what a player was sent.
func ecThrough(t *testing.T, data []byte, ends []int, at []float64, portNo string) []byte {
	t.Helper()
	var asked atomic.Int32
	prov := httptest.NewServer(ecProvider(data, ends, at, &asked))
	defer func() { prov.CloseClientConnections(); prov.Close() }()
	f := newFront(provider{Name: "Episodes", URL: prov.URL, User: "realU", Password: "realP", Port: port(portNo), LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(f)
	defer func() {
		srv.CloseClientConnections()
		srv.Close()
		mu.Lock()
		for _, ch := range channels {
			if ch.provider == f.base {
				ch.drop()
			}
		}
		mu.Unlock()
	}()
	wall := at[len(at)-1] + 30
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wall*float64(time.Second)))
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/live/tv/easy/24.ts", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("asking for the channel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("asking for the channel: status %d", resp.StatusCode)
	}
	var got bytes.Buffer
	buf := make([]byte, 64<<10)
	for got.Len() < len(data) {
		n, err := resp.Body.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				t.Logf("reading the channel: %v", err)
			}
			break
		}
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("the provider was asked %d times, want once", n)
	}
	return got.Bytes()
}

// ecCheck: what the player was sent (out) against what the provider sent (in).
func ecCheck(t *testing.T, what string, in, out []byte) {
	t.Helper()
	inSt := ecStamps(in)
	jump, from, to := ecFirstJump(inSt)
	if jump < 0 {
		t.Fatalf("%s: the provider's stream has no clock that starts over; nothing to test", what)
	}
	t.Logf("%s: %d bytes; the provider's clock starts over at byte %d (%.3f s, then %.3f s)", what, len(in), jump, from, to)

	// Nothing added or taken out, and nothing before the change touched.
	if len(out) != len(in) {
		t.Errorf("%s: the player was sent %d bytes, the provider sent %d", what, len(out), len(in))
	}
	if n := min(jump, len(out)); !bytes.Equal(out[:n], in[:n]) {
		k := 0
		for k < n && out[k] == in[k] {
			k++
		}
		t.Errorf("%s: before the clock started over, the stream was changed (first at byte %d of %d)", what, k, jump)
	}
	// Apart from the timestamp fields, every byte is the provider's.
	a, b := append([]byte(nil), in...), append([]byte(nil), out[:min(len(out), len(in))]...)
	for _, s := range inSt {
		for _, x := range [][]byte{a, b} {
			if s.field+5 <= len(x) {
				copy(x[s.field:s.field+5], make([]byte, 5))
			}
		}
	}
	if !bytes.Equal(a[:len(b)], b) {
		k := 0
		for k < len(b) && a[k] == b[k] {
			k++
		}
		t.Errorf("%s: bytes other than timestamps were changed (first at byte %d)", what, k)
	}

	// The clock a player follows runs on: the PCR never goes back, nothing leaps ahead more than 30 s, a
	// decode time never goes back more than 50 ms, and a PTS (pictures are shown in another order than
	// decoded) never more than 1 s. (One offset moves every stream on together; where an episode's sound
	// tracks ended a few milliseconds apart, the next one's first sound frame can overlap the last one's by
	// that much, as on a real recorded change: 3 ms. Players and recorders take that in their stride; a
	// clock that starts over is a step back of minutes.)
	keys, vals, idx := ecLines(ecStamps(out))
	outSt := ecStamps(out)
	last, bad := map[ecLine]int64{}, map[ecLine]int{}
	name := map[byte]string{'C': "PCR", 'T': "decode time (DTS)", 'P': "PTS"}
	var after []string
	for k, key := range keys {
		p, ok := last[key]
		last[key] = vals[k]
		if !ok {
			continue
		}
		d, back := ecDiff(vals[k], p), 0.0
		switch key.kind {
		case 'T':
			back = -0.05
		case 'P':
			back = -1
		}
		if d < back || d > 30 {
			bad[key]++
			if bad[key] <= 3 {
				t.Errorf("%s: the player's %s on PID %d went from %.3f s to %.3f s (%+.3f s) at byte %d",
					what, name[key.kind], key.pid, float64(p)/90000, float64(vals[k])/90000, d, outSt[idx[k]].at)
			}
		}
		if s := outSt[idx[k]]; s.at >= jump && s.at < jump+188*400 && key.kind != 'P' && len(after) < 6 {
			after = append(after, fmt.Sprintf("%s pid %d %.3f -> %.3f", name[key.kind], key.pid, float64(p)/90000, float64(vals[k])/90000))
		}
	}
	for key, n := range bad {
		if n > 3 {
			t.Errorf("%s: ... %d such steps on the %s of PID %d", what, n, name[key.kind], key.pid)
		}
	}
	t.Logf("%s: what the player was sent where the provider's clock started over: %s", what, strings.Join(after, "; "))
}

// ecPieces: data cut into reads the way a provider sends a live channel: a lump of about lump bytes every
// gap seconds, each lump written in pieces that end in the middle of a packet.
func ecPieces(data []byte, lump int, gap float64) (ends []int, at []float64) {
	for from, k := 0, 0; from < len(data); k++ {
		to := min(len(data), from+lump)
		for p := from; p < to; {
			p = min(to, p+65536+94) // (a read rarely ends on a packet)
			ends, at = append(ends, p), append(at, float64(k)*gap)
		}
		from = to
	}
	return
}

// ecSynthetic: a 24/7 channel's stream across an episode change. The first file has been playing for 22
// minutes; the next starts its clock at 0.7 s, as files do, with the discontinuity flag on every PID and its
// continuity counters started over, the program tables first. 25 fps video (PID 0x100, carrying the PCR,
// pictures decoded in another order than shown) and AAC-like audio (PID 0x101), both 0.5 s ahead of the PCR.
func ecSynthetic() []byte {
	const fps, perFrame, aFrame = 25, 12, 1920 // video packets per frame; 1024 samples at 48 kHz in 90 kHz
	var out []byte
	cc := map[int]byte{}
	pkt := func(pid int, start bool, af []byte, payload []byte) {
		p := []byte{0x47, byte(pid >> 8), byte(pid), 0x10 | cc[pid]&0x0f}
		cc[pid]++
		if start {
			p[1] |= 0x40
		}
		if af != nil {
			p[3] |= 0x20
			p = append(append(p, byte(len(af))), af...)
		}
		p = append(p, payload...)
		for len(p) < 188 {
			p = append(p, byte(len(out)/188*7+len(p)))
		}
		out = append(out, p[:188]...)
	}
	ts := func(prefix byte, v int64) []byte {
		return []byte{prefix<<4 | byte(v>>29)&0x0e | 1, byte(v >> 22), byte(v>>14)&0xfe | 1, byte(v >> 7), byte(v<<1)&0xfe | 1}
	}
	pcrAF := func(v int64, flags byte) []byte {
		return []byte{0x10 | flags, byte(v >> 25), byte(v >> 17), byte(v >> 9), byte(v >> 1), byte(v&1)<<7 | 0x7e, 0}
	}
	pat := leadTable(0, []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0x00})
	pmt := leadTable(0x1000, []byte{0x02, 0xb0, 23, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0,
		0x1b, 0xe1, 0x00, 0xf0, 0, 0x0f, 0xe1, 0x01, 0xf0, 0})
	tables := func(disc bool) {
		for _, t := range [][]byte{pat, pmt} {
			p := append([]byte(nil), t...)
			pid := int(p[1]&0x1f)<<8 | int(p[2])
			p[3] = 0x10 | cc[pid]&0x0f
			cc[pid]++
			if disc { // (the flag needs an adaptation field: a 2-byte one before the section)
				p = append([]byte{p[0], p[1], p[2], p[3] | 0x20, 1, 0x80}, p[4:]...)[:188]
			}
			out = append(out, p...)
		}
	}
	episode := func(pcr0 int64, seconds int, disc bool) {
		aPTS := pcr0 + 45000
		for f := 0; f < seconds*fps; f++ {
			pcr := pcr0 + int64(f)*90000/fps
			first := disc && f == 0
			if f%(fps/2) == 0 {
				tables(first)
			}
			// decode order I P B B P B B ...: P frames are shown 3 frames later, B frames one earlier
			show := f + 2
			if f%3 != 0 {
				show = f - 1
			}
			dts, pts := pcr+45000, pcr+45000+int64(show-f+2)*90000/fps
			var flags byte
			if f%(2*fps) == 0 {
				flags |= 0x40 // a keyframe
			}
			if first {
				flags |= 0x80
			}
			pes := append([]byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0xc0, 10}, append(ts(3, pts), ts(1, dts)...)...)
			pkt(0x100, true, pcrAF(pcr, flags), pes)
			for i := 1; i < perFrame; i++ {
				pkt(0x100, false, nil, nil)
			}
			for aPTS <= dts+int64(90000/fps) {
				var af []byte
				if first {
					af, first = []byte{0x80}, false
				}
				pkt(0x101, true, af, append([]byte{0, 0, 1, 0xc0, 0, 0, 0x80, 0x80, 5}, ts(2, aPTS)...))
				aPTS += aFrame
			}
		}
	}
	episode(int64(1326.4*90000), 20, false)
	for pid := range cc { // the next file's counters start over
		cc[pid] = 0
	}
	episode(int64(0.7*90000), 20, true)
	return out
}

func TestEpisodeChangeSynthetic(t *testing.T) {
	data := ecSynthetic()
	// about 6 s of the stream per lump, sent faster than real time: what matters here is the order of the
	// bytes and where reads end, not the pauses between lumps
	ends, at := ecPieces(data, len(data)/40*6, 0.05)
	out := ecThrough(t, data, ends, at, "18631")
	ecCheck(t, "synthetic 24/7 stream", data, out)
}

func TestEpisodeChangeReplay(t *testing.T) {
	path := os.Getenv("COAXIAL_REPLAY_EPISODE")
	if path == "" {
		t.Skip("COAXIAL_REPLAY_EPISODE names no recorded episode change")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ends []int
	var at []float64
	if idx, err := os.ReadFile(strings.TrimSuffix(path, ".ts") + ".idx"); err == nil {
		sum := 0
		for i := 0; i+12 <= len(idx) && sum < len(data); i += 12 {
			sum = min(len(data), sum+int(binary.LittleEndian.Uint32(idx[i+8:])))
			ends, at = append(ends, sum), append(at, math.Float64frombits(binary.LittleEndian.Uint64(idx[i:])))
		}
		if sum < len(data) {
			ends, at = append(ends, len(data)), append(at, at[len(at)-1])
		}
	} else {
		ends, at = ecPieces(data, 4<<20, 0.05)
	}
	out := ecThrough(t, data, ends, at, "18632")
	ecCheck(t, "recorded 24/7 stream", data, out)
}
