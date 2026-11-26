package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Live TV watched as HLS on a provider set to MPEG-TS. The provider is only ever asked for MPEG-TS; the
// playlist and its pieces are made here, cut from the channel's one shared MPEG-TS stream (tshls.go). Until
// now only the cutting was tested, piece by piece, never the channel as a player watches it. Here a player
// tunes a real channel (program tables, H.264 with keyframes, sound) that the provider sends as providers do,
// an opening burst and then lumps, follows the playlist for as long as the channel runs, and a recorder asks
// for the same channel as MPEG-TS meanwhile (incident 2: mixed formats on one channel). What must hold:
//
//   1. the playlist comes as soon as the provider's opening burst has brought two pieces, and every address in
//      it, worked out the way a player works it out, leads to the proxy;
//   2. the playlist only ever moves forward, and each one carries on where the last one ended: a player that
//      reloads it is never left with a hole between two playlists;
//   3. every piece named can be had, starts with the program tables and a keyframe (a player can begin at any
//      piece), and the pieces joined end to end are the provider's video and sound, packet for packet, in
//      order, with nothing left out and nothing twice;
//   4. nothing is held: a piece is in the playlist as soon as the provider has sent the keyframe that ends it
//      (AGENTS.md rule 2);
//   5. the provider is asked for the channel once, however many devices watch it and in whatever format.

// tsHLSLate: how long after the provider sent the keyframe that ends a piece the piece may be missing from the
// playlist. (The player here asks every 200 ms, far more often than a real one, to see when it appears.)
const tsHLSLate = 1500 * time.Millisecond

// tsHLSProv: a provider of live MPEG-TS. Every connection is sent the channel from its start: burst seconds of
// video at once, then two more seconds of it every two seconds.
type tsHLSProv struct {
	s     *wreckStream
	burst int

	mu     sync.Mutex
	asks   map[string]int       // connections asked for, by channel
	open   map[string]int       // connections open now, by channel
	closed map[string]time.Time // when a channel's connection last ended
	sent   []roughMark          // the first connection: how much of the stream had been sent, and when
}

func (p *tsHLSProv) serve(w http.ResponseWriter, r *http.Request) {
	id := channelID(r.URL.Path)
	p.mu.Lock()
	p.asks[id]++
	p.open[id]++
	first := len(p.sent) == 0 && p.asks[id] == 1 && id == "7"
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.open[id]--
		p.closed[id] = time.Now()
		p.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "video/mp2t")
	start, from := time.Now(), 0
	for sec := p.burst; ; sec += 2 {
		to := len(p.s.data)
		if f := sec * wreckFPS; f < len(p.s.frames) {
			to = p.s.frames[f]
		}
		for from < to { // (in writes of 64 KB, as it comes off a socket)
			e := min(to, from+64<<10)
			if _, err := w.Write(p.s.data[from:e]); err != nil {
				return
			}
			from = e
		}
		w.(http.Flusher).Flush()
		if first {
			p.mu.Lock()
			p.sent = append(p.sent, roughMark{time.Now(), to})
			p.mu.Unlock()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Until(start.Add(time.Duration(sec-p.burst+2) * time.Second))):
		}
	}
}

func newTSHLSProv(t *testing.T, secs, burst int) (*tsHLSProv, string) {
	p := &tsHLSProv{s: wreckBuild(11, wreckPart{secs: secs, clock: 1000}), burst: burst,
		asks: map[string]int{}, open: map[string]int{}, closed: map[string]time.Time{}}
	fp := newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/live/realU/realP/") || !strings.HasSuffix(r.URL.Path, ".ts") {
			return false
		}
		p.serve(w, r)
		return true
	})
	_, base := limFront(t, fp, "ts", "", advanced{})
	return p, base
}

