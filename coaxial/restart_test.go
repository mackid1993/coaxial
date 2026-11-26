package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The proxy part is a program of its own, and the launcher stops it and starts it again: after a save that
// changes a provider, after a VPN change, after it stopped by itself. AGENTS.md rule 10 says not to do that
// under a viewer, and it still happens (a save is the user's own doing). Everything the proxy part knows
// about what plays is in its memory, so a viewer's next request finds a program that has never heard of it.
// No test ever asked a freshly started proxy part for an address handed out by the one before it.
//
// Here the proxy part really is another program: the test program started again in its place (as the
// launcher starts "coaxial proxy": settings on its standard input, stopped with SIGTERM), in front of a
// stand-in provider that runs in the test. Devices are told apart by their address, as in real use (the
// loopback network has many: 127.0.0.11, 127.0.0.12). The provider allows one connection, so anything left
// counted, or anything not counted that should be, shows as a refusal or as two connections at the provider.
//
// For each kind of viewer: what its next request gets, what the provider is asked, what the status list
// shows, and that the limit holds again.

// The test program run with COAXIAL_TEST_PROXY_PART set is the proxy part, as "coaxial proxy" is (see main).
func init() {
	if os.Getenv("COAXIAL_TEST_PROXY_PART") == "" {
		return
	}
	stop, sig := make(chan struct{}), make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sig; close(stop) }()
	code := runProxy(os.Stdin, stop)
	logFlush()
	os.Exit(code)
}

type proxyPart struct {
	t          *testing.T
	conf, port string
	dir        string
	cmd        *exec.Cmd
	out        *syncBuf
	done       chan struct{}
}

// newProxyPart: a proxy part in front of the provider at provURL (one connection allowed), started.
func newProxyPart(t *testing.T, provURL, mode string) *proxyPart {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	pp := &proxyPart{t: t, port: port, dir: t.TempDir(), out: &syncBuf{}}
	pp.conf = `{"player":{"user":"tv","password":"easy"},"providers":[{"name":"One","url":"` + provURL +
		`","user":"realU","password":"realP","port":` + port + `,"connections":1,"live_tv":"` + mode + `"}]}`
	t.Cleanup(func() {
		if pp.cmd != nil {
			pp.cmd.Process.Kill()
			<-pp.done
		}
		if t.Failed() {
			lines := strings.Split(strings.TrimSpace(pp.out.String()), "\n")
			t.Logf("the proxy part's own log (its last lines):\n%s", strings.Join(lines[max(0, len(lines)-60):], "\n"))
		}
	})
	pp.start()
	return pp
}

func (pp *proxyPart) start() {
	pp.t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(pp.conf), pp.out, pp.out
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "COAXIAL_TEST_PROXY_PART=1", "COAXIAL_NAMES_FILE=" + filepath.Join(pp.dir, "names.json")}
	if err := cmd.Start(); err != nil {
		pp.t.Fatalf("the proxy part could not be started: %v", err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	pp.cmd, pp.done = cmd, done
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if c, err := net.Dial("tcp", "127.0.0.1:"+pp.port); err == nil {
			c.Close()
			return
		}
		select {
		case <-done:
			pp.t.Fatalf("the proxy part ended as soon as it was started:\n%s", pp.out.String())
		default:
		}
		if time.Now().After(end) {
			pp.t.Fatalf("the proxy part did not open its port in 15 s:\n%s", pp.out.String())
		}
	}
}

// stop: as the launcher stops it (SIGTERM, 3 s to end).
func (pp *proxyPart) stop() {
	pp.t.Helper()
	pp.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-pp.done:
	case <-time.After(3 * time.Second):
		pp.cmd.Process.Kill()
		<-pp.done
		pp.t.Errorf("told to stop, the proxy part was still running 3 s later and had to be killed")
	}
	pp.cmd = nil
}

func (pp *proxyPart) base() string { return "http://127.0.0.1:" + pp.port }

