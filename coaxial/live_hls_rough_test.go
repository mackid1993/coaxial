package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Live HLS as a player watches it on a provider whose players use HLS, fed the rough things providers do with
// their playlists and pieces: a reload answered late, the same playlist again and again, a piece that is not
// there the first time it is asked for, a piece that comes slowly, numbering that leaps ahead and starts over,
// a playlist and a piece that move to another server in the middle of the channel, and a provider that answers
// the playlist itself. live_rough_test.go holds live MPEG-TS to "the same bytes, as they come"; this holds live
// HLS to the same, in the terms HLS has. For every one of them:
//
//   1. every piece the provider's playlists named can be had through the proxy, and is byte for byte what the
//      provider sent; every playlist the player is given names the same pieces, in the same order, as the
//      provider's own playlist for that reload;
//   2. every address the player is given, worked out the way a player works it out (from the address it was
//      last answered at), leads to the proxy and nowhere else, even when the player first came by way of
//      another server that sent it here;
//   3. nothing is held: an answer takes no longer through the proxy than the provider itself took, give or take
//      hlsRoughHeld (AGENTS.md rules 2 and 14: a player gives up after 8 s, and waits for whatever the proxy
//      waits for);
//   4. the provider is asked no more often than the player asked: the channel is opened once, each reload is
//      one ask for the playlist, each piece is asked for as often as the player asked for it, and once the
//      player stops, nothing more is asked (AGENTS.md rule 1).
//
// The player here is a simple one: it asks for the playlist, then for each piece it has not had yet, one after
// the other, and asks once more for a piece that was not there. It never asks the provider anything itself.

// hlsRoughHeld: how much longer than the provider itself took an answer may take through the proxy.
const hlsRoughHeld = 2 * time.Second

// hlsRoughList: what the provider's playlist says at one ask for it.
type hlsRoughList struct {
	seq   int64         // its MEDIA-SEQUENCE
	ids   []int         // the pieces it names, by what is in them
	wait  time.Duration // how long the server takes to answer this ask
	moved bool          // the playlist is on the other server by now: the first one sends the player on
}

// hlsRoughPiece: how one piece is served (the zero value: whole, at once).
type hlsRoughPiece struct {
	gone  int           // the first asks for it are answered 404
	slow  time.Duration // it is sent in three parts, this long apart
	moved bool          // it is on the other server: the first one sends the player on
	big   bool          // half a megabyte, not 20 kB
}

// hlsRoughProv: a provider with one live channel as HLS. Its own address sends the player on to a video
// server (own: it answers the playlist itself instead). The n-th ask for the playlist is answered with
// lists[n] (the last one from then on), so a playlist asked for more often than the player asked shows.
type hlsRoughProv struct {
	front, video, other *fakeProv
	own                 bool
	lists               []hlsRoughList
	pieces              map[int]hlsRoughPiece

	mu        sync.Mutex
	opens     int               // asks at the channel's own address
	reloads   int               // asks for the playlist that were answered with it
	sentOn    int               // asks for the playlist that were sent on to the other server
	asked     map[int]int       // asks for each piece, on either server
	held      time.Duration     // the time the provider itself took over its answers, in all
	firstSent map[int]time.Time // when the first bytes of a piece were sent
}

var hlsRoughName = regexp.MustCompile(`/p([0-9]+)\.ts`)

// hlsRoughData: what is in piece id: MPEG-TS packets with no programme in them, each one different from every
// other packet of every piece.
func hlsRoughData(id int, big bool) []byte {
	n := 120
	if big {
		n = 2800
	}
	out := make([]byte, 0, n*188)
	for i := 0; i < n; i++ {
		p := make([]byte, 188)
		p[0], p[1], p[2], p[3] = 0x47, 0x1f, 0xff, 0x10|byte(i&0x0f)
		binary.BigEndian.PutUint32(p[4:], uint32(id))
		binary.BigEndian.PutUint32(p[8:], uint32(i))
		for k := 12; k < 188; k++ {
			p[k] = byte(id*131 + i*31 + k*7)
		}
		out = append(out, p...)
	}
	return out
}

