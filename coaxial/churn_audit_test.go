package main

// Churn audit bench (scratch only, not for the repo). A fake provider that logs every live stream connection
// (when it opened, when the provider side saw it close, bytes, opening burst), and scripted players.
// Measured: provider opens, the most connections open at once per channel and in total, and the gap between
// one connection closing and the next opening.

import (
	"bufio"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A live feed with a real clock: PAT/PMT before each keyframe, PCR and PTS on every frame
// ---------------------------------------------------------------------------

const cFPS, cGOP, cPkts = 25, 50, 20 // 25 fps, keyframe every 2 s, 20 packets a frame (~0.75 Mbit/s)

var cPAT = leadTable(0, []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0x00})
var cPMT = leadTable(0x1000, []byte{0x02, 0xb0, 23, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0,
	0x1b, 0xe1, 0x00, 0xf0, 0, 0x0f, 0xe1, 0x01, 0xf0, 0})

func cFrame(f int, cc *byte) []byte {
	var out []byte
	key := f%cGOP == 0
	if key {
		out = append(append(out, cPAT...), cPMT...)
	}
	b := int64((1000 + float64(f)/cFPS) * 90000)
	af := []byte{0x10, byte(b >> 25), byte(b >> 17), byte(b >> 9), byte(b >> 1), byte(b&1)<<7 | 0x7e, 0}
	if key {
		af[0] |= 0x40
	}
	p := []byte{0x47, 0x41, 0x00, 0x30 | *cc&0x0f}
	*cc++
	p = append(append(p, byte(len(af))), af...)
	pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5, 0x21, 0, 1, 0, 1}
	writeTS(pes[9:14], b)
	p = append(p, pes...)
	for len(p) < 188 {
		p = append(p, 0xa5)
	}
	out = append(out, p...)
	for i := 1; i < cPkts; i++ {
		q := []byte{0x47, 0x01, 0x00, 0x10 | *cc&0x0f}
		*cc++
		for len(q) < 188 {
			q = append(q, 0xa5)
		}
		out = append(out, q...)
	}
	return out
}

// ---------------------------------------------------------------------------
// The provider
// ---------------------------------------------------------------------------

type pconn struct {
	ch           string
	start, end   time.Time
	bytes, burst int64
}

type churnProv struct {
	srv    *httptest.Server
	t0     time.Time
	mu     sync.Mutex
	conns  []*pconn
	api    []time.Time
	maxStr string
	// answer: status for the n-th ask of channel ch (0: stream it); dropAfter: the provider ends a stream after this
	answer    func(ch string, n int) int
	dropAfter func(ch string, n int) time.Duration
	asks      map[string]int
	askTimes  map[string][]time.Time
}

func newChurnProv(t *testing.T, max string) *churnProv {
	cp := &churnProv{t0: time.Now(), maxStr: max, asks: map[string]int{}, askTimes: map[string][]time.Time{}}
	cp.srv = httptest.NewUnstartedServer(http.HandlerFunc(cp.serve))
	cp.srv.Listener = publicListener(t)
	cp.srv.Start()
	t.Cleanup(func() { cp.srv.CloseClientConnections(); cp.srv.Close() })
	return cp
}

