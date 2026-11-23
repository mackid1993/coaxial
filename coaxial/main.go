// Coaxial: one program in front of your IPTV providers, run inside a VPN container.
//
// This file is the relay: the jobs a plain reverse proxy cannot do. The front door (front.go) calls these
// directly.
//
//	live             one provider MPEG-TS stream per channel, shared by every reader ("/ts"), or MPEG-TS
//	                 for recorders made from the channel's HLS, sharing the players' one HLS session ("/hls")
//	playlistRequest  HLS playlists: one provider session per live channel, every address pointed back here
//	api              player_api.php lists: logos and posters pointed back here
//	nameOK           whether a /__cdn/ server name leads to this machine or a home network
//	statusPage       the status page
//
// Same status codes and timing rules as the relay it grew from. Standard library only.
// main.go: channels and their sources. hls.go: playlists, sessions, lists. status.go: the page.
// front.go: the provider ports. settings.go: settings.json. launcher.go: the VPN, the proxy part and the
// web interface. passthrough.go: copying an answer on (two small functions from ah4c, see its notice).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	joinSeconds   = 10               // a joiner starts this far behind live: its cushion, handed over in its first seconds
	joinStart     = 2                // seconds of that cushion handed on at once, so its player can start
	keepSeconds   = 15               // what is kept of each channel for joiners...
	ringMin       = 12 << 20         // ...never less than this
	ringMax       = 64 << 20         // ...nor more than this
	burstSeconds  = 2.5              // the provider's opening burst is measured over this and logged
	reopenAfter   = 30               // a stream that ran this long and ended while watched is asked for again
	keyWait       = 5                // seconds of a stream asked for again searched for a keyframe to join at (renumber.go)
	reopenWait    = 5                // seconds to answer when asked again (twice that to send video)
	dropGap       = 0.1              // time a provider gets to see a hang-up before another channel is asked for
	linger        = 0.4              // a channel in its first seconds is kept this long after its last reader (tuning)
	readersMax    = 100              // readers served at once, all channels together
	behind        = 16 << 20         // a reader this far behind for behindSeconds is disconnected...
	behindSeconds = 15               //
	farBehind     = 64 << 20         // ...or this far behind at any moment
	connectWait   = 10 * time.Second // to accept a connection
	keepIdle      = 20 * time.Second // an idle connection to a server is kept this long for the next request
	headerWait    = 20               // seconds for an answer's heading
	playlistWait  = 30               // the same for a playlist
	startWait     = 60               // seconds a reader waits for a channel's first video
	quietWait     = 60               // seconds a stream may send nothing before it is given up on
)

var mu sync.Mutex // all shared state; held only between waits (the Python ran on one thread)

var logger = log.New(os.Stdout, "coaxial: ", log.LstdFlags|log.Lmsgprefix)

const quietNote = 20 // seconds a provider may send nothing before its carrying on is logged

func say(format string, a ...any) { logger.Printf(format, a...) } // never an address or a login

var clockStart = time.Now()

func mono() float64 { return time.Since(clockStart).Seconds() + 1e6 } // starts high so "never" (0) is long ago

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func sleep(ctx context.Context, s float64) error {
	t := time.NewTimer(secs(math.Max(0, s)))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Fetching: net/http, certificates not checked (many players do not check them either)
// ---------------------------------------------------------------------------

type refused struct { // the other side said no; carries what to tell the reader
	status int
	kind   string
	text   []byte
	after  int // seconds to ask again after (Retry-After), when known
}

func (e *refused) Error() string { return "refused " + strconv.Itoa(e.status) }

type timeoutError struct{}

func (timeoutError) Error() string { return "timed out" }
func (timeoutError) Timeout() bool { return true }

var errStop = errors.New("the feed's reader stopped")

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}

func pyName(err error) string { // the names the Python log used
	var dns *net.DNSError
	switch {
	case isTimeout(err):
		return "TimeoutError"
	case errors.As(err, &dns):
		return "gaierror"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ConnectionRefusedError"
	case errors.Is(err, syscall.ECONNRESET):
		return "ConnectionResetError"
	}
	return "OSError"
}

var privateNets = func() (out []netip.Prefix) {
	for _, n := range strings.Fields("0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.168.0.0/16 ::/127 fc00::/7 fe80::/10") {
		out = append(out, netip.MustParsePrefix(n))
	}
	return
}()

// isPrivate: this machine, a home or private network (false for anything not written as an address).
func isPrivate(host string) bool {
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	ip = ip.WithZone("").Unmap()
	for _, n := range privateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// looseNumbers: 127.1, 2130706433, 0x7f000001: the C library reads these as addresses, DNS never answers them.
var looseNumbers = regexp.MustCompile(`(?i)^(0x[0-9a-f]+|[0-9]+)(\.(0x[0-9a-f]+|[0-9]+)){0,3}$`)

// leadsHome looks a server name up and says whether any answer is this machine or a home network.
func leadsHome(ctx context.Context, host string) (bool, error) {
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return isPrivate(host), nil
	}
	if strings.EqualFold(host, "localhost") || looseNumbers.MatchString(host) {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, connectWait)
	defer cancel()
	found, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	for _, f := range found {
		if isPrivate(f.IP.String()) {
			return true, nil
		}
	}
	return false, err
}

type guardKey struct{}

