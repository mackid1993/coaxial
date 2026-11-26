package main

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The HLS-to-MPEG-TS bridge, fed the roughest HLS a provider serves. A recorder (or a player) on a provider
// whose players use HLS asks for a live channel as MPEG-TS, and Coaxial makes that from the channel's HLS
// (hlsSource), following the playlist by itself for as long as anyone reads. live_rough_test.go and
// live_wreck_test.go hold live MPEG-TS to "the same bytes, as they come"; live_hls_rough_test.go does it for
// live HLS; this is the third of the three, for the bridge. The channel is a real one (program tables, H.264
// with a keyframe at the start of every piece, sound), and the provider does what providers do: answers a
// reload late, serves the same playlist again and again and then five new pieces at once, stalls for 10 s,
// answers 404 once for a piece, never has a piece at all, sends a piece slowly, lets its numbering leap ahead
// or start afresh, moves the playlist and a piece to another server; and the readers do what readers do: two
// on one channel, one that joins in the middle, one that stops reading for a few seconds, a second device
// watching the same channel as HLS. A recording has no second chance, so in every scene, for every reader:
//
//  1. from its first packet on, the reader is sent the provider's pieces byte for byte, each once and in
//     order: nothing left out, nothing twice, nothing changed. A piece the provider never has is the one
//     thing that may be missing: the stream carries on with the next piece, and the log says a piece is
//     missing;
//  2. the stream stays MPEG-TS: 188-byte packets, in step from the first packet to the last, across every
//     piece boundary;
//  3. no piece reaches a reader later than a player would wait (8 s, AGENTS.md rule 14) after the provider
//     published it, the provider's own delays and the reader's own pause aside. When one does, where every
//     part of the program was waiting at that moment is in the failure, as file:line;
//  4. the provider is asked no more than a player straight at it would ask: the channel is opened once,
//     however many readers (every open is a new session at the provider, and its newest session is the only
//     one that still answers), the playlist no more often than HLS players reload one, each piece once (and
//     again only for cause: it was not there), and nothing once the last reader has hung up.
//
// The scenes run side by side; each lasts as long as its channel, 13 to 23 s.

const (
	brRoughLast   = 18              // the channel's last piece: every reader is checked up to it
	brRoughWindow = 8               // pieces a playlist names
	brRoughLate   = 8 * time.Second // how long after the provider published a piece a reader may be sent it
)

// brRoughStream: the channel, 2 s of video (from one keyframe to the next) per piece.
var brRoughStream = sync.OnceValue(func() *wreckStream { return wreckBuild(21, wreckPart{secs: 2 * brRoughLast, clock: 1000}) })

// brRoughSpan: where piece id (from 1) lies in the channel's stream.
func brRoughSpan(id int) (from, to int) {
	s := brRoughStream()
	at := func(piece int) int {
		if f := piece * wreckGOP; f < len(s.frames) {
			return s.frames[f]
		}
		return len(s.data)
	}
	return at(id - 1), at(id)
}

// brRoughPieceOf: the piece the stream's byte at off belongs to.
func brRoughPieceOf(off int) int {
	for id := 1; id <= brRoughLast; id++ {
		if _, to := brRoughSpan(id); off < to {
			return id
		}
	}
	return brRoughLast
}

// brRoughPiece: how one piece is served (the zero value: whole, at once).
type brRoughPiece struct {
	gone  int           // the first asks for it are answered 404
	never bool          // every ask for it is answered 404
	slow  time.Duration // it is sent in three parts, this long apart
	moved bool          // it is on the other server: the first one sends the reader on
}

// brRoughReader: a device that asks for the channel as MPEG-TS.
type brRoughReader struct {
	dev      string
	joinAt   float64 // when it asks, seconds in
	pauseAt  float64 // when it stops reading (0: never)...
	pauseFor float64 // ...and for how long
}

// brRoughScene: a live channel of 1 s pieces, the newest brRoughWindow of them in its playlist.
type brRoughScene struct {
	name      string
	ch        int                             // the channel's number (each scene has its own, to tell the log lines apart)
	top       func(t float64) int             // the newest piece t seconds in (nil: one more every second)
	seqOf     func(id int) int64              // the MEDIA-SEQUENCE of a playlist that begins at piece id (nil: 100+id)
	listWait  map[int]time.Duration           // how long the n-th ask for the playlist (from 1) takes to answer
	pieces    map[int]brRoughPiece            // the pieces served another way than whole and at once
	movedFrom float64                         // from this second on the playlist is on another server (0: never)
	readers   []brRoughReader                 // (none given: one, from the start)
	player    func(t *testing.T, base string) // another device on the channel as HLS meanwhile (nil: none)
}