func (cp *churnProv) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/player_api.php" {
		cp.mu.Lock()
		cp.api = append(cp.api, time.Now())
		cp.mu.Unlock()
		if cp.maxStr == "fail" {
			http.Error(w, "busy", 503)
			return
		}
		io.WriteString(w, `{"user_info":{"auth":1,"status":"Active","max_connections":"`+cp.maxStr+`","active_cons":"0"},"server_info":{}}`)
		return
	}
	if !(strings.HasPrefix(p, "/live/") && strings.HasSuffix(p, ".ts")) {
		http.NotFound(w, r)
		return
	}
	ch := channelID(p)
	cp.mu.Lock()
	cp.asks[ch]++
	n := cp.asks[ch]
	cp.askTimes[ch] = append(cp.askTimes[ch], time.Now())
	cp.mu.Unlock()
	if cp.answer != nil {
		if s := cp.answer(ch, n); s != 0 {
			http.Error(w, "unavailable", s)
			return
		}
	}
	var drop time.Duration
	if cp.dropAfter != nil {
		drop = cp.dropAfter(ch, n)
	}
	pc := &pconn{ch: ch, start: time.Now()}
	cp.mu.Lock()
	cp.conns = append(cp.conns, pc)
	cp.mu.Unlock()
	defer func() {
		cp.mu.Lock()
		pc.end = time.Now()
		cp.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "video/mp2t")
	var cc byte
	now := int(time.Since(cp.t0).Seconds() * cFPS)
	f := max(0, now-10*cFPS) // the opening burst: the last 10 s at once, from a keyframe
	f -= f % cGOP
	var buf []byte
	for ; f < now; f++ {
		buf = append(buf, cFrame(f, &cc)...)
	}
	if _, err := w.Write(buf); err != nil {
		return
	}
	w.(http.Flusher).Flush()
	cp.mu.Lock()
	pc.bytes, pc.burst = int64(len(buf)), int64(len(buf))
	cp.mu.Unlock()
	began := time.Now()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		if drop > 0 && time.Since(began) > drop {
			panic(http.ErrAbortHandler)
		}
		to := int(time.Since(cp.t0).Seconds() * cFPS)
		buf = buf[:0]
		for ; f < to; f++ {
			buf = append(buf, cFrame(f, &cc)...)
		}
		if _, err := w.Write(buf); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		cp.mu.Lock()
		pc.bytes += int64(len(buf))
		cp.mu.Unlock()
	}
}

// stats: opens per channel, most open at once per channel and in total, smallest gap from one connection's end
// to the next one's start (same channel, and any channel).
type cstats struct {
	opens                 map[string]int
	mostCh                map[string]int
	mostAll               int
	minGapSame, minGapAny time.Duration
	bursts                int64
}

func (cp *churnProv) stats() cstats {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	s := cstats{opens: map[string]int{}, mostCh: map[string]int{}, minGapSame: time.Hour, minGapAny: time.Hour}
	type ev struct {
		t  time.Time
		d  int
		ch string
	}
	var evs []ev
	for _, c := range cp.conns {
		s.opens[c.ch]++
		s.bursts += c.burst
		end := c.end
		if end.IsZero() {
			end = time.Now().Add(time.Hour)
		}
		evs = append(evs, ev{c.start, 1, c.ch}, ev{end, -1, c.ch})
	}
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].t.Equal(evs[j].t) {
			return evs[i].d < evs[j].d
		}
		return evs[i].t.Before(evs[j].t)
	})
	open, all := map[string]int{}, 0
	for _, e := range evs {
		open[e.ch] += e.d
		all += e.d
		s.mostCh[e.ch] = max(s.mostCh[e.ch], open[e.ch])
		s.mostAll = max(s.mostAll, all)
	}
	for _, a := range cp.conns {
		for _, b := range cp.conns {
			if a == b || a.end.IsZero() || b.start.Before(a.start) {
				continue
			}
			g := b.start.Sub(a.end)
			if g < 0 {
				continue // (overlap: counted above)
			}
			if a.ch == b.ch && g < s.minGapSame {
				s.minGapSame = g
			}
			if g < s.minGapAny {
				s.minGapAny = g
			}
		}
	}
	return s
}

func (cp *churnProv) timeline() string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var b strings.Builder
	for _, c := range cp.conns {
		end := "open"
		if !c.end.IsZero() {
			end = fmt.Sprintf("%7.3f", c.end.Sub(cp.t0).Seconds())
		}
		fmt.Fprintf(&b, "    ch %s  open %7.3f  close %s  burst %4d KB  total %5d KB\n", c.ch, c.start.Sub(cp.t0).Seconds(), end, c.burst>>10, c.bytes>>10)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Front door and players
// ---------------------------------------------------------------------------

func churnFront(t *testing.T, cp *churnProv, pt string, a advanced) (*front, string) {
	f := newFront(provider{Name: "Churn" + pt, URL: cp.srv.URL, User: "realU", Password: "realP", Port: port(pt), LiveTV: "ts", Advanced: a},
		player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = r.Header.Get("X-Dev") + ":5000"
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		mu.Lock()
		for _, ch := range channels {
			if ch.provider == f.base {
				ch.drop()
			}
		}
		mu.Unlock()
	})
	return f, srv.URL
}

var churnClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// tsPlay: device dev reads channel ch as MPEG-TS for d, then hangs up. Returns the status and bytes read.
func tsPlay(base, dev, ch string, d time.Duration) (int, int64) {
	req, _ := http.NewRequest("GET", base+"/live/tv/easy/"+ch+".ts", nil)
	req.Header.Set("X-Dev", dev)
	resp, err := churnClient.Do(req)
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, 0
	}
	var n int64
	end := time.Now().Add(d)
	buf := make([]byte, 65536)
	for time.Now().Before(end) {
		k, err := resp.Body.Read(buf)
		n += int64(k)
		if err != nil {
			break
		}
	}
	return 200, n
}

// hlsPlay: device dev plays channel ch as HLS for d (playlist about every 4 s, every new piece fetched), as AVPlayer does.
func hlsPlay(base, dev, ch string, d time.Duration) (pieces int, status int) {
	get := func(u string) (*http.Response, error) {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("X-Dev", dev)
		return churnClient.Do(req)
	}
	r, err := get(base + "/live/tv/easy/" + ch + ".m3u8")
	if err != nil {
		return 0, 0
	}
	r.Body.Close()
	if r.StatusCode != 302 {
		return 0, r.StatusCode
	}
	pl := base + r.Header.Get("Location")
	dir := pl[:strings.LastIndexByte(pl, '/')+1]
	seen := map[string]bool{}
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		began := time.Now()
		r, err := get(pl)
		if err != nil {
			return pieces, 0
		}
		status = r.StatusCode
		sc := bufio.NewScanner(r.Body)
		var segs []string
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
				segs = append(segs, l)
			}
		}
		r.Body.Close()
		if status != 200 {
			return pieces, status
		}
		for _, s := range segs {
			if seen[s] {
				continue
			}
			seen[s] = true
			if sr, err := get(dir + s); err == nil {
				io.Copy(io.Discard, sr.Body)
				sr.Body.Close()
				pieces++
			}
		}
		time.Sleep(max(0, 4*time.Second-time.Since(began)))
	}
	return pieces, status
}

