package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Live MPEG-TS on a provider that does not send the video from the address a channel is asked at: it answers
// "go to that other address" (a 301, 302 or 307), as many providers do for every live channel: the address
// players know is a front desk, and the video comes from a server picked for each ask, most often under a
// token that is good for that one connection. Until now every stand-in provider in these tests sent live
// MPEG-TS from the very address asked. Here the provider sends the proxy on:
//
//   - to another server, the address written in full;
//   - to another address on its own server, written by path alone;
//   - the same, written relative to the address asked ("../../../play/...");
//   - to another server, written with no scheme ("//server/...");
//   - on twice: to a server in between, which sends on again;
//   - and to a new address at every ask: the video server ends the connection after half a minute, and the
//     token it was reached under is no longer good.
//
// What must hold for the device watching, whichever way the provider wrote it:
//
//   1. the player itself is sent nowhere: its one request is answered 200 with video (a player that followed
//      the provider's redirect itself would leave the proxy, and the VPN);
//   2. it is sent exactly the bytes the video server sent, as they come (live_rough_test's two rules);
//   3. the provider is asked once per hop for the channel, whatever the number of devices on it, with the
//      player's own User-Agent at every hop, and holds one video connection, which ends when the last device
//      hangs up;
//   4. when the video server ends the connection, the channel is asked for again at its own address (where a
//      new token is handed out), not at the address the old token led to, and its video carries on.

// liveRedirTune: how long after the player asked its first video may come (the provider answers at once).
const liveRedirTune = 2 * time.Second

type liveRedirScene struct {
	name string
	// where: what server n (0: the provider's own, 1: one in between, 2: the video server) answers an ask for
	// path with: the address to send on to ("": the video itself). own, mid and video are the servers'
	// addresses, tok the token handed out for this ask.
	where func(n int, path, own, mid, video, tok string) (status int, loc string)
	final int           // the server the video comes from
	hops  [3]int        // how often each server is asked for one open of the channel
	drop  time.Duration // the video server ends its first connection after this (0: never)
}

type liveRedirProv struct {
	sc   liveRedirScene
	id   string
	srv  [3]*fakeProv
	data []byte
	plan []roughStep
	lw   *lateWatch

	mu       sync.Mutex
	asks     [3][]string // what each server was asked, in order
	agents   [3][]string // and the User-Agent each ask came with
	toks     int         // tokens handed out
	step     int         // the next step of the plan
	off      int
	sent     []roughMark
	streams  int // video connections answered
	open     int // open now
	most     int
	stale    int // asks under a token that is no longer the newest
	began    time.Time
	finished chan struct{}
	once     sync.Once
}

func newLiveRedirProv(t *testing.T, sc liveRedirScene, id string, plan []roughStep) (*liveRedirProv, string) {
	p := &liveRedirProv{sc: sc, id: id, plan: plan, data: roughBytes(roughTotal(plan)), finished: make(chan struct{})}
	for n := range p.srv {
		n := n
		p.srv[n] = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
			p.serve(n, w, r)
			return true
		})
	}
	_, base := limFront(t, p.srv[0], "ts", "", advanced{})
	return p, base
}

