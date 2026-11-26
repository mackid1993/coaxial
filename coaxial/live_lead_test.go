package main

import (
	"context"
	"encoding/binary"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// What viewers of a live channel experience through the proxy, measured as a player sees it: seconds of
// video in hand, stalls and time to first video. A provider sends a live channel the way providers do: an
// opening burst of several seconds of video, then a lump every 6 s, now and then 12 s apart. Two players
// watch: the first opens the channel, a second device joins 2 s later. Past reopenAfter the provider drops
// the stream in the middle of a lump; the proxy asks for it again, and the provider starts the new stream
// some seconds back, as providers do.
//
// The stream is a small synthetic one with that timing. COAXIAL_REPLAY_TS may name a recorded MPEG-TS file
// to replay instead; the file of the same name ending in .idx holds its timing: one 12-byte record per read,
// float64 seconds since the first byte, then uint32 bytes read, both little-endian.

// leadFeed: a live stream and when the provider makes each piece of it available.
type leadFeed struct {
	data []byte
	at   []float64  // seconds after the start when piece k is sent on the first connection
	end  []int      // where piece k ends in data
	back int        // a connection opened later starts this many bytes behind the newest piece
	late *lateWatch // if set: told what the first connection has sent (the first player is sent it as it is)
}

// lumpStart: the first piece at or after t that follows a pause of a second or more.
func (fd *leadFeed) lumpStart(t float64) int {
	for k := 1; k < len(fd.at); k++ {
		if fd.at[k] >= t && fd.at[k]-fd.at[k-1] >= 1 {
			return k
		}
	}
	return len(fd.at) - 1
}

func leadCRC(b []byte) uint32 {
	c := uint32(0xffffffff)
	for _, x := range b {
		c ^= uint32(x) << 24
		for k := 0; k < 8; k++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
	}
	return c
}

// leadTable: one packet carrying a PSI section (the CRC is added).
func leadTable(pid int, section []byte) []byte {
	c := leadCRC(section)
	s := append(section, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
	p := append([]byte{0x47, 0x40 | byte(pid>>8), byte(pid), 0x10, 0}, s...)
	for len(p) < 188 {
		p = append(p, 0xff)
	}
	return p
}

// syntheticFeed: about 4 Mbit/s of 50 fps video on PID 0x100 (its clock on each frame, a keyframe marked
// every 1.5 s with the program tables just before it) and sound on 0x101. The provider sends 18 s at the
// start in two reads 2 s apart, then a lump every 6 s, two of them after a 12 s pause.
func syntheticFeed() *leadFeed {
	const fps, gop = 50, 75
	pat := leadTable(0, []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0x00})
	pmt := leadTable(0x1000, []byte{0x02, 0xb0, 23, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0,
		0x1b, 0xe1, 0x00, 0xf0, 0, 0x0f, 0xe1, 0x01, 0xf0, 0})
	pkt := func(pid int, cc *byte, start bool, af []byte) []byte {
		p := []byte{0x47, byte(pid >> 8), byte(pid), 0x10 | *cc&0x0f}
		*cc++
		if start {
			p[1] |= 0x40
		}
		if af != nil {
			p[3] |= 0x20
			p = append(append(p, byte(len(af))), af...)
		}
		if start {
			p = append(p, 0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5, 0x21, 0, 1, 0, 1)
		}
		for len(p) < 188 {
			p = append(p, 0xa5)
		}
		return p
	}
	var data []byte
	var frameAt []int
	var vcc, acc byte
	for f := 0; f < 90*fps; f++ {
		frameAt = append(frameAt, len(data))
		key, n := f%gop == 0, 50
		if key {
			data, n = append(append(data, pat...), pmt...), 250
		}
		b := int64((1000 + float64(f)/fps) * 90000)
		af := []byte{0x10, byte(b >> 25), byte(b >> 17), byte(b >> 9), byte(b >> 1), byte(b&1)<<7 | 0x7e, 0}
		if key {
			af[0] |= 0x40
		}
		data = append(data, pkt(0x100, &vcc, true, af)...)
		for i := 1; i < n; i++ {
			data = append(data, pkt(0x100, &vcc, false, nil)...)
			if i%16 == 0 {
				data = append(data, pkt(0x101, &acc, i == 16, nil)...)
			}
		}
	}
	fd := &leadFeed{data: data, back: frameAt[12*fps]}
	lumps := [][2]float64{{0, 12}, {2, 18}, {8, 24}, {14, 30}, {20, 36}, {32, 48}, {38, 54}, {44, 60}, {56, 72}, {62, 78}}
	from := 0
	for _, l := range lumps {
		to := frameAt[int(l[1]*fps)]
		for ; from < to; from = min(to, from+64<<10) {
			fd.at, fd.end = append(fd.at, l[0]), append(fd.end, min(to, from+64<<10))
		}
	}
	return fd
}

// replayFeed: a recorded stream and its read timing; a later connection starts as far back as the
// recording's first 3 s were.
func replayFeed(t *testing.T, path string) *leadFeed {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := os.ReadFile(strings.TrimSuffix(path, ".ts") + ".idx")
	if err != nil {
		t.Fatal(err)
	}
	fd, sum := &leadFeed{data: data}, 0
	for i := 0; i+12 <= len(idx); i += 12 {
		sum += int(binary.LittleEndian.Uint32(idx[i+8:]))
		fd.at = append(fd.at, math.Float64frombits(binary.LittleEndian.Uint64(idx[i:])))
		fd.end = append(fd.end, min(sum, len(data)))
		if fd.at[len(fd.at)-1] <= 3 {
			fd.back = sum
		}
	}
	return fd
}

// leadProvider serves fd as one live channel, from the moment start on. The first connection is dropped
// in the middle of the first lump at or after dropAt; every later one starts fd.back behind the newest piece.
func leadProvider(fd *leadFeed, start time.Time, dropAt float64, asked *atomic.Int32) http.HandlerFunc {
	drop := fd.lumpStart(dropAt)
	return func(w http.ResponseWriter, r *http.Request) {
		n := asked.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		sent := 0
		send := func(b []byte) bool {
			_, err := w.Write(b)
			w.(http.Flusher).Flush()
			if sent += len(b); n == 1 && err == nil {
				fd.late.sent(sent)
			}
			return err == nil
		}
		from, k := 0, 0
		if n > 1 {
			now := time.Since(start).Seconds()
			for k < len(fd.at) && fd.at[k] <= now {
				k++
			}
			if k > 0 {
				e := fd.end[k-1]
				from = max(0, e-fd.back) / 188 * 188
				if !send(fd.data[from:e]) {
					return
				}
				from = e
			}
		}
		for ; k < len(fd.at); k++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Until(start.Add(time.Duration(fd.at[k] * float64(time.Second))))):
			}
			if n == 1 && k == drop { // half the lump, cut off mid-packet, and the connection drops
				send(fd.data[from : from+(fd.end[k]-from)/2/188*188+94])
				panic(http.ErrAbortHandler)
			}
			if !send(fd.data[from:fd.end[k]]) {
				return
			}
			from = fd.end[k]
		}
		<-r.Context().Done()
	}
}

