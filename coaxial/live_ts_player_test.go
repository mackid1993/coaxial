package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Live MPEG-TS as players other than the owner's ask for it and as providers other than the owner's answer.
// Most people who run this proxy will watch live TV as MPEG-TS, on players built on ExoPlayer, FFmpeg or VLC,
// from providers whose servers are set up in every way there is. The harsh tests of this path (live_rough,
// live_wreck) hold the bytes and the timing of a channel that plays; these hold the first seconds of the
// conversation, where a channel is asked for, in which form, with which headers, and what comes back when the
// provider says no or says yes in an unusual way.

// livePlayerProv: a provider that notes every ask and answers live channels by how.
type livePlayerProv struct {
	fp   *fakeProv
	base string
	how  func(id string, n int, w http.ResponseWriter, r *http.Request) bool // n: which ask for this channel, from 1 (false: video as usual)

	mu   sync.Mutex
	asks map[string][]*http.Request // by channel
}

func newLivePlayerProv(t *testing.T, connections number) *livePlayerProv {
	p := &livePlayerProv{asks: map[string][]*http.Request{}}
	p.fp = newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		id := channelID(r.URL.Path)
		if id == "?" || strings.Contains(r.URL.Path, "/movie/") {
			return false
		}
		p.mu.Lock()
		p.asks[id] = append(p.asks[id], r)
		n := len(p.asks[id])
		p.mu.Unlock()
		if p.how != nil && p.how(id, n, w, r) {
			return true
		}
		fp.stream(w, r, "ch"+id, true, 0)
		return true
	})
	_, p.base = limFront(t, p.fp, "ts", connections, advanced{})
	return p
}

func (p *livePlayerProv) asked(id string) []*http.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*http.Request(nil), p.asks[id]...)
}

// livePlayerPaths: what the provider was asked, for a failure message.
func livePlayerPaths(asked []*http.Request) (out []string) {
	for _, r := range asked {
		out = append(out, r.URL.RequestURI()+" (User-Agent "+fmt.Sprintf("%q", r.Header.Get("User-Agent"))+")")
	}
	return
}