type restartRow struct {
	Device  string `json:"device"`
	Channel string `json:"channel_id"`
	Format  string `json:"format"`
	Seconds int64  `json:"seconds"`
}

// status: what the proxy part tells the launcher is playing (the rows of the status list), and how many
// channels it counts as open.
func (pp *proxyPart) status() (rows []restartRow, open int) {
	pp.t.Helper()
	data, ok := askProxy("/status", 10*time.Second)
	var st struct {
		Providers []struct {
			Open int `json:"channels_open"`
		}
		Playing []restartRow
	}
	if !ok || json.Unmarshal(data, &st) != nil || len(st.Providers) != 1 {
		pp.t.Fatalf("the proxy part's status: %v %s", ok, data)
	}
	return st.Playing, st.Providers[0].Open
}

// wantRows: the status list holds exactly one row, this one.
func (pp *proxyPart) wantRows(when, dev, channel, format string) {
	pp.t.Helper()
	rows, _ := pp.status()
	if len(rows) != 1 || rows[0].Device != dev || rows[0].Channel != channel || rows[0].Format != format {
		pp.t.Errorf("%s the status list shows %+v; want one row: %s on channel %s as %s", when, rows, dev, channel, format)
	} else if rows[0].Seconds > 60 {
		pp.t.Errorf("%s the row says %d s", when, rows[0].Seconds)
	}
}

// restartDev: a device at an address of its own.
func restartDev(ip string) *http.Client {
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}, Timeout: 5 * time.Second}
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: d.DialContext}, Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

const restartA, restartB = "127.0.0.11", "127.0.0.12"