// leadView: what one player was sent, as seconds of video, and when.
type leadView struct {
	asked     float64   // when it asked for the channel
	t, have   []float64 // after each read: when, and seconds of video received from its first keyframe on
	back      int       // times the clock went backwards (a player stops on that)
	skips     []float64 // seconds of video left out where a stream asked for again was joined on
	skipAtKey []bool    // whether each of those joins was made at a keyframe
}

// watch reads the stream until ctx ends, as a player that keeps everything it is sent.
func watch(ctx context.Context, t *testing.T, url string, start time.Time, v *leadView) {
	v.asked = time.Since(start).Seconds()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("asking for the channel: %v", err)
		return
	}
	defer resp.Body.Close()
	resp.Body = lateFor(t).reads(resp.Body, 0) // (counted, if this is the test's first player)
	var part []byte
	pcrPID, last, have, started := -1, 0.0, 0.0, false
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		part = append(part, buf[:n]...)
		i := 0
		for ; i+188 <= len(part); i += 188 {
			if part[i] != 0x47 { // (out of step, as after a stream that stopped mid-packet)
				for i < len(part) && part[i] != 0x47 {
					i++
				}
				i -= 188
				continue
			}
			p := part[i : i+188]
			pid := int(p[1]&0x1f)<<8 | int(p[2])
			if p[3]&0x20 == 0 || p[4] == 0 {
				continue
			}
			rai, hasPCR := p[5]&0x40 != 0, p[4] >= 7 && p[5]&0x10 != 0
			if !hasPCR || (pcrPID >= 0 && pid != pcrPID) {
				continue
			}
			pcrPID = pid
			pcr := float64(int64(p[6])<<25|int64(p[7])<<17|int64(p[8])<<9|int64(p[9])<<1|int64(p[10])>>7) / 90000
			switch d := pcr - last; {
			case !started:
				started = rai
			case d < 0:
				v.back++
			case d < 1:
				have += d
			default:
				v.skips, v.skipAtKey = append(v.skips, d), append(v.skipAtKey, rai)
			}
			if started {
				last = pcr
			}
		}
		part = append(part[:0], part[min(i, len(part)):]...)
		if n > 0 {
			v.t, v.have = append(v.t, time.Since(start).Seconds()), append(v.have, have)
		}
		if err != nil {
			return
		}
	}
}