// The private-address check happens at connect time, on the address actually dialed.
var dialer = &net.Dialer{Timeout: connectWait, ControlContext: func(ctx context.Context, _, addr string, _ syscall.RawConn) error {
	if host, _, _ := net.SplitHostPort(addr); ctx.Value(guardKey{}) != nil && isPrivate(host) {
		return &refused{status: 403}
	}
	return nil
}}

func newTransport(verify, keep bool) *http.Transport {
	return &http.Transport{DialContext: dialer.DialContext, TLSClientConfig: &tls.Config{InsecureSkipVerify: !verify},
		TLSHandshakeTimeout: connectWait, DisableKeepAlives: !keep, DisableCompression: true,
		MaxIdleConns: 256, MaxIdleConnsPerHost: 32, IdleConnTimeout: keepIdle,
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
}

// plainTransport: one connection per request (a live stream, a channel's first open, a list).
// sharedTransport: connections to every server are kept open and reused, by the pass-through and by the
// relay's own playlist refreshes alike; an idle one is let go after keepIdle.
var plainTransport, checkedTransport, sharedTransport = newTransport(false, false), newTransport(true, false), newTransport(false, true)

type opts struct {
	guardAll bool // every connection checked (otherwise only those a redirect leads to)
	noFollow bool
	stay     bool              // redirects may not leave the first server
	verify   bool              // check the certificate (only ipinfo.io)
	shared   bool              // over the connections kept open to each server
	via      http.RoundTripper // answered inside this program (see inside in front.go)
	wait     float64
}

// idleBody gives up when one read waits longer than wait (a stream that went quiet).
type idleBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	wait   atomic.Int64
	timed  atomic.Bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	t := time.AfterFunc(time.Duration(b.wait.Load()), func() { b.timed.Store(true); b.cancel() })
	n, err := b.ReadCloser.Read(p)
	t.Stop()
	if err != nil && err != io.EOF && b.timed.Load() {
		err = timeoutError{}
	}
	return n, err
}

func (b *idleBody) Close() error { b.cancel(); return b.ReadCloser.Close() }

func setWait(r *http.Response, s float64) { r.Body.(*idleBody).wait.Store(int64(secs(s))) }

func isRedirect(s int) bool { return s == 301 || s == 302 || s == 303 || s == 307 || s == 308 }

// fetch opens url with the given headers (the player's own, or just a name). The caller closes the body.
func fetch(ctx context.Context, url string, h http.Header, o opts) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	if o.guardAll {
		ctx = context.WithValue(ctx, guardKey{}, true)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Hostname() == "" || req.URL.User != nil {
		cancel()
		return nil, &refused{status: 502}
	}
	req.Header = h.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header["User-Agent"] = []string{""} // no Go-http-client name
	}
	var tr http.RoundTripper = plainTransport
	switch {
	case o.via != nil:
		tr = o.via
	case o.verify:
		tr = checkedTransport
	case o.shared:
		tr = sharedTransport
	}
	client := &http.Client{Transport: tr, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if o.noFollow || len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		if o.stay && r.URL.Host != via[0].URL.Host {
			return &refused{status: 502}
		}
		*r = *r.WithContext(context.WithValue(r.Context(), guardKey{}, true)) // a redirect may not lead home
		return nil
	}}
	var timed atomic.Bool
	t := time.AfterFunc(connectWait+secs(o.wait), func() { timed.Store(true); cancel() })
	resp, err := client.Do(req)
	t.Stop()
	if err != nil {
		cancel()
		var r *refused
		if errors.As(err, &r) {
			return nil, r
		}
		if timed.Load() {
			return nil, timeoutError{}
		}
		return nil, err
	}
	b := &idleBody{ReadCloser: resp.Body, cancel: cancel}
	b.wait.Store(int64(secs(o.wait)))
	resp.Body = b
	return resp, nil
}

type tooLarge struct{}

func (tooLarge) Error() string { return "too large" }

// readAll reads a whole small answer (a playlist, a piece, a list) and closes it.
func readAll(r *http.Response, limit int64, wait float64) ([]byte, error) {
	defer r.Body.Close()
	setWait(r, wait)
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = tooLarge{}
	}
	return data, err
}

// get fetches a whole small answer from this proxy's own front door (never sent elsewhere).
func get(ctx context.Context, url string, h http.Header, limit int64, via http.RoundTripper) (int, []byte, error) {
	r, err := fetch(ctx, url, h, opts{stay: true, via: via, wait: headerWait})
	if err != nil {
		return 0, nil, err
	}
	data, err := readAll(r, limit, headerWait)
	return r.StatusCode, data, err
}

func agentOf(name string) http.Header { // the relay's own fetches: just a name
	h := http.Header{"Accept": {"*/*"}}
	if name != "" {
		h.Set("User-Agent", name)
	}
	return h
}

// ---------------------------------------------------------------------------
// A channel: one source of video, any number of readers
// ---------------------------------------------------------------------------

// cleanStart: where a reader can start in MPEG-TS: a program table nearby, else the first whole packet.
func cleanStart(buf []byte) int {
	end := len(buf) - 376
	for i, tries := bytes.IndexByte(buf, 0x47), 0; 0 <= i && i < end && tries < 2000; tries++ {
		if buf[i+188] == 0x47 && buf[i+376] == 0x47 {
			for k := i; k < min(end, i+188*3000); k += 188 {
				if buf[k] != 0x47 {
					break
				}
				if buf[k+1]&0x1f == 0 && buf[k+2] == 0 {
					return k
				}
			}
			return i
		}
		j := bytes.IndexByte(buf[i+1:], 0x47)
		if j < 0 {
			break
		}
		i += 1 + j
	}
	return 0
}