func (p *liveRedirProv) serve(n int, w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.asks[n] = append(p.asks[n], r.URL.RequestURI())
	p.agents[n] = append(p.agents[n], r.Header.Get("User-Agent"))
	tok := fmt.Sprintf("tok%d", p.toks)
	if n == 0 && r.URL.Path == "/live/realU/realP/"+p.id+".ts" { // every ask at the channel's own address: a new token
		p.toks++
		tok = fmt.Sprintf("tok%d", p.toks)
	}
	p.mu.Unlock()
	status, loc := p.sc.where(n, r.URL.Path, p.srv[0].srv.URL, p.srv[1].srv.URL, p.srv[2].srv.URL, tok)
	if loc != "" {
		w.Header().Set("Location", loc)
		w.WriteHeader(status)
		return
	}
	if n != p.sc.final || !strings.HasPrefix(r.URL.Path, "/play/") || r.URL.Query().Get("sig") != "a/b" {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	if !strings.HasPrefix(r.URL.Path, "/play/"+tok+"/") { // only the newest token is good
		p.stale++
		p.mu.Unlock()
		http.Error(w, "token expired", 403)
		return
	}
	p.streams++
	conn := p.streams
	p.open++
	p.most = max(p.most, p.open)
	if conn == 1 {
		p.began = time.Now()
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.open--
		p.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Server", "video-edge-7") // (says something about the provider: not for the player)
	for {
		p.mu.Lock()
		if p.step >= len(p.plan) {
			p.mu.Unlock()
			break
		}
		if p.sc.drop > 0 && conn == 1 && time.Since(p.began) >= p.sc.drop {
			p.mu.Unlock()
			return // the video server ends the connection, between two lumps
		}
		s, from := p.plan[p.step], p.off
		p.step++
		p.off += s.n
		p.mu.Unlock()
		select {
		case <-time.After(s.wait):
		case <-r.Context().Done():
			return
		}
		if _, err := w.Write(p.data[from : from+s.n]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		p.mu.Lock()
		p.sent = append(p.sent, roughMark{time.Now(), from + s.n})
		p.mu.Unlock()
		p.lw.sent(from + s.n)
	}
	p.once.Do(func() { close(p.finished) })
	<-r.Context().Done() // (the connection stays open, as a live channel's does)
}

func (p *liveRedirProv) counts() (asks [3]int, streams, open, most, stale int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for n := range p.asks {
		asks[n] = len(p.asks[n])
	}
	return asks, p.streams, p.open, p.most, p.stale
}

func liveRedirScenes() []liveRedirScene {
	play := func(tok, path string) string { return "/play/" + tok + "/" + channelID(path) + ".ts?e=1&sig=a%2Fb" }
	return []liveRedirScene{
		{name: "to another server, the address in full", final: 2, hops: [3]int{1, 0, 1},
			where: func(n int, path, own, mid, video, tok string) (int, string) {
				if n == 0 {
					return 302, video + play(tok, path)
				}
				return 0, ""
			}},
		{name: "to another address on its own server, by path", final: 0, hops: [3]int{2, 0, 0},
			where: func(n int, path, own, mid, video, tok string) (int, string) {
				if strings.HasPrefix(path, "/live/") {
					return 302, play(tok, path)
				}
				return 0, ""
			}},
		{name: "written relative to the address asked", final: 0, hops: [3]int{2, 0, 0},
			where: func(n int, path, own, mid, video, tok string) (int, string) {
				if strings.HasPrefix(path, "/live/") { // (from /live/<login>/<n>.ts, three folders up)
					return 302, "../../.." + play(tok, path)
				}
				return 0, ""
			}},
		{name: "to another server, written with no scheme", final: 2, hops: [3]int{1, 0, 1},
			where: func(n int, path, own, mid, video, tok string) (int, string) {
				if n == 0 {
					return 301, strings.TrimPrefix(video, "http:") + play(tok, path)
				}
				return 0, ""
			}},
		{name: "sent on twice", final: 2, hops: [3]int{1, 1, 1},
			where: func(n int, path, own, mid, video, tok string) (int, string) {
				switch n {
				case 0:
					return 301, mid + "/gate/" + tok + "/" + channelID(path)
				case 1:
					return 307, video + play(tok, "/"+channelID(path))
				}
				return 0, ""
			}},
		{name: "a new address at every ask, and the video server ends the connection", final: 2, hops: [3]int{2, 0, 2},
			drop: time.Duration(reopenAfter+1) * time.Second,
			where: func(n int, path, own, mid, video, tok string) (int, string) {
				if n == 0 {
					return 302, video + play(tok, path)
				}
				return 0, ""
			}},
	}
}

func TestLiveTSProviderSendsTheChannelElsewhere(t *testing.T) {
	const agent = "TestPlayer/4.2 (Linux; Android 12)"
	ms := time.Millisecond
	for i, sc := range liveRedirScenes() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			var plan []roughStep
			if sc.drop == 0 { // an opening burst, then lumps with pauses, one of them long
				plan = []roughStep{{0, 3 << 20}}
				for k := 0; k < 6; k++ {
					w := 700 * ms
					if k == 3 {
						w = 3000 * ms
					}
					plan = append(plan, roughStep{w, 600 << 10})
				}
			} else { // whole packets, a second apart, until well past the drop
				plan = []roughStep{{0, 188 * 8000}}
				for k := 0; k < int(sc.drop.Seconds())+6; k++ {
					plan = append(plan, roughStep{1000 * ms, 188 * 1400})
				}
			}
			id := fmt.Sprint(951 + i)
			p, base := newLiveRedirProv(t, sc, id, plan)
			note := &heldNote{}
			p.lw = startLateWatch(roughLate, note)
			defer p.lw.stop()
			where := func() string {
				if s := note.Load(); s != "" {
					return "\n\n" + s
				}
				return ""
			}

			// The device: one request, redirects not followed (devClient follows none).
			asked := time.Now()
			req, _ := http.NewRequest("GET", base+"/live/tv/easy/"+id+".ts", nil)
			req.Header.Set("X-Dev", "192.0.2.50")
			req.Header.Set("User-Agent", agent)
			r, err := devClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			if r.StatusCode != 200 || r.Header.Get("Location") != "" {
				t.Fatalf("the player asked for the channel and was answered %d (Location %q); it must be answered 200 with the video: the provider's redirect is the proxy's to follow, inside the VPN",
					r.StatusCode, r.Header.Get("Location"))
			}
			if kind := r.Header.Get("Content-Type"); kind != "video/mp2t" {
				t.Errorf("the channel's Content-Type is %q, the video server sent video/mp2t", kind)
			}
			if s := r.Header.Get("Server"); s != "" {
				t.Errorf("the player was told the video server's own name (Server: %s)", s)
			}
			p.lw.got(0)
			var gmu sync.Mutex
			var got []roughMark
			var read []byte
			first := time.Time{}
			done := make(chan struct{})
			go func() {
				defer close(done)
				buf := make([]byte, 256<<10)
				for {
					k, err := r.Body.Read(buf)
					if k > 0 {
						gmu.Lock()
						if first.IsZero() {
							first = time.Now()
						}
						read = append(read, buf[:k]...)
						got = append(got, roughMark{time.Now(), len(read)})
						gmu.Unlock()
						p.lw.gotMore(k)
					}
					if err != nil {
						return
					}
				}
			}()
			// A second device comes to the channel a second in: it costs the provider nothing.
			joined := make(chan string, 1)
			go func() {
				time.Sleep(time.Second)
				req, _ := http.NewRequest("GET", base+"/live/tv/easy/"+id+".ts", nil)
				req.Header.Set("X-Dev", "192.0.2.51")
				j, err := devClient.Do(req)
				if err != nil {
					joined <- err.Error()
					return
				}
				defer j.Body.Close()
				buf := make([]byte, 64<<10)
				n, _ := j.Body.Read(buf)
				if j.StatusCode != 200 || n == 0 {
					joined <- fmt.Sprintf("status %d, %d bytes", j.StatusCode, n)
					return
				}
				joined <- ""
			}()
			select {
			case <-p.finished:
			case <-time.After(sc.drop + 60*time.Second):
				asks, streams, _, _, stale := p.counts()
				t.Fatalf("the video server never got to send everything: the channel was no longer being read (asks at the provider's own server, the one in between and the video server: %v; video connections: %d; asks under a token no longer good: %d)%s",
					asks, streams, stale, where())
			}
			if why := <-joined; why != "" {
				t.Errorf("the second device on the channel: %s", why)
			}
			time.Sleep(1500 * time.Millisecond) // (the last bytes are on their way)
			asks, streams, _, most, stale := p.counts()
			r.Body.Close()
			<-done
			gmu.Lock()
			p.mu.Lock()
			sent := append([]roughMark(nil), p.sent...)
			agents := p.agents
			p.mu.Unlock()
			if len(sent) == 0 || len(got) == 0 {
				t.Fatalf("nothing was sent (%d lumps) or nothing arrived (%d reads)", len(sent), len(got))
			}
			roughCheck(t, p.data, sent, got, read, roughLate)
			if tune := first.Sub(asked); tune > liveRedirTune {
				t.Errorf("the first video reached the player %.1f s after it asked; the provider and its video server answer at once, so at most %.1f s is allowed%s",
					tune.Seconds(), liveRedirTune.Seconds(), where())
			} else {
				t.Logf("first video %d ms after the player asked", tune.Milliseconds())
			}
			gmu.Unlock()
			// What the provider was asked.
			if asks != sc.hops {
				t.Errorf("the provider's own server, the one in between and the video server were asked %v times, want %v: two devices on one channel are one open of it", asks, sc.hops)
			}
			want := 1
			if sc.drop > 0 {
				want = 2
			}
			if streams != want || most != 1 {
				t.Errorf("the video server answered %d video connections, at most %d at once; want %d, one at a time", streams, most, want)
			}
			if stale != 0 {
				t.Errorf("the video server was asked %d times under a token that was no longer good: after a connection ends, the channel is asked for again at its own address, where a new token is handed out", stale)
			}
			for n, list := range agents {
				for k, a := range list {
					if a != agent {
						t.Errorf("server %d, ask %d came with User-Agent %q; the player's own is %q, and it goes with every hop", n, k+1, a, agent)
					}
				}
			}
			// Once the last device has hung up, the video connection ends and nothing more is asked.
			gone := false
			for end := time.Now().Add(3 * time.Second); time.Now().Before(end) && !gone; time.Sleep(20 * time.Millisecond) {
				_, _, open, _, _ := p.counts()
				gone = open == 0
			}
			if !gone {
				t.Errorf("3 s after the last device hung up, the video server's connection is still open")
			}
			time.Sleep(1200 * time.Millisecond)
			if after, _, _, _, _ := p.counts(); after != asks {
				t.Errorf("after the last device hung up the provider was asked again: %v asks, then %v", asks, after)
			}
		})
	}
}