// tsHLSGet: one ask by device dev, the way a player makes it: sent elsewhere, it follows, working the new
// address out from the one just asked. An address that is not the proxy's fails the test.
func tsHLSGet(t *testing.T, dev, addr string) (status int, data []byte, kind, at string) {
	t.Helper()
	home, _ := url.Parse(addr)
	for hop := 0; hop < 6; hop++ {
		u, err := url.Parse(addr)
		if err != nil || u.Host != home.Host {
			t.Fatalf("the player was given the address %q, which does not lead to the proxy (%s)", addr, home.Host)
		}
		req, _ := http.NewRequest("GET", addr, nil)
		req.Header.Set("X-Dev", dev)
		resp, err := devClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		if loc := resp.Header.Get("Location"); isRedirect(resp.StatusCode) && loc != "" {
			resp.Body.Close()
			next, err := u.Parse(loc)
			if err != nil {
				t.Fatalf("sent on to an address the player cannot read: %q", loc)
			}
			addr = next.String()
			continue
		}
		data, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, data, resp.Header.Get("Content-Type"), addr
	}
	t.Fatalf("the player was sent from one address to the next 6 times and never answered")
	return
}

// tsHLSList: a playlist's MEDIA-SEQUENCE and the addresses of its pieces, as written.
func tsHLSList(text string) (seq int, names []string) {
	seq = -1
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"); ok {
			seq, _ = strconv.Atoi(v)
		} else if line != "" && !strings.HasPrefix(line, "#") {
			names = append(names, line)
		}
	}
	return seq, names
}

// tsHLSAV: the packets of an MPEG-TS stream that are not program tables (PAT, and the PMT on 0x1000), joined.
func tsHLSAV(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i+188 <= len(data); i += 188 {
		if pid := int(data[i+1]&0x1f)<<8 | int(data[i+2]); pid != 0 && pid != 0x1000 {
			out = append(out, data[i:i+188]...)
		}
	}
	return out
}