type brRoughProv struct {
	sc                  *brRoughScene
	start               time.Time
	front, video, other *fakeProv

	mu      sync.Mutex
	opens   int         // asks at the channel's own address: each one opens a new session
	reloads int         // asks for the playlist that were answered with it
	stale   int         // asks at a session that is not the newest (answered 404, as real providers do)
	asked   map[int]int // asks for each piece, on either server
}

func brRoughTop(sc *brRoughScene, t float64) int {
	top := brRoughWindow + int(t)
	if sc.top != nil {
		top = sc.top(t)
	}
	return min(top, brRoughLast)
}

func newBRRoughProv(t *testing.T, sc *brRoughScene) *brRoughProv {
	p := &brRoughProv{sc: sc, start: time.Now(), asked: map[int]int{}}
	s := brRoughStream()
	hold := func(r *http.Request, d time.Duration) bool {
		if d <= 0 {
			return true
		}
		select {
		case <-time.After(d):
			return true
		case <-r.Context().Done():
			return false
		}
	}
	// newest: the address is in the session opened last (the others are gone, as at real providers).
	newest := func(path string) bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		if strings.HasPrefix(path, fmt.Sprintf("/hlsr/tok%d/", p.opens)) || strings.HasPrefix(path, "/moved/") {
			return true
		}
		p.stale++
		return false
	}
	list := func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.reloads++
		wait := sc.listWait[p.reloads]
		p.mu.Unlock()
		if !hold(r, wait) {
			return
		}
		top := brRoughTop(sc, time.Since(p.start).Seconds())
		first := max(1, top-brRoughWindow+1)
		seq := int64(100 + first)
		if sc.seqOf != nil {
			seq = sc.seqOf(first)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", seq)
		for id := first; id <= top; id++ {
			fmt.Fprintf(&b, "#EXTINF:1.000,\np%d.ts\n", id)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte(b.String()))
	}
	piece := func(w http.ResponseWriter, r *http.Request, onOther bool) bool {
		name := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "p"), ".ts"))
		if err != nil || !strings.HasPrefix(name, "p") || !strings.HasSuffix(name, ".ts") || id < 1 || id > brRoughLast {
			return false
		}
		if !newest(r.URL.Path) {
			http.NotFound(w, r)
			return true
		}
		how := sc.pieces[id]
		p.mu.Lock()
		p.asked[id]++
		n := p.asked[id]
		p.mu.Unlock()
		switch {
		case n <= how.gone || how.never:
			http.NotFound(w, r)
			return true
		case how.moved && !onOther:
			http.Redirect(w, r, p.other.srv.URL+"/moved/"+name, 302)
			return true
		}
		from, to := brRoughSpan(id)
		data := s.data[from:to]
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		for part := 0; part < 3; part++ {
			if part > 0 && !hold(r, how.slow) {
				return true
			}
			w.Write(data[part*len(data)/3 : (part+1)*len(data)/3])
			w.(http.Flusher).Flush()
		}
		return true
	}
	p.video = newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/index.m3u8") {
			return piece(w, r, false)
		}
		switch {
		case !newest(r.URL.Path):
			http.NotFound(w, r)
		case sc.movedFrom > 0 && time.Since(p.start).Seconds() >= sc.movedFrom:
			http.Redirect(w, r, p.other.srv.URL+"/moved/index.m3u8", 302)
		default:
			list(w, r)
		}
		return true
	})
	p.other = newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/moved/index.m3u8" {
			return piece(w, r, true)
		}
		list(w, r)
		return true
	})
	p.front = newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != fmt.Sprintf("/live/realU/realP/%d.m3u8", sc.ch) {
			return false
		}
		p.mu.Lock()
		p.opens++
		n := p.opens
		p.mu.Unlock()
		http.Redirect(w, r, fmt.Sprintf("%s/hlsr/tok%d/index.m3u8", p.video.srv.URL, n), 302)
		return true
	})
	return p
}

// ownDelay: the time the provider itself adds in this scene, all of it together.
func (sc *brRoughScene) ownDelay() (d time.Duration) {
	for _, w := range sc.listWait {
		d += w
	}
	for _, how := range sc.pieces {
		d += 2*how.slow + time.Duration(how.gone)*time.Second // (a piece that was not there is asked for again a second later)
		if how.never {
			d += 2 * time.Second // (and twice more for one that never comes, before the stream carries on)
		}
	}
	return d
}