func newHLSRoughProv(t *testing.T, own bool, lists []hlsRoughList, pieces map[int]hlsRoughPiece) *hlsRoughProv {
	p := &hlsRoughProv{own: own, lists: lists, pieces: pieces, asked: map[int]int{}, firstSent: map[int]time.Time{}}
	hold := func(r *http.Request, d time.Duration) bool { // the provider takes its time; false: the reader hung up
		if d <= 0 {
			return true
		}
		p.mu.Lock()
		p.held += d
		p.mu.Unlock()
		select {
		case <-time.After(d):
			return true
		case <-r.Context().Done():
			return false
		}
	}
	// The playlist, as the server at `here` writes it: its pieces named the three ways playlists name them (next
	// to the playlist with a token, by path, and in full).
	list := func(w http.ResponseWriter, r *http.Request, dir string) {
		p.mu.Lock()
		l := p.lists[min(p.reloads, len(p.lists)-1)]
		p.reloads++
		p.mu.Unlock()
		if !hold(r, l.wait) {
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:%d\n", l.seq)
		for _, id := range l.ids {
			b.WriteString("#EXTINF:4.000,\n")
			switch {
			case id%3 == 0 && !p.own:
				fmt.Fprintf(&b, "p%d.ts?token=t%d\n", id, id)
			case id%3 == 1 || p.own:
				fmt.Fprintf(&b, "%sp%d.ts\n", dir, id)
			default:
				fmt.Fprintf(&b, "http://%s%sp%d.ts\n", r.Host, dir, id)
			}
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, b.String())
	}
	piece := func(w http.ResponseWriter, r *http.Request, onOther bool) bool {
		m := hlsRoughName.FindStringSubmatch(r.URL.Path)
		if m == nil {
			return false
		}
		id, _ := strconv.Atoi(m[1])
		how := p.pieces[id]
		p.mu.Lock()
		p.asked[id]++
		n := p.asked[id]
		p.mu.Unlock()
		switch {
		case n <= how.gone:
			http.NotFound(w, r)
			return true
		case how.moved && !onOther:
			http.Redirect(w, r, p.other.srv.URL+"/moved/p"+m[1]+".ts", 302)
			return true
		}
		data := hlsRoughData(id, how.big)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		for part := 0; part < 3; part++ {
			if part > 0 && !hold(r, how.slow) {
				return true
			}
			w.Write(data[part*len(data)/3 : (part+1)*len(data)/3])
			w.(http.Flusher).Flush()
			if part == 0 {
				p.mu.Lock()
				p.firstSent[id] = time.Now()
				p.mu.Unlock()
			}
		}
		return true
	}
	p.video = newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/hlsr/tok1/index.m3u8" {
			return piece(w, r, false)
		}
		p.mu.Lock()
		moved := p.lists[min(p.reloads, len(p.lists)-1)].moved
		if moved {
			p.sentOn++
		}
		p.mu.Unlock()
		if moved {
			http.Redirect(w, r, p.other.srv.URL+"/moved/index.m3u8", 302)
		} else {
			list(w, r, "/hlsr/tok1/")
		}
		return true
	})
	p.other = newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/moved/index.m3u8" {
			return piece(w, r, true)
		}
		list(w, r, "/moved/")
		return true
	})
	p.front = newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/live/realU/realP/44.m3u8" {
			return p.own && piece(w, r, false)
		}
		p.mu.Lock()
		p.opens++
		p.mu.Unlock()
		if p.own { // (its pieces are on its own server, under the account's own folder)
			list(w, r, "/hlsown/realU/realP/44/")
		} else {
			http.Redirect(w, r, p.video.srv.URL+"/hlsr/tok1/index.m3u8", 302)
		}
		return true
	})
	return p
}

// longest: the longest the provider itself takes over one answer.
func (p *hlsRoughProv) longest() (d time.Duration) {
	for _, l := range p.lists {
		d = max(d, l.wait)
	}
	for _, how := range p.pieces {
		d = max(d, 2*how.slow)
	}
	return d
}

func (p *hlsRoughProv) heldSoFar() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.held
}

// asks: everything asked of the provider so far, in one line.
func (p *hlsRoughProv) asks() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	pieces := 0
	for _, n := range p.asked {
		pieces += n
	}
	return fmt.Sprintf("the channel's own address %d times, the playlist %d times (sent on to the other server %d times), pieces %d times",
		p.opens, p.reloads, p.sentOn, pieces)
}