// play: a player that starts once it holds startBuf seconds and, after running dry, starts again once it
// holds rebuf seconds; until end. first: seconds from asking to playing; least: the fewest seconds it held
// when one of the provider's lumps came (after a pause of a second or more); stalls and their seconds.
func (v *leadView) play(startBuf, rebuf, end float64) (first, least float64, stalls int, stalled float64) {
	first, least = -1, math.Inf(1)
	pos, have, now, playing := 0.0, 0.0, v.asked, false
	step := func(to float64) {
		if playing {
			if have-pos >= to-now {
				pos += to - now
			} else {
				stalls++
				stalled += to - (now + have - pos)
				pos, playing = have, false
			}
		} else if first >= 0 {
			stalled += to - now
		}
		now = to
	}
	for k := range v.t {
		step(v.t[k])
		if k > 0 && v.t[k]-v.t[k-1] >= 1 && first >= 0 { // what it holds as the provider's next lump comes
			least = math.Min(least, have-pos)
		}
		have = v.have[k]
		need := rebuf
		if first < 0 {
			need = startBuf
		}
		if !playing && have-pos >= need {
			playing = true
			if first < 0 {
				first = now - v.asked
			}
		}
	}
	step(end)
	return
}

func TestLiveViewersKeepVideo(t *testing.T) {
	fd, what := syntheticFeed(), "synthetic stream"
	if path := os.Getenv("COAXIAL_REPLAY_TS"); path != "" {
		fd, what = replayFeed(t, path), "recorded stream"
	}
	const dropAt, joinAt, end = reopenAfter + 3.0, 2.0, reopenAfter + 30.0
	fd.late = lateFor(t)
	start := time.Now()
	var asked atomic.Int32
	prov := httptest.NewServer(leadProvider(fd, start, dropAt, &asked))
	defer func() { prov.CloseClientConnections(); prov.Close() }()
	f := newFront(provider{Name: "Lead", URL: prov.URL, User: "realU", Password: "realP", Port: "18611", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(f)
	defer func() {
		whereIfFailed(t)
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(end*float64(time.Second)))
	defer cancel()
	views := []*leadView{{}, {}}
	var wg sync.WaitGroup
	for k, v := range views {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(float64(k) * joinAt * float64(time.Second)))
			watch(ctx, t, srv.URL+"/live/tv/easy/7.ts", start, v)
		}()
	}
	wg.Wait()
	if n := asked.Load(); n != 2 {
		t.Errorf("%s: the provider was asked %d times, want 2 (once, then once again after its drop)", what, n)
	}
	for k, v := range views {
		who := []string{"the first player", "a player that joined 2 s later"}[k]
		first, least, stalls, stalled := v.play(2, 1, end)
		t.Logf("%s, %s: first video %.1f s after asking, fewest seconds in hand %.1f, %d stalls (%.1f s), "+
			"left out at the reconnect %.1f s", what, who, first, least, stalls, stalled, v.skips)
		if first < 0 || first > 5 {
			t.Errorf("%s, %s: playing %.1f s after asking, want within 5 s", what, who, first)
		}
		if least < 1 {
			t.Errorf("%s, %s: held %.1f s of video when a lump came, want 1 s or more", what, who, least)
		}
		if stalls > 0 {
			t.Errorf("%s, %s: ran out of video %d times (%.1f s)", what, who, stalls, stalled)
		}
		if v.back > 0 {
			t.Errorf("%s, %s: the stream's clock went backwards %d times", what, who, v.back)
		}
		if len(v.skips) != 1 || !v.skipAtKey[0] || v.skips[0] > 5 {
			t.Errorf("%s, %s: joins after the reconnect %v s, at a keyframe %v; want one, at a keyframe, a few seconds at most",
				what, who, v.skips, v.skipAtKey)
		}
		if n := len(v.t); n == 0 || v.t[n-1] < end-15 {
			t.Errorf("%s, %s: no video in the last 15 s", what, who)
		}
	}
}