func (p *brRoughProv) asks() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	pieces := 0
	for _, n := range p.asked {
		pieces += n
	}
	return fmt.Sprintf("the channel's own address %d times, the playlist %d times, pieces %d times, a session already replaced %d times",
		p.opens, p.reloads, pieces, p.stale)
}

// brRoughRead: what one reader was sent, followed packet by packet against the channel's stream.
type brRoughRead struct {
	spec    brRoughReader
	mu      sync.Mutex
	started time.Duration         // when its first bytes came (0: none yet)
	first   int                   // where its first packet lies in the channel's stream (-1: not known yet)
	pos     int                   // where the next packet is expected to lie
	bytes   int                   // bytes read
	gotAt   map[int]time.Duration // when the last packet of each piece came
	skipped []int                 // whole pieces the stream went past
	wrong   string                // the first thing that is not the provider's stream in order
	done    chan struct{}
}

// take: the reader has been sent these whole packets.
func (rd *brRoughRead) take(pkts []byte, now time.Duration, ends map[int]int, never map[int]bool) {
	s := brRoughStream()
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.started == 0 {
		rd.started = now
	}
	for ; len(pkts) >= 188; pkts, rd.bytes = pkts[188:], rd.bytes+188 {
		pkt := pkts[:188]
		if pkt[0] != 0x47 && rd.wrong == "" {
			rd.wrong = fmt.Sprintf("the stream is out of step: %d bytes in, a packet begins with %#02x, not 0x47 (the packet before it ended at byte %d of the channel, in piece %d)",
				rd.bytes, pkt[0], rd.pos, brRoughPieceOf(max(rd.pos-1, 0)))
		}
		if rd.first < 0 { // where the stream begins: its first three packets, found in the channel
			if len(pkts) < 3*188 {
				return
			}
			rd.first = bytes.Index(s.data, pkts[:3*188])
			if rd.first < 0 || rd.first%188 != 0 {
				rd.wrong, rd.first = fmt.Sprintf("the stream begins with packets the provider never sent: % x ...", pkt[:16]), 0
			}
			rd.pos = rd.first
		}
		// A whole piece the provider never had may be passed over, at a piece boundary.
		if id := brRoughPieceOf(rd.pos); rd.pos < len(s.data) && never[id] {
			if from, to := brRoughSpan(id); rd.pos == from && to+188 <= len(s.data) && bytes.Equal(pkt, s.data[to:to+188]) {
				rd.skipped, rd.pos = append(rd.skipped, id), to
			}
		}
		if rd.pos+188 <= len(s.data) && bytes.Equal(pkt, s.data[rd.pos:rd.pos+188]) {
			rd.pos += 188
			if id, ok := ends[rd.pos]; ok {
				if _, had := rd.gotAt[id]; !had {
					rd.gotAt[id] = now
				}
			}
			continue
		}
		// Not the packet that follows. Where does it lie in the channel?
		at, was := -1, brRoughPieceOf(max(rd.pos-1, 0))
		if len(pkts) >= 2*188 {
			at = bytes.Index(s.data, pkts[:2*188])
		}
		if rd.wrong == "" {
			switch {
			case at < 0:
				rd.wrong = fmt.Sprintf("in piece %d (%d bytes into the stream) came a packet that does not follow and that the provider never sent so: % x ...", was, rd.bytes, pkt[:16])
			case at > rd.pos:
				rd.wrong = fmt.Sprintf("video the provider published was left out: after byte %d of the channel (in piece %d) came byte %d (in piece %d); %d bytes are missing",
					rd.pos, was, at, brRoughPieceOf(at), at-rd.pos)
			default:
				rd.wrong = fmt.Sprintf("video was sent twice: after byte %d of the channel (in piece %d) came byte %d (in piece %d) again", rd.pos, was, at, brRoughPieceOf(at))
			}
		}
		if at >= 0 && at%188 == 0 {
			rd.pos = at + 188
		}
	}
}