// restartPlay: one ask the way a player makes it (sent elsewhere, it follows, on the proxy only), read whole.
func restartPlay(t *testing.T, c *http.Client, addr string, hdr ...string) (status int, data []byte, h http.Header, at string, took time.Duration) {
	t.Helper()
	home, _ := url.Parse(addr)
	began := time.Now()
	for hop := 0; hop < 6; hop++ {
		u, err := url.Parse(addr)
		if err != nil || u.Host != home.Host {
			t.Fatalf("the player was given the address %q, which does not lead to the proxy (%s)", addr, home.Host)
		}
		req, _ := http.NewRequest("GET", addr, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := c.Do(req)
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
		data, _ = io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // (a stream that was not expected ends here too)
		resp.Body.Close()
		return resp.StatusCode, data, resp.Header, addr, time.Since(began)
	}
	t.Fatalf("the player was sent from one address to the next 6 times and never answered")
	return
}

// restartRefused: the other device asks for something of its own and is turned away: the one connection the
// account allows is counted as in use.
func restartRefused(t *testing.T, when, addr string) {
	t.Helper()
	resp, err := restartDev(restartB).Get(addr)
	if err != nil {
		t.Fatalf("%s another device asked for a channel of its own: %v", when, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096)) // (let in, it is sent a stream with no end)
	if resp.StatusCode != 503 || !bytes.Contains(data, []byte("connection")) {
		t.Errorf("%s another device asked for a channel of its own and was answered %d; the account allows one connection and it is in use, so it must be turned away (503): the viewer's stream is not counted",
			when, resp.StatusCode)
	}
}

// restartPiece: an HLS piece, 75 kB, every packet its own.
func restartPiece(n int) []byte {
	out := make([]byte, 0, 188*400)
	for i := uint64(0); i < 400; i++ {
		p := make([]byte, 188)
		p[0], p[1], p[2], p[3] = 0x47, 0x1f, 0xff, 0x10|byte(i&0x0f)
		binary.BigEndian.PutUint64(p[4:], uint64(n)<<32|i)
		for k := 12; k < 188; k++ {
			p[k] = byte(uint64(n)*131 + i*31 + uint64(k)*7)
		}
		out = append(out, p...)
	}
	return out
}

func restartWait(t *testing.T, limit time.Duration, cond func() bool, what string) {
	t.Helper()
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Errorf("%s (waited %.0f s)", what, limit.Seconds())
}

// A device watches a channel as MPEG-TS when the proxy part is restarted. Its stream ends at once (a player
// asks again by itself), the provider's connection is let go before anything new is opened, and the player's
// next ask plays. Nothing of the old stream is counted by the new proxy part, and the new stream is.
func TestRestartUnderAnMPEGTSViewer(t *testing.T) {
	fp := newFakeProv(t, false, nil) // (its live channels send MPEG-TS for as long as they are read)
	pp := newProxyPart(t, fp.srv.URL, "ts")
	a := restartDev(restartA)
	r, err := a.Get(pp.base() + "/live/tv/easy/7.ts")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("the channel: %v %v", r, err)
	}
	defer r.Body.Close()
	if _, err := io.ReadFull(r.Body, make([]byte, 1<<20)); err != nil {
		t.Fatalf("the channel's video: %v", err)
	}
	pp.wantRows("before the restart", restartA, "7", "MPEG-TS")
	restartRefused(t, "before the restart", pp.base()+"/live/tv/easy/8.ts")

	stopped := time.Now()
	ended := make(chan time.Duration, 1)
	go func() { io.Copy(io.Discard, r.Body); ended <- time.Since(stopped) }()
	pp.stop()
	select {
	case took := <-ended:
		t.Logf("the viewer's stream ended %d ms after the proxy part was told to stop", took.Milliseconds())
	case <-time.After(5 * time.Second):
		t.Fatalf("5 s after the proxy part stopped, the viewer's stream has not ended: its player waits on a dead connection instead of asking again")
	}
	restartWait(t, 3*time.Second, func() bool { return fp.count(fp.open, "") == 0 },
		"the provider's connection for the channel is still open after the proxy part stopped")
	pp.start()

	// Nothing is left counted from before: another device may use the account's one connection now.
	b := restartDev(restartB)
	rb, err := b.Get(pp.base() + "/live/tv/easy/8.ts")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := io.ReadFull(rb.Body, make([]byte, 64<<10)); rb.StatusCode != 200 || n == 0 {
		t.Errorf("right after the restart, with nothing playing, another device asked for a channel and got %d, %d bytes: the stream from before the restart is still counted", rb.StatusCode, n)
	}
	rb.Body.Close()
	restartWait(t, 3*time.Second, func() bool { return fp.count(fp.open, "") == 0 }, "the other device hung up and its channel is still open at the provider")

	// The viewer's player asks again.
	began := time.Now()
	r2, err := a.Get(pp.base() + "/live/tv/easy/7.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("the viewer's player asked for its channel again after the restart: %d %q", r2.StatusCode, body(r2))
	}
	if _, err := io.ReadFull(r2.Body, make([]byte, 1<<20)); err != nil || time.Since(began) > 3*time.Second {
		t.Errorf("the channel asked for again: 1 MB of video took %.1f s (%v)", time.Since(began).Seconds(), err)
	}
	pp.wantRows("after the restart", restartA, "7", "MPEG-TS")
	if _, open := pp.status(); open != 1 {
		t.Errorf("after the restart the proxy part counts %d channels open, want 1", open)
	}
	restartRefused(t, "after the restart", pp.base()+"/live/tv/easy/8.ts")
	if n, most := fp.count(fp.hits, "/live/realU/realP/7.ts"), fp.count(fp.most, ""); n != 2 || most != 1 {
		t.Errorf("the provider was asked for the viewer's channel %d times and had %d connections open at once; want 2 asks (one before the restart, one after) and never more than 1 connection, which is all the account allows", n, most)
	}
}

// restartHLS: a provider of live HLS as real ones are: every ask at a channel's own address opens a new session
// (a new token in the address it sends the player on to), and only the newest session answers.
type restartHLS struct {
	prov, cdn *fakeProv
	opens     atomic.Int32 // sessions opened for channel 44
	lists     atomic.Int32 // playlist asks
}