func report(t *testing.T, name string, cp *churnProv, extra string) cstats {
	s := cp.stats()
	line := fmt.Sprintf("RESULT %s: opens=%v most_per_channel=%v most_total=%d min_gap_same_channel=%v min_gap_any=%v opening_bursts=%d KB %s",
		name, s.opens, s.mostCh, s.mostAll, s.minGapSame.Round(time.Millisecond), s.minGapAny.Round(time.Millisecond), s.bursts>>10, extra)
	t.Log(line + "\n" + cp.timeline())
	if fn := os.Getenv("CHURN_OUT"); fn != "" {
		if fh, err := os.OpenFile(fn, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(fh, line)
			fmt.Fprint(fh, cp.timeline())
			fh.Close()
		}
	}
	for ch, m := range s.mostCh {
		if m > 1 {
			t.Errorf("%s: channel %s had %d provider connections open at once", name, ch, m)
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// Scenarios
// ---------------------------------------------------------------------------

// S1: the Apple TV sequence seen on CNN: start, leave after 1 s, come back at once, leave after 0 s, start again
// 29 s later, leave after 4 s, come back at once, leave after 6 s, then the same channel as HLS.
func TestChurnAppleTVSequence(t *testing.T) {
	for i, gap := range []time.Duration{50 * time.Millisecond, 1 * time.Second} {
		cp := newChurnProv(t, "4")
		_, base := churnFront(t, cp, fmt.Sprint(18700+i), advanced{})
		dev := "198.51.100.31"
		tsPlay(base, dev, "7", 1*time.Second)
		time.Sleep(50 * time.Millisecond)
		tsPlay(base, dev, "7", 100*time.Millisecond)
		time.Sleep(29 * time.Second)
		tsPlay(base, dev, "7", 4*time.Second)
		time.Sleep(50 * time.Millisecond)
		tsPlay(base, dev, "7", 6*time.Second)
		time.Sleep(gap)
		pieces, st := hlsPlay(base, dev, "7", 16*time.Second)
		time.Sleep(time.Second)
		report(t, fmt.Sprintf("S1 appletv-cnn hls-gap=%v", gap), cp, fmt.Sprintf("hls_pieces=%d hls_status=%d", pieces, st))
	}
}

// S2: several devices asking for the same channel at the same instant, TS and HLS mixed, and leaving while
// others arrive, over the 0.4 s keep and the 5 s tuning edge. Must never be two provider connections.
func TestChurnSameInstant(t *testing.T) {
	cp := newChurnProv(t, "4")
	_, base := churnFront(t, cp, "18710", advanced{})
	rng := rand.New(rand.NewSource(7))
	devs := []string{"198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4"}
	for round := 0; round < 24; round++ {
		var wg sync.WaitGroup
		n := 2 + rng.Intn(3)
		for k := 0; k < n; k++ {
			wg.Add(1)
			hls := rng.Intn(3) == 0
			delay := time.Duration(rng.Intn(30)) * time.Millisecond
			hold := time.Duration(300+rng.Intn(6000)) * time.Millisecond
			go func(dev string) {
				defer wg.Done()
				time.Sleep(delay)
				if hls {
					hlsPlay(base, dev, "9", hold)
				} else {
					tsPlay(base, dev, "9", hold)
				}
			}(devs[k])
		}
		wg.Wait()
		time.Sleep(time.Duration(rng.Intn(800)) * time.Millisecond)
	}
	// leave and arrive at the same moment, around the keep and the tuning edge
	for _, at := range []time.Duration{1 * time.Second, 4900 * time.Millisecond, 5100 * time.Millisecond, 8 * time.Second} {
		for _, d := range []time.Duration{-20 * time.Millisecond, 0, 380 * time.Millisecond, 400 * time.Millisecond, 420 * time.Millisecond} {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); tsPlay(base, devs[0], "9", at) }()
			go func() { defer wg.Done(); time.Sleep(at + d); tsPlay(base, devs[1], "9", 500*time.Millisecond) }()
			wg.Wait()
			time.Sleep(600 * time.Millisecond)
		}
	}
	time.Sleep(time.Second)
	report(t, "S2 same-instant+edges", cp, "")
}

// S3: one-connection account, a device zapping through channels with short dwells, hanging up on the old
// channel before or after asking for the new one; and a second device on a 2-connection account.
func TestChurnZapOneConnection(t *testing.T) {
	cp := newChurnProv(t, "1")
	_, base := churnFront(t, cp, "18720", advanced{Connections: "1"})
	dev := "198.51.100.40"
	chs := []string{"11", "12", "13", "14", "15", "16", "17", "18"}
	var refused atomic.Int32
	// overlapping: the new request goes out 150 ms before the old one hangs up
	for i, ch := range chs {
		done := make(chan struct{})
		go func(ch string, d time.Duration) {
			if st, _ := tsPlay(base, dev, ch, d); st != 200 {
				refused.Add(1)
			}
			close(done)
		}(ch, time.Duration(800+300*i)*time.Millisecond)
		time.Sleep(time.Duration(650+300*i) * time.Millisecond)
		_ = done
	}
	time.Sleep(4 * time.Second)
	report(t, "S3 zap-1conn overlapping-requests", cp, fmt.Sprintf("refused=%d", refused.Load()))

	cp2 := newChurnProv(t, "2")
	_, base2 := churnFront(t, cp2, "18721", advanced{Connections: "2"})
	go tsPlay(base2, "198.51.100.41", "21", 20*time.Second) // a recorder on one channel
	time.Sleep(time.Second)
	var ref atomic.Int32
	for i := 0; i < 10; i++ {
		go func(ch string) {
			if st, _ := tsPlay(base2, dev, ch, 1200*time.Millisecond); st != 200 {
				ref.Add(1)
			}
		}(fmt.Sprint(30 + i))
		time.Sleep(1000 * time.Millisecond)
	}
	time.Sleep(10 * time.Second)
	s := report(t, "S3b zap-2conn with recorder", cp2, fmt.Sprintf("refused=%d", ref.Load()))
	if s.mostAll > 2 {
		t.Errorf("more than the 2 connections allowed: %d", s.mostAll)
	}
}

// S4: a provider ends a watched channel after 35 s and answers 404 for 20 s (its side restarting the channel).
// S4b: the same, but the channel never comes back, and the player keeps asking every 3 s afterwards.
func TestChurnDrop404(t *testing.T) {
	t.Run("back", func(t *testing.T) {
		t.Parallel()
		cp := newChurnProv(t, "4")
		var back time.Time
		var once sync.Once
		cp.dropAfter = func(ch string, n int) time.Duration {
			if n == 1 {
				return 35 * time.Second
			}
			return 0
		}
		cp.answer = func(ch string, n int) int {
			if n >= 2 {
				once.Do(func() { back = time.Now().Add(20 * time.Second) })
				if time.Now().Before(back) {
					return 404
				}
			}
			return 0
		}
		_, base := churnFront(t, cp, "18730", advanced{})
		tsPlay(base, "198.51.100.50", "5", 80*time.Second)
		time.Sleep(time.Second)
		cp.mu.Lock()
		var at []string
		for _, x := range cp.askTimes["5"] {
			at = append(at, fmt.Sprintf("%.1f", x.Sub(cp.t0).Seconds()))
		}
		cp.mu.Unlock()
		report(t, "S4 drop-then-404-for-20s", cp, "asks_at_s="+strings.Join(at, ","))
	})
	t.Run("never", func(t *testing.T) {
		t.Parallel()
		cp := newChurnProv(t, "4")
		cp.dropAfter = func(ch string, n int) time.Duration {
			if n == 1 {
				return 35 * time.Second
			}
			return 0
		}
		cp.answer = func(ch string, n int) int {
			if n >= 2 {
				return 404
			}
			return 0
		}
		_, base := churnFront(t, cp, "18731", advanced{})
		tsPlay(base, "198.51.100.51", "6", 120*time.Second)
		ended := time.Since(cp.t0).Seconds()
		statuses := []string{}
		for i := 0; i < 6; i++ { // the player retries on its own
			time.Sleep(3 * time.Second)
			st, _ := tsPlay(base, "198.51.100.51", "6", time.Second)
			statuses = append(statuses, fmt.Sprint(st))
		}
		cp.mu.Lock()
		var at []string
		for _, x := range cp.askTimes["6"] {
			at = append(at, fmt.Sprintf("%.1f", x.Sub(cp.t0).Seconds()))
		}
		cp.mu.Unlock()
		report(t, "S4b drop-then-404-forever", cp, fmt.Sprintf("player_released_at=%.1fs player_retries=%v asks_at_s=%s", ended, statuses, strings.Join(at, ",")))
	})
	t.Run("accounts", func(t *testing.T) {
		t.Parallel()
		good := newChurnProv(t, "3")
		bad := newChurnProv(t, "fail")
		fg := newFront(provider{Name: "Good", URL: good.srv.URL, User: "u", Password: "p", Port: port("18732"), LiveTV: "ts"}, player{User: "tv", Password: "easy"})
		fb := newFront(provider{Name: "Bad", URL: bad.srv.URL, User: "u", Password: "p", Port: port("18733"), LiveTV: "ts"}, player{User: "tv", Password: "easy"})
		mu.Lock()
		fronts[fg.port], fronts[fb.port] = fg, fb
		delete(accounts, fg.port)
		delete(accounts, fb.port)
		mu.Unlock()
		stop, done := make(chan struct{}), make(chan struct{})
		go func() { keepAccounts(stop); close(done) }()
		time.Sleep(95 * time.Second)
		close(stop)
		<-done
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		delete(fronts, fg.port)
		delete(fronts, fb.port)
		delete(accounts, fg.port)
		delete(accounts, fb.port)
		mu.Unlock()
		good.mu.Lock()
		bad.mu.Lock()
		gt, bt := []string{}, []string{}
		for _, x := range good.api {
			gt = append(gt, fmt.Sprintf("%.1f", x.Sub(good.t0).Seconds()))
		}
		for _, x := range bad.api {
			bt = append(bt, fmt.Sprintf("%.1f", x.Sub(bad.t0).Seconds()))
		}
		good.mu.Unlock()
		bad.mu.Unlock()
		line := fmt.Sprintf("RESULT S5 account reads over 95 s, one provider failing: good_provider_reads=%d at %v; failing_provider_reads=%d at %v", len(gt), gt, len(bt), bt)
		t.Log(line)
		if fn := os.Getenv("CHURN_OUT"); fn != "" {
			if fh, err := os.OpenFile(fn, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				fmt.Fprintln(fh, line)
				fh.Close()
			}
		}
	})
}

// S6: a channel the provider answers 404 (off air), tuned by an Apple TV app that asks for it as MPEG-TS three
// times and then as HLS within a second (seen live), and tunes it again a few seconds later, four times.
func TestChurnRefusedRetries(t *testing.T) {
	cp := newChurnProv(t, "4")
	cp.answer = func(ch string, n int) int {
		if ch == "404" {
			return 404
		}
		return 0
	}
	_, base := churnFront(t, cp, "18740", advanced{})
	dev := "198.51.100.31"
	player := 0
	for round := 0; round < 4; round++ {
		for k := 0; k < 3; k++ {
			tsPlay(base, dev, "404", time.Second)
			player++
			time.Sleep(150 * time.Millisecond)
		}
		hlsPlay(base, dev, "404", time.Second)
		player++
		time.Sleep(2500 * time.Millisecond)
	}
	cp.mu.Lock()
	asks := cp.asks["404"]
	cp.mu.Unlock()
	line := fmt.Sprintf("RESULT S6 refused-channel retries: player_requests=%d provider_asks=%d over ~%.0f s", player, asks, time.Since(cp.t0).Seconds())
	t.Log(line)
	if fn := os.Getenv("CHURN_OUT"); fn != "" {
		if fh, err := os.OpenFile(fn, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(fh, line)
			fh.Close()
		}
	}
}

// S2b: a device leaves at the moment another arrives, at several channel ages (inside and past the 5 s tuning
// time) and several offsets around the 0.4 s keep: a fresh channel for each case. Counts opens per case.
func TestChurnEdges(t *testing.T) {
	cp := newChurnProv(t, "4")
	_, base := churnFront(t, cp, "18750", advanced{})
	devs := []string{"198.51.100.1", "198.51.100.2"}
	n := 100
	var cases []string
	for _, at := range []time.Duration{1 * time.Second, 4900 * time.Millisecond, 5100 * time.Millisecond, 8 * time.Second} {
		for _, d := range []time.Duration{-20 * time.Millisecond, 0, 100 * time.Millisecond, 380 * time.Millisecond, 420 * time.Millisecond, 600 * time.Millisecond} {
			n++
			ch := fmt.Sprint(n)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); tsPlay(base, devs[0], ch, at) }()
			go func() { defer wg.Done(); time.Sleep(at + d); tsPlay(base, devs[1], ch, 500*time.Millisecond) }()
			wg.Wait()
			time.Sleep(700 * time.Millisecond)
			cp.mu.Lock()
			cases = append(cases, fmt.Sprintf("age=%v next_after=%v opens=%d", at, d, cp.asks[ch]))
			cp.mu.Unlock()
		}
	}
	report(t, "S2b leave-vs-arrive edges", cp, "\n      "+strings.Join(cases, "\n      "))
}