func brRoughRecord(t *testing.T, sc *brRoughScene) {
	prov := newBRRoughProv(t, sc)
	_, base := limFront(t, prov.front, "hls", "", advanced{})
	// When each piece is published: the first moment the channel's newest piece is that one or a later one.
	published, ends, never := map[int]time.Duration{}, map[int]int{}, map[int]bool{}
	for ms := 0; ms <= 60000; ms += 50 {
		for id := 1; id <= brRoughTop(sc, float64(ms)/1000); id++ {
			if _, ok := published[id]; !ok {
				published[id] = time.Duration(ms) * time.Millisecond
			}
		}
	}
	for id := 1; id <= brRoughLast; id++ {
		_, to := brRoughSpan(id)
		ends[to], never[id] = id, sc.pieces[id].never
	}
	specs := sc.readers
	if len(specs) == 0 {
		specs = []brRoughReader{{dev: "192.0.2.9"}}
	}
	allowedFor := func(spec brRoughReader) time.Duration {
		return brRoughLate + sc.ownDelay() + time.Duration(spec.pauseFor*float64(time.Second))
	}
	end := published[brRoughLast] + brRoughLate + sc.ownDelay() + 10*time.Second
	// While the scene runs: the first time a piece is not with a reader in the time allowed, where every part
	// of the program is at that moment is written down (as in live_rough_test.go).
	var held heldNote
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	var reads []*brRoughRead
	var wg sync.WaitGroup
	for _, spec := range specs {
		rd := &brRoughRead{spec: spec, first: -1, gotAt: map[int]time.Duration{}, done: make(chan struct{})}
		reads = append(reads, rd)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(rd.done)
			time.Sleep(time.Until(prov.start.Add(time.Duration(spec.joinAt * float64(time.Second)))))
			req, _ := http.NewRequest("GET", fmt.Sprintf("%s/live/tv/easy/%d.ts", base, sc.ch), nil)
			req.Header.Set("X-Dev", spec.dev)
			resp, err := devClient.Do(req)
			if err != nil || resp.StatusCode != 200 {
				rd.mu.Lock()
				rd.wrong = fmt.Sprintf("it asked for the channel as MPEG-TS and was answered %v, %v", resp, err)
				rd.mu.Unlock()
				return
			}
			defer resp.Body.Close()
			go func() { // (hung up when the scene is over, wherever the read is)
				select {
				case <-rd.done:
				case <-time.After(time.Until(prov.start.Add(end))):
					resp.Body.Close()
				}
			}()
			var buf []byte
			chunk := make([]byte, 64<<10)
			paused := false
			for {
				if now := time.Since(prov.start).Seconds(); spec.pauseFor > 0 && !paused && now >= spec.pauseAt {
					paused = true
					time.Sleep(time.Duration(spec.pauseFor * float64(time.Second))) // (it reads nothing for a while)
				}
				n, err := resp.Body.Read(chunk)
				buf = append(buf, chunk[:n]...)
				whole := len(buf) / 188 * 188
				if whole < 3*188 && err == nil {
					continue // (the beginning is looked for in the first three packets)
				}
				rd.take(buf[:whole], time.Since(prov.start), ends, never)
				buf = append(buf[:0], buf[whole:]...)
				rd.mu.Lock()
				all := rd.gotAt[brRoughLast] != 0
				rd.mu.Unlock()
				if err != nil || all {
					return
				}
			}
		}()
		go func() { // the watch: is each piece with this reader in time?
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stopWatch:
					return
				case <-rd.done:
					return
				case <-tick.C:
				}
				now := time.Since(prov.start)
				rd.mu.Lock()
				late := 0
				if rd.first >= 0 {
					for id := brRoughPieceOf(rd.first) + 1; id <= brRoughLast; id++ {
						due := max(published[id], rd.started) + allowedFor(spec)
						if _, ok := rd.gotAt[id]; !ok && !never[id] && now > due {
							late = id
							break
						}
					}
				}
				rd.mu.Unlock()
				if late != 0 {
					held.add(fmt.Sprintf("piece %d, published %.1f s in, was not with the reader %s %.1f s in. The program was here:\n%s",
						late, published[late].Seconds(), spec.dev, now.Seconds(), whereItIsNow(true)))
					return
				}
			}
		}()
	}
	if sc.player != nil {
		go sc.player(t, base)
	}
	wg.Wait()
	took := time.Since(prov.start)
	where := func() string {
		if w := held.Load(); w != "" {
			return "\n\n" + w
		}
		return ""
	}
	lowest := brRoughLast
	for _, rd := range reads {
		rd.mu.Lock()
		who := "the reader " + rd.spec.dev
		if rd.spec.joinAt > 0 {
			who += fmt.Sprintf(" (it joined %.1f s in)", rd.spec.joinAt)
		}
		if rd.first < 0 {
			t.Errorf("%s was sent no video in %.0f s (%d bytes; %s); the provider was asked %s%s", who, took.Seconds(), rd.bytes, rd.wrong, prov.asks(), where())
			rd.mu.Unlock()
			continue
		}
		first := brRoughPieceOf(rd.first)
		lowest = min(lowest, first)
		t.Logf("%s: %.1f MB, from piece %d (byte %d of the channel) to piece %d, pieces passed over: %v",
			who, float64(rd.bytes)/1e6, first, rd.first, brRoughPieceOf(max(rd.pos-1, 0)), rd.skipped)
		if rd.wrong != "" {
			t.Errorf("%s was not sent the provider's video whole, once and in order: %s", who, rd.wrong)
		}
		if from, _ := brRoughSpan(first); rd.spec.joinAt == 0 && (rd.first != from || first > brRoughWindow) {
			t.Errorf("%s opened the channel and its stream begins at byte %d of piece %d; want the start of one of the pieces the first playlist named (1 to %d)",
				who, rd.first-from, first, brRoughWindow)
		}
		var missing []int
		for id := range never {
			if never[id] && id > first {
				missing = append(missing, id)
			}
		}
		sort.Ints(missing)
		if fmt.Sprint(missing) != fmt.Sprint(rd.skipped) {
			t.Errorf("%s: the provider never had pieces %v, and those are the pieces the stream should carry on past; it went past %v", who, missing, rd.skipped)
		}
		worst, worstOf := time.Duration(0), 0
		for id := first + 1; id <= brRoughLast; id++ {
			if never[id] {
				continue
			}
			at, ok := rd.gotAt[id]
			if !ok {
				t.Errorf("%s: piece %d, which the provider published %.1f s in, is not whole in its stream %.1f s in (the stream stopped in piece %d, at byte %d of the channel)%s",
					who, id, published[id].Seconds(), took.Seconds(), brRoughPieceOf(max(rd.pos-1, 0)), rd.pos, where())
				break
			}
			if late := at - max(published[id], rd.started); late > worst {
				worst, worstOf = late, id
			}
		}
		allowed := allowedFor(rd.spec)
		t.Logf("%s: a piece reached it at most %.1f s after the provider published it (piece %d); the provider's own delays and the reader's own pause come to %.1f s",
			who, worst.Seconds(), worstOf, (allowed - brRoughLate).Seconds())
		if worst > allowed {
			t.Errorf("video was held: piece %d reached %s %.1f s after the provider published it; at most %.1f s is allowed (8 s, and %.1f s of the provider's and the reader's own)%s",
				worstOf, who, worst.Seconds(), allowed.Seconds(), (allowed - brRoughLate).Seconds(), where())
		}
		rd.mu.Unlock()
	}
	// What was asked of the provider.
	time.Sleep(300 * time.Millisecond) // (an ask under way when the last reader hung up)
	before := prov.asks()
	prov.mu.Lock()
	opens, reloads, stale := prov.opens, prov.reloads, prov.stale
	asked := map[int]int{}
	for id, n := range prov.asked {
		asked[id] = n
	}
	prov.mu.Unlock()
	t.Logf("in %.1f s the provider was asked %s", took.Seconds(), before)
	if opens != 1 || stale != 0 {
		t.Errorf("the channel was opened at the provider's own address %d times for %d reader(s) of one channel, and a session already replaced was asked %d times; want 1 and 0 (every open is a new session at the provider, and ends the one before)",
			opens, len(specs), stale)
	}
	// An HLS player asks for the playlist again one target duration after a playlist that changed and half of
	// one after a playlist that did not (RFC 8216, 6.3.4): at most twice per target duration, here 1 s.
	most := int(took.Seconds()*2) + 4
	if sc.player != nil {
		most += 4 // (the other device's own asks)
	}
	if reloads > most {
		t.Errorf("the playlist was asked for %d times in %.1f s; a player asks at most twice per target duration (1 s here), which is %d", reloads, took.Seconds(), most)
	}
	for id := 1; id <= brRoughLast; id++ {
		how := sc.pieces[id]
		want := 1 + how.gone
		switch {
		case how.never:
			want = 3 // (asked for, and twice more a second apart, as a player retries; then the stream goes on)
		case how.moved:
			want++ // (where the playlist names it, then where it was sent on to)
		}
		if sc.player != nil {
			want++ // (the other device may ask for it too)
		}
		if asked[id] > want {
			t.Errorf("piece %d was asked of the provider %d times, want at most %d: a piece is asked for once, and again only when it was not there", id, asked[id], want)
		}
		if id > lowest && asked[id] == 0 {
			t.Errorf("piece %d was never asked of the provider", id)
		}
	}
	time.Sleep(2500 * time.Millisecond) // (the last reader has hung up: nothing more may be asked)
	if after := prov.asks(); after != before {
		t.Errorf("after the last reader hung up the provider was asked more:\n    before: %s\n    after:  %s", before, after)
	}
}