func TestLiveHLSFromTSPlaysTheChannelWhole(t *testing.T) {
	const secs, burst, watch = 34, 14, 19 * time.Second
	p, base := newTSHLSProv(t, secs, burst)
	began := time.Now()
	done := within(t, 5*time.Second, "the first playlist")
	status, data, _, at := tsHLSGet(t, "192.0.2.71", base+"/live/tv/easy/7.m3u8")
	done()
	tune := time.Since(began)
	if status != 200 || !bytes.Contains(data, []byte("#EXTINF")) {
		t.Fatalf("the channel asked for as HLS: %d %q", status, data)
	}
	if tune > 5*time.Second {
		t.Errorf("the first playlist took %.1f s; the provider's opening burst (%d s of video, sent at once) holds its first two pieces", tune.Seconds(), burst)
	}
	// A recorder asks for the same channel as MPEG-TS three seconds in, and reads it to the end.
	var recorded atomic.Int64
	recDone := make(chan string, 1)
	go func() {
		time.Sleep(3*time.Second - time.Since(began))
		req, _ := http.NewRequest("GET", base+"/live/tv/easy/7.ts", nil)
		req.Header.Set("X-Dev", "192.0.2.72")
		resp, err := devClient.Do(req)
		if err != nil {
			recDone <- err.Error()
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			recDone <- fmt.Sprintf("status %d", resp.StatusCode)
			return
		}
		buf := make([]byte, 64<<10)
		for time.Since(began) < watch {
			n, err := resp.Body.Read(buf)
			recorded.Add(int64(n))
			if err != nil {
				recDone <- fmt.Sprintf("its stream ended after %.1f s: %v", time.Since(began).Seconds(), err)
				return
			}
		}
		recDone <- ""
	}()
	// The player: the playlist again and again, and every piece it has not had yet.
	listAt, _ := url.Parse(at)
	pieces := map[int][]byte{}
	seen := map[int]time.Time{}
	firstSeq, lastSeq, lists := -1, -1, 0
	for time.Since(began) < watch {
		if lists > 0 {
			time.Sleep(200 * time.Millisecond)
			status, data, _, _ = tsHLSGet(t, "192.0.2.71", at)
			if status != 200 {
				t.Fatalf("playlist %d, %.1f s in: the player was answered %d %q", lists+1, time.Since(began).Seconds(), status, data)
			}
		}
		lists++
		now := time.Now()
		seq, names := tsHLSList(string(data))
		if seq < 0 || len(names) == 0 {
			t.Fatalf("playlist %d names no pieces or has no MEDIA-SEQUENCE:\n%s", lists, data)
		}
		if strings.Contains(string(data), "#EXT-X-ENDLIST") {
			t.Fatalf("playlist %d, %.1f s in, says the channel has ended (#EXT-X-ENDLIST) while the provider is still sending it:\n%s", lists, time.Since(began).Seconds(), data)
		}
		if firstSeq < 0 {
			firstSeq = seq
		}
		if lastSeq >= 0 && (seq+len(names)-1 < lastSeq || seq > lastSeq+1) {
			t.Fatalf("playlist %d names pieces %d to %d; the one before it ended at piece %d: a player is left with a hole, or sent back", lists, seq, seq+len(names)-1, lastSeq)
		}
		lastSeq = seq + len(names) - 1
		for i, name := range names {
			n := seq + i
			if _, have := pieces[n]; have {
				continue
			}
			u, err := listAt.Parse(name)
			if err != nil {
				t.Fatalf("playlist %d names a piece at an address the player cannot read: %q", lists, name)
			}
			st, piece, kind, _ := tsHLSGet(t, "192.0.2.71", u.String())
			if st != 200 || !strings.HasPrefix(kind, "video/") || len(piece) == 0 || len(piece)%188 != 0 {
				t.Fatalf("piece %d, named in playlist %d: status %d, type %q, %d bytes (want 200, video, whole packets)", n, lists, st, kind, len(piece))
			}
			pieces[n], seen[n] = piece, now
		}
	}
	if why := <-recDone; why != "" {
		t.Errorf("the recorder on the same channel as MPEG-TS: %s", why)
	} else if recorded.Load() < 1<<20 {
		t.Errorf("the recorder on the same channel as MPEG-TS was sent %d bytes in %d s", recorded.Load(), int(watch.Seconds())-3)
	}
	p.mu.Lock()
	asks, sent := p.asks["7"], append([]roughMark(nil), p.sent...)
	p.mu.Unlock()
	if asks != 1 {
		t.Errorf("the provider was asked for the channel %d times; one device watched it as HLS and one recorded it as MPEG-TS, and they share one connection: want 1", asks)
	}
	// The pieces, in order, against what the provider sent.
	source := tsHLSAV(p.s.data)
	var joined []byte
	starts := map[int]int{} // where each piece starts in the provider's video and sound
	for n := firstSeq; n <= lastSeq; n++ {
		piece, ok := pieces[n]
		if !ok {
			t.Fatalf("piece %d was never named in a playlist the player was given (pieces %d to %d were)", n, firstSeq, lastSeq)
		}
		pid := func(i int) int { return int(piece[i*188+1]&0x1f)<<8 | int(piece[i*188+2]) }
		if len(piece) < 3*188 || pid(0) != 0 || pid(1) != 0x1000 {
			t.Fatalf("piece %d does not start with the program tables (PAT, then PMT): a player cannot begin at it", n)
		}
		if first := piece[2*188:]; pid(2) != 0x100 || first[3]&0x20 == 0 || first[5]&0x40 == 0 {
			t.Errorf("piece %d does not start at a keyframe (its first video packet is on PID %#x and carries no keyframe mark)", n, pid(2))
		}
		starts[n] = len(joined)
		joined = append(joined, tsHLSAV(piece)...)
	}
	off := bytes.Index(source, joined[:min(len(joined), 188)])
	if off < 0 {
		t.Fatalf("the first piece does not begin with a packet the provider sent")
	}
	if want := source[off:min(len(source), off+len(joined))]; !bytes.Equal(joined, want) {
		t.Fatalf("the pieces joined end to end are not the provider's video and sound from there on: %s", diffAt(want, joined))
	}
	if off != 0 {
		t.Errorf("the first piece begins %d packets into the provider's video and sound; the stream began at a keyframe, so nothing before it should be missing", off/188)
	}
	t.Logf("tuned in %.1f s; %d playlists; pieces %d to %d (%d), %.1f MB, are the provider's video and sound packet for packet; the recorder was sent %.1f MB",
		tune.Seconds(), lists, firstSeq, lastSeq, len(pieces), float64(len(joined))/1e6, float64(recorded.Load())/1e6)
	if len(pieces) < 4 {
		t.Errorf("only %d pieces in %d s of a channel whose keyframes are 2 s apart", len(pieces), int(watch.Seconds()))
	}
	// Held? A piece is whole once the provider has sent the first video packet of the next one.
	avAt := func(avOff int) time.Time { // when the provider had sent this much of its video and sound
		raw := 0
		for i, n := 0, 0; i+188 <= len(p.s.data); i += 188 {
			if pid := int(p.s.data[i+1]&0x1f)<<8 | int(p.s.data[i+2]); pid != 0 && pid != 0x1000 {
				if n += 188; n >= avOff {
					raw = i + 188
					break
				}
			}
		}
		for _, m := range sent {
			if m.off >= raw {
				return m.at
			}
		}
		return time.Time{}
	}
	worst, worstOf := time.Duration(0), 0
	for n := firstSeq; n < lastSeq; n++ {
		ended := avAt(off + starts[n+1] + 188)
		if ended.IsZero() {
			continue
		}
		if late := seen[n].Sub(ended); late > worst {
			worst, worstOf = late, n
		}
	}
	t.Logf("a piece was in the playlist at most %d ms after the provider sent the keyframe that ends it (piece %d)", worst.Milliseconds(), worstOf)
	if worst > tsHLSLate {
		t.Errorf("video was held: piece %d was in the playlist %d ms after the provider had sent the keyframe that ends it; at most %d ms is allowed",
			worstOf, worst.Milliseconds(), tsHLSLate.Milliseconds())
	}
}