// hlsRoughPlayer: a simple player. It asks only the proxy (and, to begin with, the server that sends it there).
type hlsRoughPlayer struct {
	t       *testing.T
	prov    *hlsRoughProv
	proxy   string // the proxy's host and port
	at      string // the address its playlist was answered at
	reloads int
	asked   map[int]int  // its asks for each piece
	have    map[int]bool // the pieces it has
	worst   time.Duration
	worstOf string
}

var hlsRoughClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// get: one ask, the way a player makes it: an answer that sends it elsewhere is followed, the new address
// worked out from the address just asked. Returns the last answer, the address it came from and when its first
// bytes came. An address that is not the proxy's fails the test: nothing but the proxy is ever asked.
func (pl *hlsRoughPlayer) get(what, addr string, first bool) (status int, data []byte, at string, began time.Time) {
	pl.t.Helper()
	limit := hlsRoughHeld + pl.prov.longest()
	done := within(pl.t, limit, what)
	defer done()
	heldBefore, t0 := pl.prov.heldSoFar(), time.Now()
	for hop := 0; hop < 8; hop++ {
		u, err := url.Parse(addr)
		if err != nil {
			pl.t.Fatalf("%s: the player was given an address it cannot read: %q", what, addr)
		}
		if u.Host != pl.proxy && !(first && hop == 0) {
			pl.t.Fatalf("%s: the player was given the address %s, which is on %s, not on the proxy (%s): asked that way, it would leave the proxy",
				what, hlsRoughShown(addr), u.Host, pl.proxy)
		}
		if m := hlsRoughName.FindStringSubmatch(u.Path); m != nil && u.Host == pl.proxy {
			id, _ := strconv.Atoi(m[1])
			pl.asked[id]++
		}
		req, _ := http.NewRequest("GET", addr, nil)
		req.Header.Set("X-Dev", "192.0.2.61")
		resp, err := hlsRoughClient.Do(req)
		if err != nil {
			pl.t.Fatalf("%s: no answer in 30 s (%v)", what, err)
		}
		if loc := resp.Header.Get("Location"); isRedirect(resp.StatusCode) && loc != "" {
			resp.Body.Close()
			next, err := u.Parse(loc)
			if err != nil {
				pl.t.Fatalf("%s: sent on to an address the player cannot read: %q", what, loc)
			}
			addr = next.String()
			continue
		}
		one := make([]byte, 1)
		if n, _ := io.ReadFull(resp.Body, one); n == 1 {
			began = time.Now()
			rest, err := io.ReadAll(resp.Body)
			if err != nil {
				pl.t.Fatalf("%s: the answer broke off after %d bytes (%v)", what, 1+len(rest), err)
			}
			data = append(one, rest...)
		}
		resp.Body.Close()
		// What the proxy added: the time the answer took, less the time the provider itself took over it.
		if added := time.Since(t0) - (pl.prov.heldSoFar() - heldBefore); added > pl.worst {
			pl.worst, pl.worstOf = added, what
		}
		return resp.StatusCode, data, addr, began
	}
	pl.t.Fatalf("%s: the player was sent from one address to the next 8 times and never answered", what)
	return
}

// hlsRoughShown: an address for a failure message, cut short.
func hlsRoughShown(a string) string {
	if len(a) > 140 {
		return a[:140] + "..."
	}
	return a
}

// hlsRoughRead: a playlist as the player was given it: its MEDIA-SEQUENCE and its pieces' addresses as written.
func hlsRoughRead(text string) (seq int64, names []string) {
	seq = -1
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"); ok {
			seq, _ = strconv.ParseInt(v, 10, 64)
		} else if line != "" && !strings.HasPrefix(line, "#") {
			names = append(names, line)
		}
	}
	return seq, names
}