// S5b: every account answers: how often each is read in the first 45 s.
func TestChurnAccountsAllGood(t *testing.T) {
	good := newChurnProv(t, "3")
	fg := newFront(provider{Name: "Good2", URL: good.srv.URL, User: "u", Password: "p", Port: port("18760"), LiveTV: "ts"}, player{User: "tv", Password: "easy"})
	mu.Lock()
	for k := range fronts {
		delete(fronts, k)
	}
	fronts[fg.port] = fg
	delete(accounts, fg.port)
	mu.Unlock()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { keepAccounts(stop); close(done) }()
	time.Sleep(45 * time.Second)
	close(stop)
	<-done
	mu.Lock()
	delete(fronts, fg.port)
	delete(accounts, fg.port)
	mu.Unlock()
	good.mu.Lock()
	gt := []string{}
	for _, x := range good.api {
		gt = append(gt, fmt.Sprintf("%.1f", x.Sub(good.t0).Seconds()))
	}
	good.mu.Unlock()
	line := fmt.Sprintf("RESULT S5b account reads over 45 s, all providers answering: reads=%d at %v", len(gt), gt)
	t.Log(line)
	if fn := os.Getenv("CHURN_OUT"); fn != "" {
		if fh, err := os.OpenFile(fn, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(fh, line)
			fh.Close()
		}
	}
}