func newRestartHLS(t *testing.T) *restartHLS {
	h := &restartHLS{}
	h.cdn = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		var tok, n int
		if _, err := fmt.Sscanf(r.URL.Path, "/hlsr/tok%d/", &tok); err != nil {
			return false
		}
		if int32(tok) != h.opens.Load() {
			http.Error(w, "session gone", 404)
			return true
		}
		if strings.HasSuffix(r.URL.Path, "/index.m3u8") {
			n = 100 + int(h.lists.Add(1)) // (each ask finds the playlist one piece further on)
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:4,\np%d.ts\n#EXTINF:4,\np%d.ts\n#EXTINF:4,\np%d.ts\n", n, n, n+1, n+2)
			return true
		}
		if _, err := fmt.Sscanf(r.URL.Path[strings.LastIndexByte(r.URL.Path, '/'):], "/p%d.ts", &n); err != nil {
			return false
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(restartPiece(n))
		return true
	})
	h.prov = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/live/realU/realP/") || !strings.HasSuffix(r.URL.Path, ".m3u8") {
			return false
		}
		http.Redirect(w, r, fmt.Sprintf("%s/hlsr/tok%d/index.m3u8", h.cdn.srv.URL, h.opens.Add(1)), 302)
		return true
	})
	return h
}

// A device watches a channel as HLS, on a provider set to HLS, when the proxy part is restarted. The pieces
// its player still has to fetch are fetched (their addresses name the video server, and the session they
// belong to is still the provider's newest). Its next reload of the playlist, at the address it was given
// before the restart, opens the channel at the provider once more, and from then on that session is used:
// the playlist carries on, and the channel is not opened at every reload.
func TestRestartUnderAnHLSViewer(t *testing.T) {
	h := newRestartHLS(t)
	pp := newProxyPart(t, h.prov.srv.URL, "hls")
	a := restartDev(restartA)
	status, data, _, at, _ := restartPlay(t, a, pp.base()+"/live/tv/easy/44.m3u8")
	seq, names := tsHLSList(string(data))
	if status != 200 || len(names) != 3 {
		t.Fatalf("the channel as HLS: %d %q", status, data)
	}
	listAt, _ := url.Parse(at)
	piece := func(when, name string, n int) {
		t.Helper()
		u, _ := listAt.Parse(name)
		st, got, _, _, took := restartPlay(t, a, u.String())
		if st != 200 || !bytes.Equal(got, restartPiece(n)) || took > 3*time.Second {
			t.Errorf("%s piece %d: status %d, %d bytes after %.1f s; want the provider's %d bytes at once", when, n, st, len(got), took.Seconds(), 188*400)
		}
	}
	piece("before the restart,", names[0], seq)
	pp.wantRows("before the restart", restartA, "44", "HLS")
	restartRefused(t, "before the restart", pp.base()+"/live/tv/easy/45.ts")
	if n := h.opens.Load(); n != 1 {
		t.Fatalf("the channel was opened %d times before the restart, want 1", n)
	}
	oldSeq, oldNames := seq, names

	pp.stop()
	pp.start()

	// The pieces the player had still to fetch: named before the restart, fetched after it.
	piece("after the restart, a piece named before it:", oldNames[1], oldSeq+1)
	piece("after the restart, a piece named before it:", oldNames[2], oldSeq+2)
	if n := h.opens.Load(); n != 1 {
		t.Errorf("fetching pieces named before the restart opened the channel at the provider again (%d opens, want 1): the session those pieces belong to is ended by it", n)
	}
	// The reload, at the address the player was given before the restart.
	asked := h.lists.Load()
	status, data, _, _, took := restartPlay(t, a, at)
	seq, names = tsHLSList(string(data))
	if status != 200 || len(names) != 3 || took > 5*time.Second {
		t.Fatalf("the playlist asked for again after the restart, at the address handed out before it: %d after %.1f s, %q", status, took.Seconds(), data)
	}
	if n := h.opens.Load(); n != 2 {
		t.Errorf("the channel was opened at the provider %d times in all, want 2: once before the restart and once for the first reload after it", n)
	}
	if seq <= oldSeq {
		t.Errorf("the playlist after the restart begins at piece %d; before it, it began at %d: the player is sent back", seq, oldSeq)
	}
	for i, name := range names {
		piece("after the restart,", name, seq+i)
	}
	for i := 0; i < 4; i++ {
		status, data, _, _, _ = restartPlay(t, a, at)
		next, names := tsHLSList(string(data))
		if status != 200 || next != seq+1 || len(names) != 3 {
			t.Fatalf("reload %d after the restart: %d, begins at piece %d, want the provider's next playlist (piece %d)\n%s", i+2, status, next, seq+1, data)
		}
		seq = next
	}
	if n, lists := h.opens.Load(), h.lists.Load()-asked; n != 2 || lists != 5 {
		t.Errorf("after five reloads the channel was opened at the provider %d times in all (want 2) and its playlist was asked for %d times since the restart (want 5, one per reload)", n, lists)
	}
	// A piece of the session from before the restart: the provider's own answer for it, at once.
	u, _ := listAt.Parse(oldNames[0])
	st, _, _, _, took := restartPlay(t, a, u.String())
	if st != 404 || took > 3*time.Second {
		t.Errorf("a piece of the session the provider has ended: %d after %.1f s; the provider answers 404 for it, and that is passed on at once", st, took.Seconds())
	}
	pp.wantRows("after the restart", restartA, "44", "HLS")
	if _, open := pp.status(); open != 1 {
		t.Errorf("after the restart the proxy part counts %d channels open, want 1", open)
	}
	restartRefused(t, "after the restart", pp.base()+"/live/tv/easy/45.ts")
}