// keyStart: where a joiner's decoder can start at once: the program table (PAT) just before the first video
// keyframe. Started at any PAT, the player was handed up to a whole keyframe interval of video it could not
// decode, out of the few seconds it gets at once, and stalled. Streams that mark no keyframes: cleanStart.
func keyStart(buf []byte) int {
	at, _ := keyFound(buf)
	return at
}

// keyFound: keyStart, and whether a keyframe was found.
func keyFound(buf []byte) (int, bool) {
	start := cleanStart(buf)
	pmtPID, vid, lastPAT := -1, -1, -1
	for i := start; i+188 <= len(buf) && buf[i] == 0x47; i += 188 {
		pkt := buf[i : i+188]
		pid, afc, off, rai := int(pkt[1]&0x1f)<<8|int(pkt[2]), pkt[3]>>4&3, 4, false
		if afc&2 != 0 {
			rai, off = pkt[4] > 0 && pkt[5]&0x40 != 0, 5+int(pkt[4])
		}
		if afc&1 == 0 || off >= 188 || pkt[1]&0x40 == 0 {
			continue
		}
		switch body := pkt[off:]; {
		case pid == 0:
			lastPAT = i
			if pmt := patPMT(tableStart(body)); pmt >= 0 {
				pmtPID = pmt
			}
		case pid == pmtPID && vid < 0:
			vid = pickStream(tableStart(body))
		case pid == vid && rai && lastPAT >= 0:
			return lastPAT, true
		}
	}
	return start, false
}

// lastPCR: the last program clock reference in b on the given stream (-1: the first one that carries one).
func lastPCR(b []byte, want int) (int, float64, int, bool) {
	start := -1
	for i := 0; i+376 < len(b); i++ {
		if b[i] == 0x47 && b[i+188] == 0x47 && b[i+376] == 0x47 {
			start = i
			break
		}
	}
	pid, v, at, ok := want, 0.0, 0, false
	for i := start; start >= 0 && i+188 <= len(b) && b[i] == 0x47; i += 188 {
		p := b[i : i+188]
		id := int(p[1]&0x1f)<<8 | int(p[2])
		if p[3]&0x20 == 0 || p[4] < 7 || p[5]&0x10 == 0 || (pid >= 0 && id != pid) {
			continue
		}
		pid = id
		v = float64(int64(p[6])<<25|int64(p[7])<<17|int64(p[8])<<9|int64(p[9])<<1|int64(p[10])>>7) / 90000
		at, ok = i, true
	}
	return pid, v, at, ok
}

func resumeAt(buf []byte, clock float64, past int, last, video []byte) (int, bool) {
	if clock < 0 {
		return 0, false
	}
	place := func(at int) bool {
		return (last == nil || samePacket(buf[at-188:at], last)) && (video == nil || lastOf(buf[:at], video))
	}
	for i := cleanStart(buf); i+188 <= len(buf) && buf[i] == 0x47; i += 188 {
		if p := buf[i : i+188]; p[3]&0x20 != 0 && p[4] >= 7 && p[5]&0x10 != 0 &&
			float64(int64(p[6])<<25|int64(p[7])<<17|int64(p[8])<<9|int64(p[9])<<1|int64(p[10])>>7)/90000 == clock {
			at := i + past // (past is whole packets: see whole)
			if at > len(buf) {
				return at, true
			}
			if place(at) {
				return at, true
			}
			end := i + 2*past + 188*64
			for k := i; k+188 <= min(len(buf), end); k += 188 {
				if place(k + 188) {
					return k + 188, true
				}
			}
			if end > len(buf) {
				return len(buf) + 188, true // (not come yet)
			}
		}
	}
	return 0, false
}

func lastOf(b, p []byte) bool {
	after := map[int]bool{}
	for i := len(b) - 188; i >= 0 && b[i] == 0x47; i -= 188 {
		pid := int(b[i+1]&0x1f)<<8 | int(b[i+2])
		if samePacket(b[i:i+188], p) {
			return !after[pid]
		}
		after[pid] = true
	}
	return false
}

func samePacket(a, b []byte) bool {
	return len(a) == 188 && len(b) == 188 && a[0] == b[0] && a[1]&0xe0 == b[1]&0xe0 && a[3]&0xf0 == b[3]&0xf0 && bytes.Equal(a[4:], b[4:])
}

func whole(b []byte) (pkts, rest []byte) {
	out, i := b[:0], 0
	for i+376 <= len(b) {
		if b[i] == 0x47 && b[i+188] == 0x47 {
			out, i = append(out, b[i:i+188]...), i+188
			continue
		}
		j := bytes.IndexByte(b[i+1:], 0x47) // (out of step: the next place a packet can begin)
		if j < 0 {
			return out, nil
		}
		i += 1 + j
	}
	if j := bytes.IndexByte(b[i:], 0x47); j >= 0 {
		return out, append([]byte(nil), b[i+j:]...)
	}
	return out, nil
}

