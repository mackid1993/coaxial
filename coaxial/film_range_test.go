package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A film or an episode is one file at the provider, and a player reads it in parts: it asks for a range of
// bytes (Range), is told which part it got and how long the whole is (206, Content-Range, Content-Length),
// and seeks by asking for another range. The tests so far prove that a seek is admitted and how long it
// takes; none checks that the range the player asked for is the range the provider was asked for, nor that
// what the provider answered is what the player got. A proxy that drops the Range, answers 200 for 206,
// loses a Content-Range or gets one byte wrong leaves a player that cannot seek, shows no length, or plays
// from the start after every seek. Here a player asks for one film in every way players do, through the proxy
// and straight at the provider, and the two answers must be the same: status, every header, every byte.
//
// The provider is a plain file server, as providers' film servers are (it answers the way nginx does): 200
// for no Range, 206 for a range it has, 416 for one past the end, If-Range by ETag, HEAD without a body.
// One film it serves itself; the other it sends the player on to its video server for, as most do. A catch-up
// (an hour of a channel) is a file like any other, and is asked for in both forms its address takes.

const filmRangeSize = 3<<20 + 123

// filmRangeProv: the provider's own server and its video server.
type filmRangeProv struct {
	own, video *fakeProv
	data       []byte

	mu    sync.Mutex
	saw   map[string][2]string // by the ask's tag: the Range and If-Range that came with it
	open  int                  // reads under way
	most  int
	reads int
}

const filmRangeETag = `"5f3c2a-300007b"`

func newFilmRangeProv(t *testing.T) *filmRangeProv {
	p := &filmRangeProv{saw: map[string][2]string{}, data: make([]byte, filmRangeSize)}
	x := uint64(0x2545f4914f6cdd1d)
	for i := range p.data { // (no two stretches alike: a part served from the wrong place shows)
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		p.data[i] = byte(x >> 32)
	}
	p.video = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/vod/tok7/") || r.URL.Query().Get("sig") != "a/b" {
			return false
		}
		p.file(w, r, "video/mp4")
		return true
	})
	p.own = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/movie/realU/realP/7.mkv", "/movie/realU/realP/17.mkv": // (17: the same film, sent slowly)
			p.file(w, r, "video/x-matroska")
		case "/timeshift/realU/realP/60/2026-01-01:10-00/2.ts": // a catch-up: an hour of channel 2, one file
			p.file(w, r, "video/mp2t")
		case "/streaming/timeshift.php": // the same, in the other form Xtream servers take
			if r.URL.RawQuery != "username=realU&password=realP&stream=2&start=2026-01-01:10-00&duration=60" {
				return false
			}
			p.file(w, r, "video/mp2t")
		case "/series/realU/realP/9.mp4":
			w.Header().Set("Location", p.video.srv.URL+"/vod/tok7/9.mp4?e=1&sig=a%2Fb")
			w.WriteHeader(302)
		default:
			return false
		}
		return true
	})
	return p
}