// A device watches a channel as HLS on a provider set to MPEG-TS (the playlist and its pieces are made in the
// proxy part, tshls.go) when the proxy part is restarted. The pieces from before are gone with it: asked for,
// they are answered 404 at once and the provider is not asked anything. The reload of the playlist, at the
// address from before the restart, opens the channel once and is answered as soon as the provider's opening
// burst has brought two pieces; its numbers carry on past the old ones (a player takes a lower number for a
// piece it has had), and its first piece is marked as not following on from what the player has.
func TestRestartUnderAnHLSViewerOfAnMPEGTSProvider(t *testing.T) {
	p := &tsHLSProv{s: wreckBuild(11, wreckPart{secs: 60, clock: 1000}), burst: 14,
		asks: map[string]int{}, open: map[string]int{}, closed: map[string]time.Time{}}
	var omu sync.Mutex
	open, most := 0, 0
	fp := newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/live/realU/realP/") || !strings.HasSuffix(r.URL.Path, ".ts") {
			return false
		}
		omu.Lock()
		open++
		most = max(most, open)
		omu.Unlock()
		defer func() { omu.Lock(); open--; omu.Unlock() }()
		p.serve(w, r)
		return true
	})
	pp := newProxyPart(t, fp.srv.URL, "ts")
	a := restartDev(restartA)
	asks := func() int { p.mu.Lock(); defer p.mu.Unlock(); return p.asks["7"] }
	status, data, _, at, _ := restartPlay(t, a, pp.base()+"/live/tv/easy/7.m3u8")
	if status != 200 || !bytes.Contains(data, []byte("#EXTINF")) {
		t.Fatalf("the channel as HLS: %d %q", status, data)
	}
	listAt, _ := url.Parse(at)
	lastSeq, oldName := -1, ""
	// follow: the playlist once a second for a while, every new piece fetched. The numbering may not go back
	// or leave a hole.
	follow := func(when string, d time.Duration) {
		t.Helper()
		for end := time.Now().Add(d); ; time.Sleep(time.Second) {
			seq, names := tsHLSList(string(data))
			if status != 200 || seq < 0 || len(names) == 0 || strings.Contains(string(data), "#EXT-X-ENDLIST") {
				t.Fatalf("%s: playlist %d\n%s", when, status, data)
			}
			if lastSeq >= 0 && (seq+len(names)-1 < lastSeq || seq > lastSeq+1) {
				t.Fatalf("%s: the playlist names pieces %d to %d; the one before it ended at piece %d", when, seq, seq+len(names)-1, lastSeq)
			}
			for i, name := range names {
				if seq+i <= lastSeq {
					continue
				}
				u, _ := listAt.Parse(name)
				st, piece, _, _, _ := restartPlay(t, a, u.String())
				pid := func(k int) int { return int(piece[k*188+1]&0x1f)<<8 | int(piece[k*188+2]) }
				if st != 200 || len(piece) < 3*188 || len(piece)%188 != 0 || pid(0) != 0 || pid(1) != 0x1000 || pid(2) != 0x100 || piece[2*188+5]&0x40 == 0 {
					t.Fatalf("%s: piece %d: status %d, %d bytes; want whole packets that start with the program tables and a keyframe", when, seq+i, st, len(piece))
				}
				lastSeq, oldName = seq+i, u.String()
			}
			if time.Now().After(end) {
				return
			}
			status, data, _, _, _ = restartPlay(t, a, at)
		}
	}
	follow("before the restart", 8*time.Second)
	pp.wantRows("before the restart", restartA, "7", "HLS")
	restartRefused(t, "before the restart", pp.base()+"/live/tv/easy/8.ts")
	before := lastSeq

	pp.stop()
	restartWait(t, 3*time.Second, func() bool { omu.Lock(); defer omu.Unlock(); return open == 0 },
		"the provider's connection for the channel is still open after the proxy part stopped")
	pp.start()

	st, _, _, _, took := restartPlay(t, a, oldName)
	if st != 404 || took > 2*time.Second {
		t.Errorf("a piece from before the restart: %d after %.1f s; it is gone with the proxy part that made it, so 404 at once", st, took.Seconds())
	}
	time.Sleep(300 * time.Millisecond) // (an ask it set off would be on its way)
	if n := asks(); n != 1 {
		t.Errorf("asking for a piece from before the restart had the channel asked for at the provider (%d asks, want 1): a piece never opens a channel", n)
	}
	var tookList time.Duration
	status, data, _, _, tookList = restartPlay(t, a, at)
	seq, _ := tsHLSList(string(data))
	if status != 200 || tookList > 5*time.Second {
		t.Fatalf("the playlist asked for again after the restart, at the address handed out before it: %d after %.1f s\n%s", status, tookList.Seconds(), data)
	}
	if seq <= before {
		t.Errorf("the playlist after the restart begins at piece %d; the player has had pieces up to %d: a player takes a lower number for a piece it has had, and waits", seq, before)
	}
	first := strings.Index(string(data), "#EXTINF")
	if disc := strings.Index(string(data), "#EXT-X-DISCONTINUITY\n"); disc < 0 || disc > first {
		t.Errorf("the first piece after the restart is not marked as not following on (#EXT-X-DISCONTINUITY before it): the provider's new connection starts on its own place in the channel\n%s", data)
	}
	lastSeq = seq - 1
	follow("after the restart", 4*time.Second)
	if n := asks(); n != 2 {
		t.Errorf("the provider was asked for the channel %d times in all, want 2: once before the restart and once after", n)
	}
	omu.Lock()
	if most != 1 {
		t.Errorf("the provider had %d connections open at once; the account allows one", most)
	}
	omu.Unlock()
	pp.wantRows("after the restart", restartA, "7", "HLS")
	if _, n := pp.status(); n != 1 {
		t.Errorf("after the restart the proxy part counts %d channels open, want 1", n)
	}
	restartRefused(t, "after the restart", pp.base()+"/live/tv/easy/8.ts")
}