// spliceAt: where a stream the provider sent again joins on to the old one. A provider asked again starts
// with its opening burst, seconds of video from before where the old stream stopped; passed on, the
// stream's clock ran backwards and players froze on it. So the new stream joins at its first keyframe
// whose clock is past the last clock passed on (pid, clock). ok=false: not there yet (wait for more).
// A clock far from the old one (another clock entirely) is not compared: it joins at once.
func spliceAt(buf []byte, pid int, clock float64) (int, bool) {
	start := cleanStart(buf)
	if clock < 0 {
		return start, true
	}
	seen := false
	for i := start; i+188 <= len(buf) && buf[i] == 0x47; i += 188 {
		_, v, _, ok := lastPCR(buf[i:min(len(buf), i+188*3)], pid)
		if !ok || (i+188*3 > len(buf)) {
			continue
		}
		if !seen && math.Abs(v-clock) > 60 {
			return start, true
		}
		seen = true
		if v > clock {
			at, found := keyFound(buf[i:])
			return i + at, found
		}
	}
	return start, false
}

type pace struct{ have, credit, at, guess, since float64 } // a joiner's cushion, allowance, when counted, speed guessed, when begun

type reader struct {
	q                   [][]byte
	n, given            int
	wake                chan struct{}
	over, dropped       bool
	behindAt            float64
	who, id, port, kind string // for the status page
	came                float64
	written             atomic.Int64
	pace                *pace
	cut                 func() // cuts its connection
}

func (rd *reader) signal() {
	select {
	case rd.wake <- struct{}{}:
	default:
	}
}

func (rd *reader) push(d []byte) { rd.q = append(rd.q, d); rd.n += len(d); rd.signal() }

func (rd *reader) end(drop bool) {
	if drop { // nothing more is sent, not even what was waiting, and the connection is cut
		rd.q, rd.n, rd.dropped = nil, 0, true
		if rd.cut != nil {
			rd.cut()
		}
	}
	rd.over = true
	rd.signal()
}

type item struct {
	t    float64
	data []byte
}

type source func(ctx context.Context, ch *channel, yield func([]byte) bool) error

type channel struct {
	key, provider      string
	owner              *front // the provider account it is opened for (see conns in advanced.go)
	readers            map[*reader]bool
	timer              *time.Timer
	ring               []item
	kept               int
	status             int // 200 once video flows, or the refusal; 0 not yet
	refusal            *refused
	ready              chan struct{}
	done, paced        bool
	total, burst       int64
	marks              []mark // (time, total so far) once a second for a minute
	born, asked        float64
	hasBorn, readySent bool
	ctx                context.Context
	cancel             context.CancelFunc
}

type mark struct {
	t     float64
	total int64
}

var (
	channels = map[string]*channel{}
	lastDrop = map[string]float64{} // per provider: when one of its streams was last let go
	lastOpen = map[string]opened{}  // per provider: when a channel was last opened, and which
)

type opened struct {
	at  float64
	key string
}

func allReaders() (n int) {
	for _, c := range channels {
		n += len(c.readers)
	}
	return
}

func newChannel(key string, src source, provider string) *channel { // (mu held)
	ch := &channel{key: key, provider: provider, readers: map[*reader]bool{}, ready: make(chan struct{}), asked: mono()}
	ch.ctx, ch.cancel = context.WithCancel(context.Background())
	go ch.run(src)
	return ch
}

func (ch *channel) setReady() {
	if !ch.readySent {
		ch.readySent = true
		close(ch.ready)
	}
}

func (ch *channel) run(src source) {
	err := src(ch.ctx, ch, ch.yield)
	mu.Lock()
	defer mu.Unlock()
	var r *refused
	switch {
	case err == nil || errors.Is(err, errStop):
		if ch.status == 0 {
			ch.status = 502
		}
	case ch.ctx.Err() != nil: // let go of on purpose
	case errors.As(err, &r):
		if ch.status == 0 {
			ch.status, ch.refusal = r.status, r
		}
	case isTimeout(err):
		if ch.status == 0 {
			ch.status = 504
		}
		say("a channel sent nothing for too long and was closed")
	default:
		if ch.status == 0 {
			ch.status = 502
		}
		say("a channel ended: %s", pyName(err))
	}
	ch.done = true
	lastDrop[ch.provider] = mono()
	if channels[ch.key] == ch {
		delete(channels, ch.key)
	}
	say("a channel was closed (%d open now)", len(channels))
	ch.setReady()
	for rd := range ch.readers {
		rd.end(false)
	}
}

func (ch *channel) yield(data []byte) bool {
	mu.Lock()
	defer mu.Unlock()
	if !ch.done {
		ch.feed(data)
	}
	return !ch.done
}

func (ch *channel) feed(data []byte) {
	if ch.status == 0 {
		ch.status = 200
		ch.setReady()
	}
	now := mono()
	if !ch.hasBorn {
		ch.born, ch.hasBorn = now, true
	}
	ch.total += int64(len(data))
	if len(ch.marks) == 0 || now-ch.marks[len(ch.marks)-1].t >= 1 {
		ch.marks = append(ch.marks, mark{now, ch.total})
		for now-ch.marks[0].t > 60 {
			ch.marks = ch.marks[1:]
		}
	}
	// Providers send live video in lumps, with pauses of several seconds between them all day long: that is
	// normal and is not logged. Only a silence long enough to be seen on a screen is.
	if len(ch.ring) > 0 && now-ch.ring[len(ch.ring)-1].t >= quietNote && !ch.paced { // (a bridged channel notes this itself)
		say("a provider sent nothing for %.0f s, then carried on", now-ch.ring[len(ch.ring)-1].t)
	}
	if now-ch.born < burstSeconds {
		ch.burst += int64(len(data))
	} else if ch.burst >= 0 {
		ch.burst = -ch.burst // (measured once)
	}
	ch.ring = append(ch.ring, item{now, data})
	ch.kept += len(data)
	keep := ringMin
	if speed, ok := ch.speed(now); ok {
		keep = min(max(ringMin, int(speed*keepSeconds)), ringMax)
	}
	for ch.kept-len(ch.ring[0].data) >= keep {
		ch.kept -= len(ch.ring[0].data)
		ch.ring[0] = item{}
		ch.ring = ch.ring[1:]
	}
	for rd := range ch.readers {
		if rd.n <= behind+rd.given { // (what a joiner was handed at the start is not "falling behind")
			rd.behindAt = 0
		} else if rd.behindAt == 0 {
			rd.behindAt = now
		}
		if rd.n > farBehind+rd.given || (rd.behindAt != 0 && now-rd.behindAt > behindSeconds) {
			say("a reader was too slow to keep up with its channel and was disconnected")
			rd.end(true)
			delete(ch.readers, rd)
			ch.dropIfEmpty()
		} else {
			rd.push(data)
		}
	}
}