// file: the film, answered as a file server answers.
func (p *filmRangeProv) file(w http.ResponseWriter, r *http.Request, kind string) {
	rg, ifr := r.Header.Get("Range"), r.Header.Get("If-Range")
	p.mu.Lock()
	if tag := r.Header.Get("X-Ask"); tag != "" {
		p.saw[tag] = [2]string{rg, ifr}
	}
	p.reads++
	p.open++
	p.most = max(p.most, p.open)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.open--
		p.mu.Unlock()
	}()
	size := len(p.data)
	h := w.Header()
	h.Set("Content-Type", kind)
	h.Set("Accept-Ranges", "bytes")
	h.Set("ETag", filmRangeETag)
	h.Set("Last-Modified", "Tue, 03 Mar 2026 10:11:12 GMT")
	h.Set("Server", "nginx")
	from, to, status := 0, size-1, 200
	if ifr != "" && ifr != filmRangeETag {
		rg = "" // (the film changed since the player read its first part: the whole of it again)
	}
	if v, ok := strings.CutPrefix(rg, "bytes="); ok {
		a, b, _ := strings.Cut(v, "-")
		switch {
		case a == "": // the last b bytes
			n, _ := strconv.Atoi(b)
			from = max(0, size-n)
		case b == "":
			from, _ = strconv.Atoi(a)
		default:
			from, _ = strconv.Atoi(a)
			to, _ = strconv.Atoi(b)
			to = min(to, size-1)
		}
		if from >= size || from > to {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			h.Set("Content-Type", "text/html")
			page := "<html><head><title>416 Requested Range Not Satisfiable</title></head></html>\n"
			h.Set("Content-Length", strconv.Itoa(len(page)))
			w.WriteHeader(416)
			if r.Method != "HEAD" {
				io.WriteString(w, page)
			}
			return
		}
		status = 206
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, size))
	}
	h.Set("Content-Length", strconv.Itoa(to-from+1))
	w.WriteHeader(status)
	if r.Method == "HEAD" {
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/17.mkv") {
		w.Write(p.data[from : to+1])
		return
	}
	for at := from; at <= to; at += 16 << 10 { // slowly: 16 KB every 25 ms, about the rate a film plays at
		if _, err := w.Write(p.data[at:min(to+1, at+16<<10)]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

type filmRangeAnswer struct {
	status int
	h      http.Header
	body   []byte
	err    error
	took   time.Duration
}

// filmRangeAsk: one ask, as a player makes it: sent elsewhere, it asks there with the same headers, working
// the address out from the one just asked. home: the only server it may ask ("": any).
func filmRangeAsk(t *testing.T, client *http.Client, method, addr, home, dev, tag, rg, ifr string) filmRangeAnswer {
	t.Helper()
	began := time.Now()
	for hop := 0; hop < 5; hop++ {
		u, err := url.Parse(addr)
		if err != nil || (home != "" && u.Host != home) {
			t.Fatalf("the player was sent to %q, which is not the proxy (%s): asked there, the film would leave the VPN", addr, home)
		}
		req, _ := http.NewRequest(method, addr, nil)
		req.Header.Set("X-Dev", dev)
		req.Header.Set("X-Ask", tag)
		if rg != "" {
			req.Header.Set("Range", rg)
		}
		if ifr != "" {
			req.Header.Set("If-Range", ifr)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, addr, err)
		}
		if loc := resp.Header.Get("Location"); isRedirect(resp.StatusCode) && loc != "" {
			resp.Body.Close()
			next, err := u.Parse(loc)
			if err != nil {
				t.Fatalf("sent on to an address a player cannot read: %q", loc)
			}
			addr = next.String()
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		return filmRangeAnswer{resp.StatusCode, resp.Header, data, err, time.Since(began)}
	}
	t.Fatalf("the player was sent from one address to the next 5 times and never answered")
	return filmRangeAnswer{}
}

// filmRangeSame: the answer through the proxy is the provider's own. page: the provider's own error page,
// which the proxy reads to hide a login in it and so sends without a length of its own.
func filmRangeSame(t *testing.T, what string, got, want filmRangeAnswer, page bool) {
	t.Helper()
	if got.err != nil {
		t.Errorf("%s: the answer broke off: %v", what, got.err)
	}
	if got.status != want.status {
		t.Errorf("%s: answered %d through the proxy; the provider itself answers %d", what, got.status, want.status)
	}
	names := map[string]bool{}
	for k := range got.h {
		names[k] = true
	}
	for k := range want.h {
		names[k] = true
	}
	var keys []string
	for k := range names {
		// (Date and the framing of one connection are each server's own)
		if k != "Date" && k != "Connection" && !(page && k == "Content-Length") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if g, w := strings.Join(got.h.Values(k), " | "), strings.Join(want.h.Values(k), " | "); g != w {
			t.Errorf("%s: header %s is %q through the proxy; the provider itself sends %q", what, k, g, w)
		}
	}
	if !bytes.Equal(got.body, want.body) {
		at := 0
		for at < len(got.body) && at < len(want.body) && got.body[at] == want.body[at] {
			at++
		}
		t.Errorf("%s: %d bytes through the proxy, %d from the provider itself; they differ from byte %d of the answer on", what, len(got.body), len(want.body), at)
	}
}

var filmRangeCases = []struct {
	name, method, rg, ifr string
	status                int
}{
	{"the whole film, no Range", "GET", "", "", 200},
	{"from the start (bytes=0-)", "GET", "bytes=0-", "", 206},
	{"its first two bytes", "GET", "bytes=0-1", "", 206},
	{"a part in the middle", "GET", "bytes=1000000-1299999", "", 206},
	{"its end (bytes=-65536)", "GET", "bytes=-65536", "", 206},
	{"from the middle to the end", "GET", "bytes=1500000-", "", 206},
	{"its very last byte", "GET", fmt.Sprintf("bytes=%d-", filmRangeSize-1), "", 206},
	{"a range that runs past the end", "GET", fmt.Sprintf("bytes=3000000-%d", filmRangeSize+5000), "", 206},
	{"a range that starts past the end", "GET", "bytes=9000000-", "", 416},
	{"HEAD", "HEAD", "", "", 200},
	{"HEAD with a Range", "HEAD", "bytes=100-199", "", 206},
	{"If-Range that still holds", "GET", "bytes=2000000-", filmRangeETag, 206},
	{"If-Range that no longer holds", "GET", "bytes=2000000-", `"an-older-file"`, 200},
}

func TestFilmRangesPassStraightThrough(t *testing.T) {
	p := newFilmRangeProv(t)
	_, base := limFront(t, p.own, "ts", "", advanced{})
	home := strings.TrimPrefix(base, "http://")
	kept := &http.Client{Transport: &http.Transport{}, // a player that keeps its connection for the next range
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer kept.CloseIdleConnections()
	films := []struct {
		name, easy, real string
		own              bool // answered by the provider's own server
	}{
		{"a film on the provider's own server", "/movie/tv/easy/7.mkv", p.own.srv.URL + "/movie/realU/realP/7.mkv", true},
		{"an episode the provider sends on to its video server", "/series/tv/easy/9.mp4", p.video.srv.URL + "/vod/tok7/9.mp4?e=1&sig=a%2Fb", false},
		{"a catch-up, asked for by its path", "/timeshift/tv/easy/60/2026-01-01:10-00/2.ts", p.own.srv.URL + "/timeshift/realU/realP/60/2026-01-01:10-00/2.ts", true},
		{"a catch-up, asked for through timeshift.php", "/streaming/timeshift.php?username=tv&password=easy&stream=2&start=2026-01-01:10-00&duration=60",
			p.own.srv.URL + "/streaming/timeshift.php?username=realU&password=realP&stream=2&start=2026-01-01:10-00&duration=60", true},
	}
	for fi, film := range films {
		for ci, c := range filmRangeCases {
			for ki, client := range []*http.Client{devClient, kept} {
				what := fmt.Sprintf("%s, %s", film.name, c.name)
				if ki == 1 {
					what += " (on a kept connection)"
				}
				tag := fmt.Sprintf("%d-%d-%d", fi, ci, ki)
				want := filmRangeAsk(t, devClient, c.method, film.real, "", "192.0.2.9", "", c.rg, c.ifr)
				if want.status != c.status {
					t.Fatalf("%s: the stand-in provider answers %d, the test expects %d of it", what, want.status, c.status)
				}
				got := filmRangeAsk(t, client, c.method, base+film.easy, home, "192.0.2.1", tag, c.rg, c.ifr)
				// (an error page from the provider's own server is read through, to hide a login in it)
				filmRangeSame(t, what, got, want, want.status >= 400 && film.own)
				p.mu.Lock()
				saw, ok := p.saw[tag]
				p.mu.Unlock()
				if !ok || saw[0] != c.rg || saw[1] != c.ifr {
					t.Errorf("%s: the player sent Range %q and If-Range %q; the provider was asked with %q and %q (asked at all: %v)", what, c.rg, c.ifr, saw[0], saw[1], ok)
				}
				if got.took > 3*time.Second {
					t.Errorf("%s: took %.1f s through the proxy; the provider answers at once", what, got.took.Seconds())
				}
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open != 0 {
		t.Errorf("%d reads of the film are still open at the provider after every answer was read to its end", p.open)
	}
}

// filmRangeOpen: a read left open: the answer's start is read, the rest is read by the caller.
func filmRangeOpen(t *testing.T, base, dev, path, rg string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("X-Dev", dev)
	if rg != "" {
		req.Header.Set("Range", rg)
	}
	resp, err := devClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s %s: %v", dev, path, rg, err)
	}
	return resp
}

// A player seeks while its first read of the film is still running (it opens the new read before it closes
// the old one, as players built on FFmpeg and VLC do). The new read must start at once and carry the bytes of
// the place asked for; with no limit set the first read goes on untouched, and on an account with one
// connection the first read is cut (it must end broken, never as a short answer that looks whole) and makes
// room. Then two devices read the same film at once, each from its own place.
func TestFilmSeekWhileTheFirstReadRuns(t *testing.T) {
	for _, connections := range []number{"", "1"} {
		name := "no limit set"
		if connections != "" {
			name = "an account with one connection"
		}
		t.Run(name, func(t *testing.T) {
			p := newFilmRangeProv(t)
			_, base := limFront(t, p.own, "ts", connections, advanced{})
			const path, seekAt = "/movie/tv/easy/17.mkv", 2000000
			first := filmRangeOpen(t, base, "192.0.2.1", path, "bytes=0-")
			defer first.Body.Close()
			if first.StatusCode != 206 || first.Header.Get("Content-Range") != fmt.Sprintf("bytes 0-%d/%d", filmRangeSize-1, filmRangeSize) {
				t.Fatalf("the first read: %d, Content-Range %q", first.StatusCode, first.Header.Get("Content-Range"))
			}
			var fmu sync.Mutex
			var firstGot []byte
			var firstErr error
			firstDone := make(chan struct{})
			go func() {
				defer close(firstDone)
				buf := make([]byte, 64<<10)
				for {
					n, err := first.Body.Read(buf)
					fmu.Lock()
					firstGot = append(firstGot, buf[:n]...)
					if err != nil {
						if err != io.EOF {
							firstErr = err
						}
						fmu.Unlock()
						return
					}
					fmu.Unlock()
				}
			}()
			time.Sleep(500 * time.Millisecond)
			began := time.Now()
			done := within(t, 3*time.Second, "the seek")
			seek := filmRangeOpen(t, base, "192.0.2.1", path, fmt.Sprintf("bytes=%d-", seekAt))
			done()
			took := time.Since(began)
			defer seek.Body.Close()
			fmu.Lock()
			had := len(firstGot)
			fmu.Unlock()
			if had >= filmRangeSize {
				t.Fatalf("the first read was over before the seek came: the test's film is sent too fast for it")
			}
			if seek.StatusCode != 206 || seek.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", seekAt, filmRangeSize-1, filmRangeSize) {
				t.Fatalf("the seek: %d, Content-Range %q", seek.StatusCode, seek.Header.Get("Content-Range"))
			}
			if took > 3*time.Second {
				t.Errorf("the seek took %.1f s to start while the first read was still running (%d bytes into the film)", took.Seconds(), had)
			}
			data, err := io.ReadAll(seek.Body)
			if err != nil || !bytes.Equal(data, p.data[seekAt:]) {
				t.Errorf("the seek: %d bytes (%v); want the film's %d bytes from byte %d on, exactly", len(data), err, filmRangeSize-seekAt, seekAt)
			}
			select {
			case <-firstDone:
			case <-time.After(20 * time.Second):
				t.Fatalf("the first read neither ended nor was cut")
			}
			fmu.Lock()
			defer fmu.Unlock()
			if !bytes.Equal(firstGot, p.data[:len(firstGot)]) {
				t.Errorf("the first read: the %d bytes it was sent are not the film's first %d", len(firstGot), len(firstGot))
			}
			if connections == "" {
				if firstErr != nil || len(firstGot) != filmRangeSize {
					t.Errorf("no limit is set, so the first read goes on beside the seek: it got %d of %d bytes (%v)", len(firstGot), filmRangeSize, firstErr)
				}
			} else if firstErr == nil && len(firstGot) != filmRangeSize {
				t.Errorf("the first read was cut to make room after %d bytes, and its answer ended as if it were whole: the player takes a short film for the whole one", len(firstGot))
			}
		})
	}
	t.Run("two devices on one film", func(t *testing.T) {
		p := newFilmRangeProv(t)
		_, base := limFront(t, p.own, "ts", "", advanced{})
		a := filmRangeOpen(t, base, "192.0.2.1", "/movie/tv/easy/17.mkv", "bytes=0-")
		b := filmRangeOpen(t, base, "192.0.2.2", "/movie/tv/easy/17.mkv", "bytes=1048576-")
		defer a.Body.Close()
		defer b.Body.Close()
		if a.StatusCode != 206 || b.StatusCode != 206 {
			t.Fatalf("two devices on one film: %d and %d, want 206 for both", a.StatusCode, b.StatusCode)
		}
		var wg sync.WaitGroup
		for _, x := range []struct {
			r    *http.Response
			from int
		}{{a, 0}, {b, 1048576}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				data, err := io.ReadAll(x.r.Body)
				if err != nil || !bytes.Equal(data, p.data[x.from:]) {
					t.Errorf("the device reading from byte %d: %d bytes (%v); want the film's %d bytes from there on, exactly", x.from, len(data), err, filmRangeSize-x.from)
				}
			}()
		}
		wg.Wait()
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.most != 2 || p.reads != 2 {
			t.Errorf("the provider saw %d reads, %d at once; two devices each read the film once, at the same time", p.reads, p.most)
		}
	})
}