// A device watching one channel as HLS on a provider set to MPEG-TS changes to another channel. The HLS
// builder of the first channel otherwise stays on it for 30 s after its last playlist request (segIdle), and
// with it the provider connection: on an account with one connection the new channel would be refused, or
// would end the old one in a way the provider counts against the account. So the old channel is let go the
// moment its only device asks for another one, and the new channel plays at once.
func TestLiveHLSFromTSChannelChangeFreesTheOldChannel(t *testing.T) {
	p, base := newTSHLSProv(t, 34, 14)
	status, data, _, at := tsHLSGet(t, "192.0.2.71", base+"/live/tv/easy/7.m3u8")
	if status != 200 || !bytes.Contains(data, []byte("#EXTINF")) {
		t.Fatalf("channel 7 as HLS: %d %q", status, data)
	}
	_, names := tsHLSList(string(data))
	listAt, _ := url.Parse(at)
	u, _ := listAt.Parse(names[0])
	if st, piece, _, _ := tsHLSGet(t, "192.0.2.71", u.String()); st != 200 || len(piece) == 0 {
		t.Fatalf("channel 7's first piece: %d, %d bytes", st, len(piece))
	}
	changed := time.Now()
	done := within(t, 5*time.Second, "the new channel's first playlist")
	status, data, _, _ = tsHLSGet(t, "192.0.2.71", base+"/live/tv/easy/8.m3u8")
	done()
	if took := time.Since(changed); status != 200 || !bytes.Contains(data, []byte("#EXTINF")) || took > 5*time.Second {
		t.Errorf("channel 8 as HLS after the change: status %d after %.1f s, want its playlist at once (the provider's opening burst holds two pieces)\n%s", status, took.Seconds(), data)
	}
	gone := time.Time{}
	for wait := time.Now(); time.Since(wait) < 3*time.Second; time.Sleep(50 * time.Millisecond) {
		p.mu.Lock()
		open, at := p.open["7"], p.closed["7"]
		p.mu.Unlock()
		if open == 0 {
			gone = at
			break
		}
	}
	if gone.IsZero() {
		t.Fatalf("3 s after its only device changed channel, channel 7's connection at the provider is still open (it is kept %d s when nothing lets it go)", int(segIdle))
	}
	t.Logf("channel 7's connection at the provider ended %d ms after the device asked for channel 8", gone.Sub(changed).Milliseconds())
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.asks["7"] != 1 || p.asks["8"] != 1 {
		t.Errorf("the provider was asked for channel 7 %d times and for channel 8 %d times, want once each", p.asks["7"], p.asks["8"])
	}
}