// A device watches a catch-up (an hour of a channel, one file at the provider) when the proxy part is
// restarted. As MPEG-TS: its read breaks off (it must not end as if the file were whole), the provider's read
// is let go, and the player carries on with a Range from where it stopped: the bytes of that place, and the
// file comes out whole. While it reads, before the restart and after it, the account's one connection is
// counted as in use; once it is done, it is not.
func TestRestartUnderACatchUpViewer(t *testing.T) {
	const size = 24 << 20
	film := make([]byte, size)
	for i := range film {
		film[i] = byte(i*7 + i>>9 + i>>17)
	}
	var mu sync.Mutex
	open, most, reads := 0, 0, 0
	fp := newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/timeshift/realU/realP/60/2026-01-01:10-00/2.ts" {
			return false
		}
		mu.Lock()
		open++
		reads++
		most = max(most, open)
		mu.Unlock()
		defer func() { mu.Lock(); open--; mu.Unlock() }()
		from, status := 0, 200
		if v, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok {
			from, _ = strconv.Atoi(strings.TrimSuffix(v, "-"))
			status = 206
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, size-1, size))
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(size-from))
		w.WriteHeader(status)
		for at := from; at < size; at += 64 << 10 { // (about 2.5 MB a second: the read runs for some 9 s)
			if _, err := w.Write(film[at:min(size, at+64<<10)]); err != nil {
				return true
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return true
			case <-time.After(25 * time.Millisecond):
			}
		}
		return true
	})
	pp := newProxyPart(t, fp.srv.URL, "ts")
	a := restartDev(restartA)
	const path = "/timeshift/tv/easy/60/2026-01-01:10-00/2.ts"
	playing := func() string { data, _ := askProxy("/playing", 5*time.Second); return string(data) }
	r, err := a.Get(pp.base() + path)
	if err != nil || r.StatusCode != 200 || r.ContentLength != size {
		t.Fatalf("the catch-up: %v %v", r, err)
	}
	defer r.Body.Close()
	got := make([]byte, 0, size)
	buf := make([]byte, 64<<10)
	for len(got) < 1<<20 {
		n, err := r.Body.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatalf("the catch-up broke off after %d bytes: %v", len(got), err)
		}
	}
	if s := playing(); !strings.Contains(s, restartA) {
		t.Errorf("before the restart, what the proxy part says is playing does not name the catch-up's device: %s", s)
	}
	restartRefused(t, "before the restart, while the catch-up is read,", pp.base()+"/live/tv/easy/8.ts")
	stopped := time.Now()
	pp.stop()
	var rerr error
	for rerr == nil {
		var n int
		n, rerr = r.Body.Read(buf)
		got = append(got, buf[:n]...)
	}
	if rerr == io.EOF || len(got) >= size {
		t.Fatalf("the proxy part was stopped %d bytes into a catch-up of %d, and the read ended as if the file were whole (%v): the player takes a short file for the whole programme", len(got), size, rerr)
	}
	if d := time.Since(stopped); d > 4*time.Second {
		t.Errorf("the catch-up's read broke off %.1f s after the proxy part was told to stop", d.Seconds())
	}
	restartWait(t, 3*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return open == 0 },
		"the provider's read of the catch-up is still open after the proxy part stopped")
	pp.start()

	req, _ := http.NewRequest("GET", pp.base()+path, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", len(got)))
	r2, err := a.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if want := fmt.Sprintf("bytes %d-%d/%d", len(got), size-1, size); r2.StatusCode != 206 || r2.Header.Get("Content-Range") != want {
		t.Fatalf("the player carried on after the restart with Range bytes=%d-: answered %d, Content-Range %q; want 206 and %q", len(got), r2.StatusCode, r2.Header.Get("Content-Range"), want)
	}
	at := len(got)
	for len(got) < at+(1<<20) {
		n, err := r2.Body.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatalf("the catch-up carried on with broke off after %d bytes: %v", len(got), err)
		}
	}
	if s := playing(); !strings.Contains(s, restartA) {
		t.Errorf("after the restart, what the proxy part says is playing does not name the catch-up's device: %s", s)
	}
	restartRefused(t, "after the restart, while the catch-up is read,", pp.base()+"/live/tv/easy/8.ts")
	rest, err := io.ReadAll(r2.Body)
	got = append(got, rest...)
	if err != nil || !bytes.Equal(got, film) {
		t.Errorf("the catch-up read in two parts, around the restart: %d bytes (%v); want the provider's %d bytes exactly, joined at byte %d", len(got), err, size, at)
	}
	// Done: the connection is free again.
	restartWait(t, 3*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return open == 0 }, "the catch-up was read to its end and its read is still open at the provider")
	b := restartDev(restartB)
	rb, err := b.Get(pp.base() + "/live/tv/easy/8.ts")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := io.ReadFull(rb.Body, make([]byte, 64<<10)); rb.StatusCode != 200 || n == 0 {
		t.Errorf("the catch-up is over, and another device asking for a channel got %d, %d bytes: the catch-up is still counted", rb.StatusCode, n)
	}
	rb.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if most != 1 || reads != 2 {
		t.Errorf("the provider saw %d reads of the catch-up, %d at once; want 2 (one before the restart, one after), one at a time", reads, most)
	}
}