func TestBridgeRoughProviderLosesNoVideo(t *testing.T) {
	ms := time.Millisecond
	scenes := []*brRoughScene{
		{name: "a steady channel"},
		{name: "a reload answered late", listWait: map[int]time.Duration{4: 3000 * ms}},
		{name: "the same playlist again and again, then five pieces at once", top: func(t float64) int {
			if t >= 4 && t < 9 {
				return brRoughWindow + 4
			}
			return brRoughWindow + int(t)
		}},
		{name: "the provider stalls for 10 s, then carries on", top: func(t float64) int {
			switch {
			case t < 3:
				return brRoughWindow + int(t)
			case t < 13:
				return brRoughWindow + 3
			}
			return brRoughWindow + 3 + int(t-13)
		}},
		{name: "a piece that is not there the first time", pieces: map[int]brRoughPiece{12: {gone: 1}}},
		{name: "a piece that never comes", pieces: map[int]brRoughPiece{12: {never: true}}},
		{name: "a piece that comes slowly", pieces: map[int]brRoughPiece{13: {slow: 1200 * ms}}},
		{name: "numbering that leaps ahead", seqOf: func(id int) int64 {
			if id < 6 {
				return int64(100 + id)
			}
			return int64(5000 + id)
		}},
		{name: "numbering that starts afresh", seqOf: func(id int) int64 {
			if id < 6 {
				return int64(100 + id)
			}
			return int64(id - 6)
		}},
		{name: "the playlist and a piece move to another server", movedFrom: 6, pieces: map[int]brRoughPiece{10: {moved: true}}},
		{name: "two readers on the channel", readers: []brRoughReader{{dev: "192.0.2.9"}, {dev: "192.0.2.11", joinAt: 0.3}}},
		{name: "a reader joins in the middle", readers: []brRoughReader{{dev: "192.0.2.9"}, {dev: "192.0.2.11", joinAt: 5}}},
		{name: "a reader stops reading for 4 s", readers: []brRoughReader{{dev: "192.0.2.9", pauseAt: 4, pauseFor: 4}, {dev: "192.0.2.11", joinAt: 0.3}}},
		{name: "another device watches the channel as HLS meanwhile"},
	}
	withPlayer := scenes[len(scenes)-1]
	withPlayer.player = func(t *testing.T, base string) {
		time.Sleep(3 * time.Second)
		for i := 0; i < 3; i++ {
			st, list, err := devGet(base, "192.0.2.10", fmt.Sprintf("/live/tv/easy/%d.m3u8", withPlayer.ch), 10*time.Second)
			if err != nil || st != 200 || !strings.Contains(string(list), "#EXTINF") {
				t.Errorf("the other device's playlist: %d %v %q", st, err, list)
				return
			}
			if st, data, err := devGet(base, "192.0.2.10", pieceOf(t, list), 10*time.Second); err != nil || st != 200 || len(data) == 0 || len(data)%188 != 0 {
				t.Errorf("the other device's piece: %d, %d bytes, %v", st, len(data), err)
			}
			time.Sleep(time.Second)
		}
	}
	log := captureLog(t)
	var ran sync.Map // the scenes that ran (go test -run may pick some)
	t.Run("scenes", func(t *testing.T) {
		for i, sc := range scenes {
			sc.ch = 4400 + i
			t.Run(sc.name, func(t *testing.T) {
				t.Parallel()
				ran.Store(sc.ch, true)
				brRoughRecord(t, sc)
			})
		}
	})
	// A piece that never came is said so in the log, for its channel; no other channel lost one.
	text := log.String()
	for _, sc := range scenes {
		if _, ok := ran.Load(sc.ch); !ok {
			continue
		}
		lost := linesWith(text, "a piece of video could not be fetched and is missing from a stream", fmt.Sprintf("channel=%d", sc.ch))
		want := 0
		for _, how := range sc.pieces {
			if how.never {
				want++
			}
		}
		if len(lost) != want {
			t.Errorf("%s: the provider never had %d piece(s); the log says a piece is missing from the stream %d time(s):\n%s", sc.name, want, len(lost), strings.Join(lost, "\n"))
		}
	}
}