// The provider refuses a live channel at its first open (the line is not paid for, the channel is gone, the
// account is at its limit, its server is in trouble). Straight at the provider a player is told so at once,
// shows it, and asks again when its user does. Through the proxy it must be the same: the provider's own
// status and page (with the login in it swapped for the player's own), at once, the provider asked once for
// each ask of the player and never again by the proxy itself (AGENTS.md rule 1), and nothing kept of the
// refusal: the player's next ask is put to the provider afresh.
func TestLiveTSRefusedAtFirstOpen(t *testing.T) {
	p := newLivePlayerProv(t, "")
	var ok sync.Map // channels the provider serves from now on
	p.how = func(id string, n int, w http.ResponseWriter, r *http.Request) bool {
		if _, plays := ok.Load(id); plays {
			return false
		}
		status := 400 + atoi(id)%200
		if id == "700" { // answered 200, and then no video at all
			w.Header().Set("Content-Type", "video/mp2t")
			w.WriteHeader(200)
			return true
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<h1>%d</h1><p>%s cannot be played now.</p>", status, r.URL.Path)
		return true
	}
	for _, id := range []string{"401", "403", "404", "406", "429", "500", "502", "503", "509", "520", "700"} {
		t.Run("answered "+id, func(t *testing.T) {
			t.Parallel()
			want := 400 + atoi(id)%200
			done := within(t, 3*time.Second, "the refusal")
			began := time.Now()
			r := getAs(t, p.base+"/live/tv/easy/"+id+".ts", "192.0.2.1")
			text := body(r)
			done()
			took := time.Since(began)
			if id == "700" {
				if r.StatusCode < 500 || r.StatusCode > 599 {
					t.Errorf("the provider answered 200 and ended the channel without a byte of video; the player was answered %d, want a server error it can show (a 200 with nothing in it reads as a channel that played and ended)", r.StatusCode)
				}
			} else {
				if r.StatusCode != want {
					t.Errorf("the provider refused the channel with %d; the player was answered %d", want, r.StatusCode)
				}
				if !strings.Contains(text, "/live/tv/easy/"+id+".ts cannot be played now") || strings.Contains(text, "realU") || strings.Contains(text, "realP") {
					t.Errorf("the provider's own page did not reach the player with its own login in it: %q", text)
				}
			}
			if took > 3*time.Second {
				t.Errorf("the player was told after %.1f s; the provider answered at once", took.Seconds())
			}
			if n := len(p.asked(id)); n != 1 {
				t.Errorf("the provider was asked %d times for one ask of the player, want 1", n)
			}
			time.Sleep(1500 * time.Millisecond)
			if n := len(p.asked(id)); n != 1 {
				t.Errorf("%d asks at the provider 1.5 s after the player was told no; the proxy asks nothing on its own", n)
			}
			// The provider plays the channel again, and the player's user tries once more.
			ok.Store(id, true)
			r = getAs(t, p.base+"/live/tv/easy/"+id+".ts", "192.0.2.1")
			defer r.Body.Close()
			if n, _ := io.ReadFull(r.Body, make([]byte, 64<<10)); r.StatusCode != 200 || n != 64<<10 {
				t.Errorf("asked for again once the provider plays it: %d, %d bytes of video; the refusal from before must not be kept", r.StatusCode, n)
			}
			if n := len(p.asked(id)); n != 2 {
				t.Errorf("the provider was asked %d times in all for two asks of the player, want 2", n)
			}
		})
	}
}

// The forms a live channel's address takes, and what players send with it. One channel is one connection at
// the provider whichever form each device used; the provider is asked in the form and with the query the
// first one used, with that player's own headers and nothing that tells of a proxy; a player built on FFmpeg
// or VLC, which sends "Range: bytes=0-" with everything, is answered 200 and the Range is not put to the
// provider (a live channel has no bytes to count from); a HEAD is answered without asking the provider
// anything (it would cost the account a connection); an old player speaking HTTP/1.0 gets the channel too.
func TestLiveTSAddressFormsAndPlayerHabits(t *testing.T) {
	p := newLivePlayerProv(t, "")
	open := func(dev, path string, hdr ...string) *http.Response {
		t.Helper()
		r := ask(t, p.base, dev, path, hdr...)
		t.Cleanup(func() { r.Body.Close() })
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "video/") {
			t.Fatalf("%s asked for %s: %d %q", dev, path, r.StatusCode, r.Header.Get("Content-Type"))
		}
		return r
	}
	// Three devices, three forms, one channel.
	open("192.0.2.1", "/live/tv/easy/21.ts")
	open("192.0.2.2", "/tv/easy/21.ts")
	open("192.0.2.3", "/tv/easy/21")
	if asked := p.asked("21"); len(asked) != 1 || asked[0].URL.Path != "/live/realU/realP/21.ts" {
		t.Errorf("three devices on channel 21, each by another form of its address: the provider was asked %d times, want once, at /live/realU/realP/21.ts", len(asked))
	}
	if n := p.fp.count(p.fp.most, "ch21"); n != 1 {
		t.Errorf("the provider had %d connections open for channel 21, want 1", n)
	}
	// Each short form alone reaches the provider as written, with the provider's login.
	for id, form := range map[string]string{"25": "/tv/easy/25", "26": "/tv/easy/26.ts"} {
		open("192.0.2.4", form)
		want := strings.Replace(form, "/tv/easy/", "/realU/realP/", 1)
		if asked := p.asked(id); len(asked) != 1 || asked[0].URL.Path != want {
			t.Errorf("%s: the provider was asked %v; want once, at %s", form, livePlayerPaths(asked), want)
		}
	}
	// A query goes with it, exactly as written.
	open("192.0.2.5", "/live/tv/easy/22.ts?token=x%2Fy&play_token=a+b")
	if asked := p.asked("22"); len(asked) != 1 || asked[0].URL.RawQuery != "token=x%2Fy&play_token=a+b" {
		t.Errorf("a channel asked for with a query: the provider was asked %v; want once, with the query as the player wrote it", livePlayerPaths(asked))
	}
	// A player built on FFmpeg or VLC.
	const agent = "VLC/3.0.20 LibVLC/3.0.20"
	r := open("192.0.2.6", "/live/tv/easy/23.ts", "Range", "bytes=0-", "Icy-MetaData", "1", "User-Agent", agent, "Accept", "*/*")
	if r.Header.Get("Content-Range") != "" || r.ContentLength != -1 {
		t.Errorf("a live channel asked for with Range bytes=0- was answered with Content-Range %q and a length of %d: a live channel has neither", r.Header.Get("Content-Range"), r.ContentLength)
	}
	if asked := p.asked("23"); len(asked) != 1 {
		t.Errorf("the provider was asked %d times, want 1", len(asked))
	} else {
		h := asked[0].Header
		if h.Get("Range") != "" {
			t.Errorf("the player's Range (%q) was put to the provider for a live channel", h.Get("Range"))
		}
		if h.Get("User-Agent") != agent || h.Get("Icy-Metadata") != "1" || h.Get("Accept") != "*/*" {
			t.Errorf("the provider was not sent the player's own headers: User-Agent %q, Icy-MetaData %q, Accept %q", h.Get("User-Agent"), h.Get("Icy-Metadata"), h.Get("Accept"))
		}
		for _, k := range []string{"Via", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded"} {
			if h.Get(k) != "" {
				t.Errorf("the provider was told of a proxy: %s: %s", k, h.Get(k))
			}
		}
	}
	// A player that sends no User-Agent at all: none is made up for it.
	open("192.0.2.7", "/live/tv/easy/27.ts", "User-Agent", "")
	if asked := p.asked("27"); len(asked) != 1 || asked[0].Header.Get("User-Agent") != "" {
		t.Errorf("a player with no User-Agent: the provider was asked %v; want once, with none (never this program's own)", livePlayerPaths(asked))
	}
	// HEAD and other methods: nothing is asked of the provider.
	for _, m := range []string{"HEAD", "POST"} {
		req, _ := http.NewRequest(m, p.base+"/live/tv/easy/28.ts", nil)
		req.Header.Set("X-Dev", "192.0.2.8")
		resp, err := devClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if m == "HEAD" && (resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "video/")) {
			t.Errorf("HEAD for a live channel: %d %q, want 200 and a video type (players that look before they play take anything else for a dead channel)", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	time.Sleep(300 * time.Millisecond)
	if asked := p.asked("28"); len(asked) != 0 {
		t.Errorf("a HEAD and a POST for a live channel had the provider asked %d times; either would cost the account a connection", len(asked))
	}
	// HTTP/1.0.
	c, err := net.Dial("tcp", strings.TrimPrefix(p.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /live/tv/easy/29.ts HTTP/1.0\r\nX-Dev: 192.0.2.9\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	line, _ := br.ReadString('\n')
	if !strings.Contains(line, " 200 ") {
		t.Fatalf("an HTTP/1.0 player asked for a channel: %q", line)
	}
	for { // (the headers)
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("an HTTP/1.0 player: the answer broke off in its headers: %v", err)
		}
		if strings.HasPrefix(strings.ToLower(l), "transfer-encoding") {
			t.Errorf("an HTTP/1.0 player was answered with %q, which it cannot read", strings.TrimSpace(l))
		}
		if l == "\r\n" {
			break
		}
	}
	video := make([]byte, 188*100)
	if _, err := io.ReadFull(br, video); err != nil || video[0] != 0x47 || video[188] != 0x47 {
		t.Errorf("an HTTP/1.0 player: the video did not follow the headers as it is (%v, first byte %#x)", err, video[0])
	}
}

// Providers answer a live channel in more than one way: with no Content-Type, with a wrong one, as 206 with a
// Content-Range, with a Content-Length that stands for "endless", with cookies and their server's name, or as
// plain HTTP/1.0 that ends when the connection does. A player straight at the provider copes with each (it
// reads what comes). Through the proxy the player must get the same thing every time: 200, a video type, no
// length and no range (either would make it stop at that many bytes), nothing that names the provider, and
// the provider's bytes as they come.
func TestLiveTSProviderAnswersInItsOwnWay(t *testing.T) {
	data := roughBytes(3 << 20)
	lumps := func(w io.Writer, flush func(), gone <-chan struct{}) {
		for off := 0; off < len(data); off += 1 << 20 {
			if _, err := w.Write(data[off : off+1<<20]); err != nil {
				return
			}
			flush()
			select {
			case <-gone:
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		<-gone
	}
	ways := []struct {
		name string
		head func(h http.Header) int // sets the provider's headers, returns its status
	}{
		{"no Content-Type", func(h http.Header) int { h["Content-Type"] = nil; return 200 }},
		{"an octet-stream", func(h http.Header) int { h.Set("Content-Type", "application/octet-stream"); return 200 }},
		{"a web page's type", func(h http.Header) int { h.Set("Content-Type", "text/html; charset=UTF-8"); return 200 }},
		{"another video type", func(h http.Header) int { h.Set("Content-Type", "video/mpeg"); return 200 }},
		{"206 with a Content-Range", func(h http.Header) int {
			h.Set("Content-Type", "video/mp2t")
			h.Set("Content-Range", "bytes 0-99999999999/100000000000")
			h.Set("Accept-Ranges", "bytes")
			return 206
		}},
		{"an endless Content-Length", func(h http.Header) int {
			h.Set("Content-Type", "video/mp2t")
			h.Set("Content-Length", "99999999999")
			return 200
		}},
		{"cookies and the server's name", func(h http.Header) int {
			h.Set("Content-Type", "video/mp2t")
			h.Set("Set-Cookie", "PHPSESSID=abc123; path=/")
			h.Set("Server", "nginx/1.18.0")
			h.Set("X-Powered-By", "PHP/7.4")
			return 200
		}},
	}
	p := newLivePlayerProv(t, "")
	p.how = func(id string, n int, w http.ResponseWriter, r *http.Request) bool {
		way := atoi(id) - 800
		if way < 0 || way >= len(ways) {
			return false
		}
		w.WriteHeader(ways[way].head(w.Header()))
		lumps(w, w.(http.Flusher).Flush, r.Context().Done())
		return true
	}
	check := func(t *testing.T, base, path string) {
		done := within(t, 3*time.Second, "the channel's answer")
		r := getAs(t, base+path, "192.0.2.1")
		done()
		defer r.Body.Close()
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "video/") {
			t.Errorf("the player was answered %d with Content-Type %q; want 200 and a video type, whatever the provider's own answer looks like", r.StatusCode, r.Header.Get("Content-Type"))
		}
		if r.ContentLength != -1 || r.Header.Get("Content-Range") != "" {
			t.Errorf("the player was given a length (%d) or a range (%q) for a live channel: it stops reading there", r.ContentLength, r.Header.Get("Content-Range"))
		}
		for _, k := range []string{"Set-Cookie", "Server", "X-Powered-By"} {
			if r.Header.Get(k) != "" {
				t.Errorf("the player was sent the provider's %s: %s", k, r.Header.Get(k))
			}
		}
		got := make([]byte, len(data))
		began := time.Now()
		if _, err := io.ReadFull(r.Body, got); err != nil || !bytes.Equal(got, data) {
			t.Errorf("the channel's video: %v; %s", err, diffAt(data, got))
		}
		if took := time.Since(began); took > 3*time.Second {
			t.Errorf("3 MB the provider sent within 0.6 s took %.1f s to reach the player", took.Seconds())
		}
	}
	for i, way := range ways {
		t.Run(way.name, func(t *testing.T) {
			t.Parallel()
			check(t, p.base, fmt.Sprintf("/live/tv/easy/%d.ts", 800+i))
		})
	}
	// Plain HTTP/1.0: no length, no chunks, the body ends when the connection does.
	t.Run("HTTP 1.0, ended by closing", func(t *testing.T) {
		t.Parallel()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					br := bufio.NewReader(c)
					for {
						if l, err := br.ReadString('\n'); err != nil || l == "\r\n" {
							break
						}
					}
					io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Type: video/mp2t\r\nServer: old\r\n\r\n")
					gone := make(chan struct{})
					go func() { br.ReadByte(); close(gone) }() // (the reader hung up)
					lumps(c, func() {}, gone)
				}()
			}
		}()
		_, base := limFront(t, &fakeProv{srv: &httptest.Server{URL: "http://" + ln.Addr().String()}}, "ts", "", advanced{})
		check(t, base, "/live/tv/easy/811.ts")
	})
}

// A provider that ends every live connection after a few seconds (a cap on how long one may run, a balancer
// that moves viewers on). A stream that ran under reopenAfter seconds is not asked for again by the proxy, by
// design: the player is told at once, by its stream ending, and asks again itself, as it would straight at
// the provider. So each of the player's asks must be one clean open: answered at once, the provider's bytes
// from that connection's first one, ended as soon as the provider ended it, and the provider asked exactly
// as often as the player asked, one connection at a time.
func TestLiveTSProviderEndsEveryConnectionEarly(t *testing.T) {
	data := roughBytes(188 * 12000)
	var emu sync.Mutex
	var ends []time.Time
	p := newLivePlayerProv(t, "1")
	p.how = func(id string, n int, w http.ResponseWriter, r *http.Request) bool {
		p.fp.mu.Lock()
		p.fp.open["cap"]++
		p.fp.most["cap"] = max(p.fp.most["cap"], p.fp.open["cap"])
		p.fp.mu.Unlock()
		defer func() {
			p.fp.mu.Lock()
			p.fp.open["cap"]--
			p.fp.mu.Unlock()
		}()
		w.Header().Set("Content-Type", "video/mp2t")
		for off := 0; off < len(data); off += 188 * 4000 { // 2.2 MB in three lumps, then the end
			w.Write(data[off : off+188*4000])
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return true
			case <-time.After(700 * time.Millisecond):
			}
		}
		emu.Lock()
		ends = append(ends, time.Now())
		emu.Unlock()
		return true
	}
	for round := 1; round <= 4; round++ {
		done := within(t, 3*time.Second, "the channel's answer")
		began := time.Now()
		r := getAs(t, p.base+"/live/tv/easy/61.ts", "192.0.2.1")
		done()
		if r.StatusCode != 200 {
			t.Fatalf("ask %d: the player was answered %d %q; the account's one connection was free again (the provider had ended the stream before)", round, r.StatusCode, body(r))
		}
		first := make([]byte, 188)
		if _, err := io.ReadFull(r.Body, first); err != nil || time.Since(began) > 2*time.Second {
			t.Errorf("ask %d: the first video came after %.1f s (%v)", round, time.Since(began).Seconds(), err)
		}
		rest, err := io.ReadAll(io.LimitReader(r.Body, int64(2*len(data)))) // (a stream that goes on past its end is cut here)
		ended := time.Now()
		r.Body.Close()
		if got := append(first, rest...); err != nil || !bytes.Equal(got, data) {
			t.Errorf("ask %d: the player's stream: %d bytes (%v); the provider sent %d on this connection: %s", round, len(got), err, len(data), diffAt(data, got))
		}
		emu.Lock()
		if len(ends) != round {
			emu.Unlock()
			t.Fatalf("ask %d: the provider has ended %d connections", round, len(ends))
		}
		late := ended.Sub(ends[round-1])
		emu.Unlock()
		if late > 2*time.Second {
			t.Errorf("ask %d: the player's stream ended %.1f s after the provider ended its own: the player waits on a channel that is over instead of asking again", round, late.Seconds())
		}
		if n := len(p.asked("61")); n != round {
			t.Errorf("after %d asks of the player the provider was asked %d times", round, n)
		}
	}
	time.Sleep(1500 * time.Millisecond)
	if n := len(p.asked("61")); n != 4 {
		t.Errorf("with the player gone the provider was asked again (%d asks, the player made 4)", n)
	}
	if n := p.fp.count(p.fp.most, "cap"); n != 1 {
		t.Errorf("the provider had %d connections open at once on an account that allows one", n)
	}
}