// The same catch-up watched as HLS: a complete playlist, asked for once, whose pieces are named in full at the
// address the player used. The player holds the playlist; after the restart it asks for the next pieces at the
// addresses it has, and they are served, byte for byte, without the provider's own address being asked again.
func TestRestartUnderAnHLSCatchUpViewer(t *testing.T) {
	var asked atomic.Int32
	cdn := newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		var n int
		switch {
		case r.URL.Path == "/arch/2/index.m3u8":
			text := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:0\n"
			for n := 0; n < 4; n++ {
				text += fmt.Sprintf("#EXTINF:10.000,\na%d.ts\n", n)
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, text+"#EXT-X-ENDLIST\n")
		case strings.HasPrefix(r.URL.Path, "/arch/2/a"):
			if _, err := fmt.Sscanf(r.URL.Path, "/arch/2/a%d.ts", &n); err != nil {
				return false
			}
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(restartPiece(5000 + n))
		default:
			return false
		}
		return true
	})
	prov := newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/timeshift/realU/realP/60/2026-01-01:10-00/2.m3u8" {
			return false
		}
		asked.Add(1)
		http.Redirect(w, r, cdn.srv.URL+"/arch/2/index.m3u8", 302)
		return true
	})
	pp := newProxyPart(t, prov.srv.URL, "ts")
	a := restartDev(restartA)
	status, data, _, _, _ := restartPlay(t, a, pp.base()+"/timeshift/tv/easy/60/2026-01-01:10-00/2.m3u8")
	_, names := tsHLSList(string(data))
	if status != 200 || len(names) != 4 || !strings.Contains(string(data), "#EXT-X-ENDLIST") {
		t.Fatalf("the catch-up as HLS: %d %q", status, data)
	}
	piece := func(when string, n int) {
		t.Helper()
		if !strings.HasPrefix(names[n], pp.base()+"/") {
			t.Fatalf("the catch-up's playlist names piece %d as %q; a complete playlist names every piece in full, at the address the player used (%s)", n, names[n], pp.base())
		}
		st, got, _, _, took := restartPlay(t, a, names[n])
		if st != 200 || !bytes.Equal(got, restartPiece(5000+n)) || took > 3*time.Second {
			t.Errorf("%s piece %d: status %d, %d bytes after %.1f s; want the provider's %d bytes at once", when, n, st, len(got), took.Seconds(), 188*400)
		}
	}
	piece("before the restart,", 0)
	pp.wantRows("before the restart", restartA, "2", "HLS catch-up")

	pp.stop()
	pp.start()

	for n := 1; n < 4; n++ {
		piece("after the restart,", n)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("the provider's own address was asked for the catch-up %d times; the player asked for its playlist once, before the restart", n)
	}
	// (What the new proxy part knows of this viewer is written down, not judged: see the report.)
	rows, _ := pp.status()
	playing, _ := askProxy("/playing", 5*time.Second)
	t.Logf("after the restart, with the catch-up's pieces being read: the status list shows %+v; playing: %s", rows, bytes.TrimSpace(playing))
}