// hlsRoughWatch: the player tunes the channel by way of another server that sends it to the proxy, then for
// each playlist the provider has: reads it, fetches every piece it names that it does not have yet, and asks
// for the playlist again. Then the four rules at the top of this file are checked.
func hlsRoughWatch(t *testing.T, prov *hlsRoughProv) {
	_, base := limFront(t, prov.front, "hls", "", advanced{})
	guide := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, base+r.URL.RequestURI(), 302) // (another server that sends every stream on to the proxy)
	}))
	defer guide.Close()
	pu, _ := url.Parse(base)
	pl := &hlsRoughPlayer{t: t, prov: prov, proxy: pu.Host, asked: map[int]int{}, have: map[int]bool{}}
	wantAsks := map[int]int{}
	var lastSeq, lastWant int64
	for k, want := range prov.lists {
		what := fmt.Sprintf("playlist %d", k+1)
		addr := pl.at
		if k == 0 {
			addr = guide.URL + "/live/tv/easy/44.m3u8"
		}
		status, data, at, _ := pl.get(what, addr, k == 0)
		pl.at, pl.reloads = at, pl.reloads+1
		text := string(data)
		if status != 200 || !strings.Contains(text, "#EXTM3U") {
			t.Fatalf("%s: the player was answered %d %q; the provider answered with a playlist", what, status, hlsRoughShown(text))
		}
		if strings.Contains(text, "realU") || strings.Contains(text, "realP") {
			t.Errorf("%s carries the provider's own login: %q", what, text)
		}
		seq, names := hlsRoughRead(text)
		var got []int
		for _, name := range names {
			if m := hlsRoughName.FindStringSubmatch(name); m != nil {
				id, _ := strconv.Atoi(m[1])
				got = append(got, id)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want.ids) || len(got) != len(names) {
			t.Fatalf("%s: the provider's playlist for this reload names pieces %v; the one the player was given names %v\n%s\n(a later playlist than the provider's own for this reload means the provider was asked more often than the player asked)",
				what, want.ids, got, text)
		}
		if n := strings.Count(text, "#EXTINF:4.000,"); n != len(want.ids) || !strings.Contains(text, "#EXT-X-TARGETDURATION:4") {
			t.Errorf("%s: the pieces' lengths or the target duration are not as the provider wrote them:\n%s", what, text)
		}
		// Where the provider's own numbering moves on in the usual way, the player's must move on by as much.
		if step := want.seq - lastWant; k > 0 && step >= 0 && step <= int64(len(want.ids)) && seq-lastSeq != step {
			t.Errorf("%s: the provider's numbering moved on by %d (from %d to %d); the player's moved by %d (from %d to %d)",
				what, step, lastWant, want.seq, seq-lastSeq, lastSeq, seq)
		}
		lastSeq, lastWant = seq, want.seq
		listAt, _ := url.Parse(pl.at)
		for i, name := range names {
			id := got[i]
			if pl.have[id] {
				continue
			}
			u, err := listAt.Parse(name)
			if err != nil {
				t.Fatalf("%s names piece %d at an address the player cannot read: %q", what, id, name)
			}
			how := prov.pieces[id]
			wantAsks[id] = 1 + how.gone
			if how.moved {
				wantAsks[id]++ // (asked for where the playlist names it, then where it was sent on to)
			}
			what := fmt.Sprintf("piece %d", id)
			var status int
			var data []byte
			var began time.Time
			for try := 0; try <= how.gone; try++ { // (a piece that was not there is asked for once more)
				status, data, _, began = pl.get(what, u.String(), false)
				if status != 404 {
					break
				}
			}
			sent := hlsRoughData(id, how.big)
			if status != 200 {
				t.Fatalf("%s, which the provider's playlist named and the provider has: the player was answered %d at %s", what, status, hlsRoughShown(u.String()))
			}
			if !bytes.Equal(data, sent) {
				t.Fatalf("%s is not what the provider sent (%d bytes sent, %d received): %s", what, len(sent), len(data), diffAt(sent, data))
			}
			prov.mu.Lock()
			first := prov.firstSent[id]
			prov.mu.Unlock()
			if late := began.Sub(first); how.slow > 0 && late > hlsRoughHeld {
				t.Errorf("%s came slowly from the provider, and its first part was held: it reached the player %d ms after the provider sent it; at most %d ms is allowed",
					what, late.Milliseconds(), hlsRoughHeld.Milliseconds())
			}
			pl.have[id] = true
		}
	}
	t.Logf("%d playlists and %d pieces; the proxy added at most %d ms to an answer (%s)", pl.reloads, len(pl.have), pl.worst.Milliseconds(), pl.worstOf)
	if pl.worst > hlsRoughHeld {
		t.Errorf("video was held: %s took %d ms longer through the proxy than the provider itself took; at most %d ms is allowed",
			pl.worstOf, pl.worst.Milliseconds(), hlsRoughHeld.Milliseconds())
	}
	// What was asked of the provider, against what the player asked.
	before := prov.asks()
	prov.mu.Lock()
	opens, reloads, sentOn := prov.opens, prov.reloads, prov.sentOn
	asked := map[int]int{}
	for id, n := range prov.asked {
		asked[id] = n
	}
	prov.mu.Unlock()
	if prov.own {
		if opens != pl.reloads {
			t.Errorf("the player asked for the playlist %d times; the provider's own address, which answers it, was asked %d times", pl.reloads, opens)
		}
	} else if opens != 1 {
		t.Errorf("the channel was opened at the provider's own address %d times while one player watched it without a break, want 1 (every open is a new session at the provider)", opens)
	}
	if reloads != pl.reloads {
		t.Errorf("the player asked for the playlist %d times; the provider was asked for it %d times", pl.reloads, reloads)
	}
	if sentOn > pl.reloads {
		t.Errorf("the player asked for the playlist %d times; the server it moved away from was asked %d times", pl.reloads, sentOn)
	}
	for id := range pl.have {
		if pl.asked[id] != wantAsks[id] {
			t.Errorf("piece %d: the player had to ask %d times, want %d", id, pl.asked[id], wantAsks[id])
		}
		if asked[id] != wantAsks[id] {
			t.Errorf("piece %d: the player asked for it %d times; the provider was asked for it %d times", id, wantAsks[id], asked[id])
		}
	}
	for id, n := range asked {
		if !pl.have[id] {
			t.Errorf("piece %d was asked of the provider %d times; the player never asked for it", id, n)
		}
	}
	time.Sleep(1200 * time.Millisecond) // (the player has stopped asking: so must the proxy)
	if after := prov.asks(); after != before {
		t.Errorf("after the player's last ask the provider was asked more:\n    before: %s\n    after:  %s", before, after)
	}
}