// speed: bytes a second over the last minute or last 15 s, whichever is larger; false in the first seconds.
func (ch *channel) speed(now float64) (float64, bool) {
	if len(ch.marks) < 2 || now-ch.marks[0].t < 8 {
		return 0, false
	}
	end := now
	if last := ch.ring[len(ch.ring)-1].t; now-last >= 15 { // a stopped feed is measured up to its last video
		end = last
	}
	older := ch.marks[0]
	for i := len(ch.marks) - 1; i >= 0; i-- {
		if end-ch.marks[i].t >= 15 {
			older = ch.marks[i]
			break
		}
	}
	best := 0.0
	for _, m := range []mark{ch.marks[0], older} {
		if end-m.t >= 8 {
			best = math.Max(best, float64(ch.total-m.total)/(end-m.t))
		}
	}
	return best, best != 0
}

// join: the opener gets everything from the first byte; a joiner the newest joinSeconds, on a whole packet.
func (ch *channel) join(first bool) *reader {
	if ch.timer != nil { // somebody is here after all
		ch.timer.Stop()
		ch.timer = nil
	}
	rd := &reader{wake: make(chan struct{}, 1)}
	// Nobody is on the channel any more (it is kept a moment after its last reader left, in its first
	// seconds): this is the same tune carrying on, most often a player that hung up and asked again. It is
	// given everything kept so far at once, as the reader who opened it was, not a joiner's paced cushion:
	// that cushion is there for a second device on a channel someone is watching, and here nobody is.
	if !first && len(ch.readers) == 0 && len(ch.ring) > 0 {
		var head []byte // (the ring may have been trimmed: start where a decoder can)
		n := 0
		for ; n < len(ch.ring) && len(head) < 2<<20; n++ {
			head = append(head, ch.ring[n].data...)
		}
		rd.push(head[keyStart(head):])
		for _, it := range ch.ring[n:] {
			rd.push(it.data)
		}
		rd.given = rd.n
		say("a reader came back to a channel nobody else was on and was given %d KB at once", rd.n/1024)
	} else if first {
		for _, it := range ch.ring {
			rd.push(it.data)
		}
	} else if len(ch.ring) > 0 {
		speed, measured := ch.speed(mono())
		want := ch.kept // (no speed yet: all of it)
		if measured {
			want = int(speed * joinSeconds)
		}
		var take [][]byte
		size, oldest := 0, ch.ring[len(ch.ring)-1].t-2*joinSeconds
		for i := len(ch.ring) - 1; i >= 0; i-- {
			if d := ch.ring[i].data; size == 0 || (size+len(d) <= want && ch.ring[i].t >= oldest) {
				take, size = append([][]byte{d}, take...), size+len(d)
			} else {
				break
			}
		}
		var head []byte // only the beginning is copied, to find where a decoder can start
		n := 0
		for ; n < len(take) && len(head) < 2<<20; n++ {
			head = append(head, take[n]...)
		}
		rd.push(head[keyStart(head):])
		for _, d := range take[n:] {
			rd.push(d)
		}
		rd.given = rd.n
		// The cushion is handed on at playing speed (a player given seconds at once jumps through it and then
		// stalls at the provider's next pause); joinStart seconds go at once so it can begin.
		// A channel in its first seconds has no measured speed yet, and what it has kept is less than
		// joinSeconds of video: paced by a guess (what is kept, taken for joinSeconds of video) the joiner
		// was sent a first lump and then nothing for seconds. It is given what is kept at once, as the
		// reader who opened the channel was a moment before.
		if measured {
			rd.pace = &pace{float64(rd.n), speed * joinStart, mono(), speed, mono()}
		}
		extra := ""
		if measured {
			extra = fmt.Sprintf(" (%d s at %d KB a second)", joinSeconds, int64(speed/1024))
		}
		say("a reader joined a channel and was given %d KB to start%s", rd.n/1024, extra)
	}
	ch.readers[rd] = true
	return rd
}