func TestLiveHLSRoughProviderPassesStraightThrough(t *testing.T) {
	ms := time.Millisecond
	// steady: a playlist of three pieces that moves on by one piece at each reload.
	steady := func(n int) (lists []hlsRoughList) {
		for k := 0; k < n; k++ {
			lists = append(lists, hlsRoughList{seq: int64(100 + k), ids: []int{k + 1, k + 2, k + 3}})
		}
		return lists
	}
	late := steady(5)
	late[2].wait = 3000 * ms

	again := []hlsRoughList{
		{seq: 100, ids: []int{1, 2, 3}}, {seq: 100, ids: []int{1, 2, 3}}, {seq: 100, ids: []int{1, 2, 3}},
		{seq: 100, ids: []int{1, 2, 3}}, {seq: 103, ids: []int{4, 5, 6}}, {seq: 104, ids: []int{5, 6, 7}},
	}

	jumps := []hlsRoughList{
		{seq: 100, ids: []int{1, 2, 3}}, {seq: 101, ids: []int{2, 3, 4}},
		{seq: 5000, ids: []int{5, 6, 7}}, {seq: 5001, ids: []int{6, 7, 8}}, // (it leaps ahead)
		{seq: 0, ids: []int{9, 10, 11}}, {seq: 1, ids: []int{10, 11, 12}}, // (and starts over)
		{seq: 3, ids: []int{12, 13, 14}}, // (and leaves one out: piece 11 to 12 is one step, the number two)
	}

	moves := steady(6)
	for k := 3; k < len(moves); k++ {
		moves[k].moved = true
	}

	cases := []struct {
		name   string
		own    bool
		lists  []hlsRoughList
		pieces map[int]hlsRoughPiece
	}{
		{name: "a steady channel", lists: steady(6)},
		{name: "a reload answered late", lists: late},
		{name: "the same playlist again and again", lists: again},
		{name: "a piece that is not there the first time", lists: steady(5), pieces: map[int]hlsRoughPiece{4: {gone: 1}}},
		{name: "a piece that comes slowly", lists: steady(4), pieces: map[int]hlsRoughPiece{3: {slow: 1500 * ms, big: true}, 5: {big: true}}},
		{name: "numbering that leaps ahead and starts over", lists: jumps},
		{name: "the playlist and a piece move to another server", lists: moves, pieces: map[int]hlsRoughPiece{2: {moved: true}}},
		{name: "a provider that answers the playlist itself", own: true, lists: steady(5), pieces: map[int]hlsRoughPiece{3: {gone: 1}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hlsRoughWatch(t, newHLSRoughProv(t, c.own, c.lists, c.pieces))
		})
	}
}