func (ch *channel) leave(rd *reader) { // (mu held)
	delete(ch.readers, rd)
	if len(ch.readers) > 0 || ch.done {
		return
	}
	now, op := mono(), lastOpen[ch.provider]
	tuning := !ch.hasBorn || now-ch.born < 5     // a player tunes in a channel's first seconds
	moved := now-op.at < 1.0 && op.key != ch.key // it has just gone to another channel
	if !tuning || moved {
		ch.drop()
		return
	}
	if ch.timer != nil {
		ch.timer.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(secs(linger), func() {
		mu.Lock()
		defer mu.Unlock()
		if ch.timer == t {
			ch.dropIfEmpty()
		}
	})
	ch.timer = t
}

func (ch *channel) dropIfEmpty() {
	if len(ch.readers) == 0 {
		ch.drop()
	}
}

func (ch *channel) drop() { // let go of the provider's stream now (mu held)
	if ch.timer != nil {
		ch.timer.Stop()
		ch.timer = nil
	}
	if !ch.done {
		ch.cancel()
		ch.done = true
		lastDrop[ch.provider] = mono()
		if channels[ch.key] == ch {
			delete(channels, ch.key)
		}
	}
}

// tsSource: the provider's own MPEG-TS stream, asked for again if it ends while people watch.
func tsSource(target string, h http.Header) source {
	return func(ctx context.Context, ch *channel, yield func([]byte) bool) error {
		again, sent, quiet := false, int64(0), 0
		clockPID, clock := -1, -1.0 // the newest stream clock passed on (see spliceAt)
		for {
			began, got := mono(), false
			wait := float64(headerWait)
			if again {
				wait = reopenWait // readers are waiting
			}
			r, err := fetch(ctx, target, h, opts{wait: wait})
			if err != nil {
				return err
			}
			if r.StatusCode != 200 && r.StatusCode != 206 {
				say("a provider answered %d when asked for a channel", r.StatusCode)
				text, _ := readAll(r, 65536, 5) // what the provider said, passed on to the player
				return &refused{status: r.StatusCode, kind: r.Header.Get("Content-Type"), text: text[:min(len(text), 65536)]}
			}
			err = func() error {
				defer r.Body.Close()
				setWait(r, quietWait)
				if again {
					setWait(r, 2*reopenWait)
				}
				var head []byte // a stream opened again is joined on at a whole packet
				joining := again
				buf := make([]byte, 65536)
				for {
					n, err := r.Body.Read(buf)
					setWait(r, quietWait)
					data := append([]byte(nil), buf[:n]...)
					if joining {
						head = append(head, data...)
						if err == nil && len(head) < 16384 {
							continue
						}
						at, ok := spliceAt(head, clockPID, clock)
						if !ok && err == nil && len(head) < 48<<20 {
							continue
						}
						if !ok {
							at = cleanStart(head)
						} else if clock >= 0 && at > 0 {
							say("a channel asked for again repeated %d KB of video; it was left out", at>>10)
						}
						data, head, joining = head[at:], nil, false
						if len(data) > 0 && sent%188 != 0 { // the old stream stopped mid-packet: fill it out
							fill := make([]byte, 188-sent%188)
							for i := range fill {
								fill[i] = 0xff
							}
							sent += int64(len(fill))
							if !yield(fill) {
								return errStop
							}
						}
					}
					if len(data) > 0 {
						if p, v, _, ok := lastPCR(data, clockPID); ok {
							clockPID, clock = p, v
						}
						got = true
						sent += int64(len(data))
						if !yield(data) {
							return errStop
						}
					}
					if err == io.EOF {
						return nil
					} else if err != nil {
						return err
					}
				}
			}()
			if errors.Is(err, errStop) || ctx.Err() != nil || (err != nil && !got) {
				return err
			}
			// The provider ended it. Ask again if anyone still watches, unless it ran under reopenAfter, or (on an
			// account that allows one connection, or whose limit is not known) another channel of this provider opened
			// in the last 15 s: asking would end that one.
			mu.Lock()
			ran, op := mono()-began, lastOpen[ch.provider]
			if isTimeout(err) && ran < quietWait+reopenAfter { // silent twice in a row: the channel is dead
				quiet++
			} else {
				quiet = 0
			}
			// (an account that allows more than one connection: opening another channel did not end this one)
			single := ch.owner == nil || ch.owner.limit() <= 1
			stop := quiet >= 2 || len(ch.readers) == 0 || ran < reopenAfter || (single && op.at > math.Max(began, mono()-15) && op.key != ch.key)
			mu.Unlock()
			if quiet >= 2 {
				say("a channel went silent twice in a row; letting its readers go")
			}
			if stop {
				return nil
			}
			again = true
			say("a provider ended a channel that was being watched; asking for it again")
			if err := sleep(ctx, dropGap); err != nil {
				return err
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Requests from the front door
// ---------------------------------------------------------------------------

// What a player sent that is not passed on: what belongs to one connection, and this proxy's notes.
var skipRe = regexp.MustCompile(`(?i)^(host|connection|keep-alive|proxy-.*|te|trailer|transfer-encoding|upgrade|content-length` +
	`|range|if-range|accept-encoding|via|x-relay-.*|x-hls-.*|x-real-ip|x-forwarded-.*)$`)
var notPrintable = regexp.MustCompile(`[^\x20-\x7e]`)

func answer(w http.ResponseWriter, status int, kind, text string) {
	if kind == "" {
		kind = "text/plain"
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(status)
	io.WriteString(w, text)
}

// channelKey: one name per channel, whichever address form (/live/u/p/1.ts, /u/p/1.ts, /u/p/1) is used.
func channelKey(path, provider, uri string) string {
	parts := strings.FieldsFunc(strings.SplitN(uri, "?", 2)[0], func(r rune) bool { return r == '/' })
	if len(parts) > 0 && parts[0] == "live" {
		parts = parts[1:]
	}
	if len(parts) > 0 {
		parts[len(parts)-1] = strings.TrimSuffix(parts[len(parts)-1], ".ts")
	}
	return path + " " + provider + " " + strings.Join(parts, "/")
}

// channelID: the channel's number (last address part without extension); never a login.
func channelID(uri string) string {
	parts := strings.Split(strings.SplitN(uri, "?", 2)[0], "/")
	id, _, _ := strings.Cut(parts[len(parts)-1], ".")
	if id == "" || len(id) > 12 || strings.Trim(id, "0123456789") != "" {
		return "?"
	}
	return id
}

func whoIs(r *http.Request) string { // the player's address, from its connection (never from a header)
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().WithZone("").Unmap().String()
	}
	return "a player"
}

func hostOf(provider string) string {
	if i := strings.Index(provider, "://"); i >= 0 {
		h, _, _ := strings.Cut(provider[i+3:], "/")
		if host, _, err := net.SplitHostPort(h); err == nil {
			return host
		}
		return h
	}
	return "?"
}

func hms(s float64) string {
	n := int64(s)
	switch {
	case n >= 3600:
		return fmt.Sprintf("%dh %02dm %02ds", n/3600, n/60%60, n%60)
	case n >= 60:
		return fmt.Sprintf("%dm %02ds", n/60, n%60)
	}
	return fmt.Sprintf("%ds", n)
}

// failed answers a refusal with its status; anything else that broke before an answer, 502.
func failed(w http.ResponseWriter, err error) {
	var r *refused
	if errors.As(err, &r) {
		if r.after > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(r.after))
		}
		answer(w, r.status, r.kind, string(r.text))
	} else if err != nil {
		answer(w, 502, "", "")
	}
}

// live: path is "/ts" (the provider's own MPEG-TS) or "/hls" (MPEG-TS made from the channel's HLS).
func live(w http.ResponseWriter, r *http.Request, rt *route, path string) {
	provider, uri := rt.f.base, rt.uri
	if r.Method == "HEAD" { // always 200 here: asking the provider would open a connection
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(200)
		return
	}
	if r.Method != "GET" {
		answer(w, 400, "", "")
		return
	}
	sent := http.Header{} // the player's own headers, for the provider
	for k, vs := range r.Header {
		if !skipRe.MatchString(k) {
			for _, v := range vs {
				sent.Add(k, notPrintable.ReplaceAllString(v, ""))
			}
		}
	}
	rt.f.agent(sent)
	key, gone := channelKey(path, provider, uri), r.Context().Done()
	mu.Lock()
	ch := channels[key]
	first := ch == nil
	if !first { // moving onto a channel already open: an HLS builder this device left goes too
		releaseFor(provider, rt.client, key)
	}
	if first {
		// Changing channel: the hang-up on the old one and this request arrive in either order. Channels of
		// this provider nobody reads go now; a hang-up just behind gets 30 ms; the provider gets dropGap to see it.
		was := lastOpen[provider]
		lastOpen[provider] = opened{mono(), key}
		releaseFor(provider, rt.client, key)
		var mine []*channel
		live := false
		for _, c := range channels {
			if c.provider == provider {
				mine = append(mine, c)
				c.dropIfEmpty()
				live = live || !c.done
			}
		}
		mu.Unlock()
		if live {
			time.Sleep(30 * time.Millisecond)
			mu.Lock()
			for _, c := range mine {
				c.dropIfEmpty()
			}
			mu.Unlock()
		}
		// A new provider connection: by the rules this provider has on (advanced.go). Another request for the
		// same channel in the meantime joins it. MPEG-TS made from HLS reads the channel's HLS session: one
		// that is open already (a device watching the channel as HLS) costs nothing more.
		want, opens := what{"ch", channelID(uri)}, key
		if path == "/hls" {
			opens = provider + " /live/" + rt.f.encU + "/" + rt.f.encP + "/" + want.item + ".m3u8"
		}
		h, why := rt.f.admit(r.Context(), rt.client, want, false, opens, nil)
		if h == nil {
			unOpen(provider, key, was)
			if why != "" {
				failed(w, rt.f.refusal(rt.client, want, why))
			}
			return
		}
		defer h.release()
		mu.Lock()
		wait := lastDrop[provider] + dropGap - mono()
		mu.Unlock()
		time.Sleep(secs(math.Max(0, wait)))
		if r.Context().Err() != nil { // the player has already moved on
			return
		}
		mu.Lock()
		for _, c := range channels { // once more, for hang-ups during the waits
			if c.provider == provider && c.key != key {
				c.dropIfEmpty()
			}
		}
		if ch = channels[key]; ch == nil {
			src := tsSource(provider+uri, sent)
			if path == "/hls" {
				port := rt.f.port
				plain, query, _ := strings.Cut(rt.orig, "?")
				if query != "" {
					query = "?" + query
				}
				// One HLS address per channel whatever form the reader used: a second form would be a
				// second provider session, which ends the players' one.
				parts := strings.FieldsFunc(strings.TrimSuffix(plain, ".ts"), func(r rune) bool { return r == '/' })
				if len(parts) > 0 && parts[0] == "live" {
					parts = parts[1:]
				}
				plain = strings.TrimSuffix(plain, ".ts")
				if len(parts) == 3 {
					plain = "/live/" + strings.Join(parts, "/")
				}
				src = hlsSource(fmt.Sprintf("http://127.0.0.1:%s/__ch%s.m3u8/__index.m3u8%s", port, plain, query),
					notPrintable.ReplaceAllString(r.Header.Get("User-Agent"), ""), inside{rt.f})
			}
			ch = newChannel(key, src, provider)
			ch.owner = rt.f
			channels[key] = ch
		} else {
			first = false
		}
		delete(holds, h) // (counted as the channel from here on)
	}
	if allReaders() >= readersMax {
		if first {
			ch.drop()
		}
		mu.Unlock()
		answer(w, 503, "", "Too many readers at once.\n")
		return
	}
	rd := ch.join(first)
	rd.who, rd.id, rd.port, rd.came = rt.client, channelID(uri), rt.f.port, mono()
	rd.kind = map[string]string{"/ts": "MPEG-TS", "/hls": "MPEG-TS fed from HLS"}[path]
	mu.Unlock()
	why, started := "the player hung up", false
	defer func() {
		mu.Lock()
		still := max(0, len(ch.readers)-1)
		ch.leave(rd)
		mu.Unlock()
		if started {
			say("%s left channel %s after %s, %.1f MB sent: %s (%d still on it)", rd.who, rd.id, hms(mono()-rd.came), float64(rd.written.Load())/1048576, why, still)
		} else {
			say("a reader left after %d s, before any video: %s (%d still on its channel)", int64(mono()-rd.came), why, still)
		}
	}()
	t := time.NewTimer(startWait * time.Second)
	select {
	case <-ch.ready:
	case <-gone:
	case <-t.C:
	}
	t.Stop()
	if r.Context().Err() != nil {
		return
	}
	mu.Lock()
	status, e, born, asked := ch.status, ch.refusal, ch.born, ch.asked
	mu.Unlock()
	if status != 200 {
		switch {
		case e != nil && e.status >= 400 && e.status < 600: // the provider's own answer, passed on
			answer(w, e.status, e.kind, string(rt.f.hideLogin(e.text)))
		case status == 0:
			answer(w, 504, "", "")
		case status >= 400 && status < 600:
			answer(w, status, "", "")
		default:
			answer(w, 502, "", "")
		}
		return
	}
	rc := http.NewResponseController(w)
	defer context.AfterFunc(r.Context(), func() { rc.SetWriteDeadline(time.Now()) })() // a blocked write gives up on hang-up
	// Sent as the provider sends it: not chunked, ended by closing the connection. (Go's server reads
	// "Transfer-Encoding: identity" as exactly that, and does not send the header itself.)
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Transfer-Encoding", "identity")
	w.Header().Set("Connection", "close")
	w.WriteHeader(200)
	rc.Flush()
	mu.Lock()
	rd.cut = func() { rc.SetWriteDeadline(time.Now()) }
	mu.Unlock()
	started = true
	if first {
		say("%s started channel %s (%s), first video in %.1f s", rd.who, rd.id, hostOf(provider), born-asked)
	} else {
		say("%s joined channel %s (%s)", rd.who, rd.id, hostOf(provider))
	}
	for {
		mu.Lock()
		if rd.dropped { // too slow: cut the connection, as the Python did
			mu.Unlock()
			panic(http.ErrAbortHandler)
		}
		if p := rd.pace; len(rd.q) > 0 && p != nil { // a joiner in its first seconds: fed at a pace, until its cushion is handed over
			now := mono()
			speed, ok := ch.speed(now)
			if !ok {
				speed = p.guess
			}
			// a little faster when more than the cushion waits, a little slower when less
			rate := speed * math.Min(1.5, math.Max(0.8, 1+0.5*(float64(rd.n)-p.have)/math.Max(p.have, 1)))
			// Nothing is kept back from a reader for good. Once the joiner's start is under way (its first
			// joinStart seconds, as above), what waits here for it is handed over at one and a half times
			// playing speed until none is left; from then on it is sent everything as it arrives, like the
			// reader who opened the channel (see below, where the pace is dropped).
			if now-p.since >= joinStart {
				rate = speed * 1.5
			}
			most := speed * joinStart
			p.credit, p.at = math.Min(p.credit+(now-p.at)*rate, most), now
			// an item longer than joinStart seconds goes once the allowance is full; its play time is waited after
			if need := math.Min(float64(len(rd.q[0])), most); p.credit < need {
				mu.Unlock()
				t := time.NewTimer(secs((need - p.credit) / rate))
				select {
				case <-gone:
					t.Stop()
					return
				case <-t.C:
				}
				continue
			}
			p.credit -= float64(len(rd.q[0]))
		}
		if len(rd.q) > 0 {
			data := rd.q[0]
			rd.q[0], rd.q = nil, rd.q[1:]
			rd.n -= len(data)
			if rd.pace == nil {
				rd.given = max(0, rd.given-len(data))
			} else if len(rd.q) == 0 && mono()-rd.pace.since >= joinStart { // the cushion is handed over: no pace from here on
				rd.pace, rd.given = nil, 0
				say("a reader that joined its channel has been handed everything kept for it; it is now sent video as it arrives")
			}
			mu.Unlock()
			n, err := w.Write(data)
			if err == nil {
				err = rc.Flush()
			}
			rd.written.Add(int64(n))
			if err != nil {
				mu.Lock()
				dropped := rd.dropped
				mu.Unlock()
				if dropped {
					panic(http.ErrAbortHandler)
				}
				select {
				case <-gone:
				case <-time.After(50 * time.Millisecond): // (the hang-up is usually seen a moment later)
					why = "the line to the player broke"
				}
				return
			}
			continue
		}
		if rd.over {
			why = "its channel ended"
			mu.Unlock()
			return
		}
		mu.Unlock()
		select {
		case <-rd.wake:
		case <-gone:
			return
		}
	}
}
