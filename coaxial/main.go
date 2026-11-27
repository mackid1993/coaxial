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
// Same status codes and timing rules as the relay it grew from. Standard library only, apart from the log (zerolog, see logging.go).
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
	"math"
	"net"
	"net/http"
	"net/netip"
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
	reopenWait    = 5                // seconds to answer when asked again (twice that to send video)
	keyWait       = 12               // seconds of a stream asked for again searched for a keyframe (see spliceAt)
	dropGap       = 0.1              // time a provider gets to see a hang-up before another channel is asked for
	linger        = 0.4              // a channel in its first seconds is kept this long after its last reader (tuning)
	readersMax    = 100              // readers served at once, all channels together
	behind        = 16 << 20         // a reader this far behind for behindSeconds is disconnected...
	behindSeconds = 15               //
	behindVideo   = 15               // ...(on a channel of a high rate: this many seconds of its video behind, where that is more)...
	farBehind     = 64 << 20         // ...or this far behind at any moment
	connectWait   = 10 * time.Second // to accept a connection
	keepIdle      = 20 * time.Second // an idle connection to a server is kept this long for the next request
	headerWait    = 20               // seconds for an answer's heading
	playlistWait  = 30               // the same for a playlist
	startWait     = 60               // seconds a reader waits for a channel's first video
	// liveQuiet: seconds a live channel's provider connection may send nothing before the channel is asked for
	// again (on a new connection; readers stay on). Above the longest pause providers make on a healthy live
	// stream (6 to 12 s lumps, 15 to 17 s at peak load, see gapWarn), and no longer than a player's own wait
	// for bytes (AetherEngine gives up after 20 s): a connection that went quiet without closing (through a VPN
	// tunnel that can happen) otherwise held every reader 60 s while a player asking direct reconnects itself.
	liveQuiet = 20
)

var mu sync.Mutex // all shared state; held only between waits (the Python ran on one thread)

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

// wentQuiet: the error is a server sending nothing for too long (idleBody's or a deadline's).
// firstQuiet: a channel's first provider connection gave no answer, or answered and sent nothing, for firstWait
// (a connection can go quiet without closing; through a VPN tunnel that happens). A player straight at the
// provider asks again on a new connection after its own wait; the readers waiting here are asked for again the
// same way, twice at most, while anyone waits. It was answered 504 instead, with no new connection made.
// firstWait: seconds a channel's first provider connection may take to answer and to send its first video before
// it is asked again (firstQuiet). A provider's opening burst comes within a second; a player straight at the
// provider waits 8 s (Media3) to 20 s (AetherEngine) before it reconnects, and a playlist of HLS built from the
// MPEG-TS waits segWait (20 s) for its first pieces, so the new connection must come well before that.
const firstWait = 12

func firstQuiet(ctx context.Context, ch *channel, again bool, err error, tries *int, st *streamStats) bool {
	if again || *tries >= 2 || ctx.Err() != nil || !wentQuiet(err) || !watched(ch) {
		return false
	}
	*tries++
	st.fields(lg.Warn()).Int("try", *tries).Msgf("a channel's first connection sent nothing for %d s; asked again on a new connection", firstWait)
	return true
}

func wentQuiet(err error) bool { return errors.As(err, new(timeoutError)) || isTimeout(err) }

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
	fresh    bool              // on a new connection of its own (a kept one went quiet)
	via      http.RoundTripper // answered inside this program (see inside in front.go)
	wait     float64
	ask      *askLog // what it is, for the log (nil: not logged)
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
	asked, began := ctx, time.Now()
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
	case o.fresh:
		tr = plainTransport
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
			o.ask.failed(r, false, time.Since(began))
			return nil, r
		}
		if timed.Load() {
			o.ask.failed(timeoutError{}, false, time.Since(began))
			return nil, timeoutError{}
		}
		o.ask.failed(err, asked.Err() != nil, time.Since(began))
		return nil, err
	}
	o.ask.answered(resp.StatusCode, time.Since(began))
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

// pieceWait: seconds a playlist or a piece of HLS video fetched for an MPEG-TS reader may send nothing before
// it is given up and asked again.
// ExoPlayer's default read timeout (DefaultHttpDataSource, 8 s); a stuck download otherwise held the stream
// for the full headerWait while the playlist already listed newer pieces.
const pieceWait = 8

// getWithin: get, giving up when the answer sends nothing for wait seconds.
func getWithin(ctx context.Context, url string, h http.Header, limit int64, via http.RoundTripper, wait float64) (int, []byte, error) {
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := time.AfterFunc(secs(wait), cancel) // (the answer's heading within wait seconds, connecting included)
	r, err := fetch(hctx, url, h, opts{stay: true, via: via, wait: wait})
	if !t.Stop() {
		if err == nil {
			r.Body.Close()
		}
		return 0, nil, timeoutError{}
	}
	if err != nil {
		return 0, nil, err
	}
	data, err := readAll(r, limit, wait)
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

// keyFound: keyStart, and whether a keyframe was found. A keyframe is a video frame marked as one, or, in
// H.264 or HEVC, a frame not marked whose first picture is one a decoder can start at (see keyNAL), the way
// players find them in streams that mark none.
func keyFound(buf []byte) (int, bool) {
	start := cleanStart(buf)
	pmtPID, vid, typ, lastPAT := -1, -1, byte(0), -1
	var es []byte // the start of an unmarked frame, read for its first picture
	esPAT := -1
	for i := start; i+188 <= len(buf) && buf[i] == 0x47; i += 188 {
		pkt := buf[i : i+188]
		pid, afc, off, rai := int(pkt[1]&0x1f)<<8|int(pkt[2]), pkt[3]>>4&3, 4, false
		if afc&2 != 0 {
			rai, off = pkt[4] > 0 && pkt[5]&0x40 != 0, 5+int(pkt[4])
		}
		if afc&1 == 0 || off >= 188 {
			continue
		}
		body := pkt[off:]
		if pid == vid && es != nil && pkt[1]&0x40 == 0 { // the frame goes on: read on, about two packets more
			es = append(es, body...)
			if key, done := keyNAL(es, typ); key {
				return esPAT, true
			} else if done || len(es) > 2*184 {
				es = nil
			}
		}
		if pkt[1]&0x40 == 0 {
			continue
		}
		switch {
		case pid == 0:
			lastPAT = i
			if pmt := patPMT(tableStart(body)); pmt >= 0 {
				pmtPID = pmt
			}
		case pid == pmtPID && vid < 0:
			vid, typ = pickType(tableStart(body))
		case pid == vid && rai && lastPAT >= 0:
			return lastPAT, true
		case pid == vid:
			es = nil
			if lastPAT >= 0 && len(body) >= 9 && body[0] == 0 && body[1] == 0 && body[2] == 1 {
				es, esPAT = append([]byte{}, body[min(len(body), 9+int(body[8])):]...), lastPAT
				if key, done := keyNAL(es, typ); key {
					return esPAT, true
				} else if done {
					es = nil
				}
			}
		}
	}
	return start, false
}

// keyNAL: whether the first picture in the start of a frame (es) is one a decoder can start at: in H.264
// (stream type 0x1b) an IDR picture, in HEVC (0x24) an IRAP picture (BLA, IDR or CRA). done: a picture
// was found (or the stream is neither), so reading on cannot change the answer.
func keyNAL(es []byte, typ byte) (key, done bool) {
	if typ != 0x1b && typ != 0x24 {
		return false, true
	}
	for i := 0; i+3 < len(es); i++ {
		if es[i] != 0 || es[i+1] != 0 || es[i+2] != 1 {
			continue
		}
		if n := es[i+3]; typ == 0x1b && n&0x1f >= 1 && n&0x1f <= 5 { // a picture: IDR is 5
			return n&0x1f == 5, true
		} else if t := n >> 1 & 0x3f; typ == 0x24 && t <= 31 { // a picture: IRAP is 16 to 21
			return t >= 16 && t <= 21, true
		}
	}
	return false, false
}

// lastPCR: the last program clock reference in b on the given stream (-1: the first one that carries one).
func lastPCR(b []byte, want int) (int, float64, bool) {
	start := -1
	for i := 0; i+376 < len(b); i++ {
		if b[i] == 0x47 && b[i+188] == 0x47 && b[i+376] == 0x47 {
			start = i
			break
		}
	}
	pid, v, ok := want, 0.0, false
	for i := start; start >= 0 && i+188 <= len(b) && b[i] == 0x47; i += 188 {
		p := b[i : i+188]
		id := int(p[1]&0x1f)<<8 | int(p[2])
		if p[3]&0x20 == 0 || p[4] < 7 || p[5]&0x10 == 0 || (pid >= 0 && id != pid) {
			continue
		}
		pid = id
		v = float64(int64(p[6])<<25|int64(p[7])<<17|int64(p[8])<<9|int64(p[9])<<1|int64(p[10])>>7) / 90000
		ok = true
	}
	return pid, v, ok
}

// clockFix keeps a live channel's clock running on. When a provider's feed starts a new file (a 24/7
// channel moving on to the next episode), the stream's timestamps start over, at or near zero, in the middle
// of the connection. Players read that as time going backwards and play video they already have again. So
// from such a jump on, every timestamp (PCR, PTS, DTS) is moved by one offset that makes the new file carry
// on from where the old one stopped. A stream whose clock runs on normally is never touched (offset 0);
// nothing is added, held back or taken out, only the timestamp fields of packets already passing are
// rewritten. The clock's own wrap after about 26 hours is not a jump.
type clockFix struct {
	off      int64 // added to every PES timestamp (PTS, DTS), modulo 2^33
	offPCR   int64 // added to every PCR: the same as off unless one of the two clocks jumped alone
	rawPCR   int64 // the newest PCR and video PTS on the provider's own clock; -1: none yet
	rawPTS   int64
	tryOff   int64 // the PES offset before a PCR jump moved it too (see stamp); -1: none to undo
	tryPCR   int64 // the PCR offset before a video PTS jump moved it too; -1: none to undo
	jumps    int   // jumps seen (for tests)
	last     int64 // the newest PCR passed on (after the offset); -1: none yet
	lastPTS  int64 // the newest video PTS passed on (after the offset); -1: none yet
	pcrPID   int
	videoPID int
	started  bool
	carry    []byte // after a jump: the start of a packet whose rest has not come yet
	next     int64  // where in the stream the next packet starts (packets are not always 188 bytes apart: a
	//                provider can cut one short), -1: not known, find it
	whole bool     // the clock has jumped: from then on only whole packets are passed on
	tail  []byte   // before that: the start of a packet the last read ended inside (read for fingerprints only)
	rp    *replays // video frames passed on, and what is held or left out after a jump (see replay.go)
	now   func() float64
	tag   logTag // the channel's fields for the log
}

const tsWrap = int64(1) << 33

// tsDiff: a-b on the 33-bit clock, from -2^32 to 2^32.
func tsDiff(a, b int64) int64 {
	d := (a - b) % tsWrap
	if d < 0 {
		d += tsWrap
	}
	if d >= tsWrap/2 {
		d -= tsWrap
	}
	return d
}

func (c *clockFix) jumped(v, last int64) bool {
	if last < 0 {
		return false
	}
	d := tsDiff(v, last)
	return d < -3*90000 || d > 30*90000
}

func readTS(b []byte) int64 {
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}

func writeTS(b []byte, v int64) {
	v = ((v % tsWrap) + tsWrap) % tsWrap
	b[0] = b[0]&0xf1 | byte(v>>29)&0x0e
	b[1] = byte(v >> 22)
	b[2] = b[2]&0x01 | byte(v>>14)&0xfe
	b[3] = byte(v >> 7)
	b[4] = b[4]&0x01 | byte(v<<1)&0xfe
}

// raw: a clock passed on (seconds, offset added) on the provider's own clock again.
func (c *clockFix) raw(clock float64) float64 {
	if clock < 0 || c.offPCR == 0 {
		return clock
	}
	return float64(((int64(math.Round(clock*90000))-c.offPCR)%tsWrap+tsWrap)%tsWrap) / 90000
}

// fix rewrites the timestamps in data in place and returns what to pass on. off: how many bytes of the stream
// were passed on before data. Once the clock has jumped, the start of a packet whose rest is still on its way
// is kept back until the rest comes, so that its timestamps can be rewritten too; half a packet is of no use to
// a player, so nothing it could play waits. Video the provider sends again after a jump is left out (see
// replay.go). A stream whose clock never jumped is passed on exactly as read.
func (c *clockFix) fix(data []byte, off int64) []byte {
	if !c.started {
		c.started, c.last, c.lastPTS, c.pcrPID, c.videoPID, c.next = true, -1, -1, -1, -1, -1
		c.rawPCR, c.rawPTS, c.tryOff, c.tryPCR = -1, -1, -1, -1
		c.rp = newReplays()
	}
	if len(c.carry) > 0 {
		data = append(c.carry, data...)
		c.carry = nil
	}
	r := c.rp
	r.data, r.res, r.pos, r.copied = data, nil, 0, false
	defer func() { r.data, r.res = nil, nil }()
	i, grid := -1, true // grid: i is a packet start known from the packets before it
	if c.next >= off && c.next-off <= int64(len(data)) {
		i = int(c.next - off)
	} else if g := c.next + 188 - off; c.next >= 0 && g >= 0 && g <= int64(len(data)) &&
		(g == int64(len(data)) || data[g] == 0x47 && (g+188 >= int64(len(data)) || data[g+188] == 0x47)) {
		i = int(g)                                     // the last read ended inside a packet: the next one starts 188 bytes after it (when it does)
		if len(c.tail)+i == 188 && c.tail[0] == 0x47 { // (that packet, read whole for the fingerprints and the clock)
			p := append(c.tail, data[:i]...)
			c.see(p)
			c.track(p)
			r.ccSeen(p)
		}
	}
	c.tail = nil
	if r.ccPend >= 0 { // (the packet that ended the last read was one: the next one starts right after it)
		if i == 0 && len(data) > 0 && data[0] == 0x47 {
			r.cc[r.ccPend] = r.ccVal
		}
		r.ccPend = -1
	}
	for ; ; i += 188 {
		if i < 0 || i < len(data) && (data[i] != 0x47 || i+188 < len(data) && data[i+188] != 0x47) {
			// Not on a packet start, or on the start of a packet the provider cut short (the next one does not follow
			// 188 bytes on), or the first read: find the next place where packet starts follow 188 bytes apart. Bytes
			// before it are passed on untouched, never rewritten.
			from := i + 1
			if i < 0 {
				from = 0
			}
			i, grid = syncAt(data, from), false
			if i < 0 {
				c.junk(len(data))
				c.next = -1
				return r.output(len(data))
			}
		}
		if i+188 > len(data) {
			break
		}
		c.junk(i)
		p := data[i : i+188]
		jumped := c.stamp(p)
		r.sure = i+188 < len(data) && data[i+188] == 0x47
		c.route(p, jumped)
		r.pos, grid = i+188, true
	}
	c.junk(min(i, len(data)))
	if c.whole && i < len(data) && data[i] == 0x47 {
		c.carry = append([]byte(nil), data[i:]...)
		out := r.output(i)
		c.next = off + int64(len(out))
		return out
	}
	if i < len(data) && data[i] == 0x47 {
		// The read ended inside a packet (before any jump: nothing is held). If its timestamps have come, they are
		// read and written now, so that a jump in this very packet does not reach players on the provider's clock.
		if part := data[i:]; grid && len(part) >= stampNeed(part) {
			var p [188]byte
			copy(p[:], part)
			if c.stamp(p[:]) {
				c.logJump("the jump came in a packet cut across two reads: video sent again was not looked for", 0, 0)
			}
			copy(part, p[:])
		}
		c.tail = append([]byte(nil), data[i:]...)
	}
	c.junk(len(data))
	out := r.output(len(data))
	if c.whole && i >= len(data) {
		c.next = off + int64(len(out))
	} else {
		c.next = off + int64(i)
	}
	return out
}

// track: the clocks of a packet already passed on whole, before any jump (its timestamps could not be read in
// time): the newest PCR and video PTS are what a jump is measured from. A jump in it went out as it was; the next
// packet shows it as before.
func (c *clockFix) track(p []byte) {
	if c.whole {
		return
	}
	var q [188]byte
	copy(q[:], p)
	if c.last < 0 || c.lastPTS < 0 {
		return
	}
	last, lastPTS, rawPCR, rawPTS := c.last, c.lastPTS, c.rawPCR, c.rawPTS
	if c.stamp(q[:]) || c.whole { // (a jump: leave it for the next packet, as it went out)
		c.whole, c.last, c.lastPTS, c.rawPCR, c.rawPTS = false, last, lastPTS, rawPCR, rawPTS
		c.off, c.offPCR, c.tryOff, c.tryPCR = 0, 0, -1, -1
		c.rp.jl.set = false
		c.rp.anchorPCR, c.rp.anchorPTS = anchor{}, anchor{}
	}
}

// stampNeed: how much of a packet stamp reads or writes (189: not known yet).
func stampNeed(p []byte) int {
	if len(p) < 6 {
		return 189
	}
	pos, need := 4, 4
	if p[3]&0x20 != 0 {
		pos, need = 5+int(p[4]), 6
		if p[4] >= 7 {
			need = 11
		}
	}
	if p[1]&0x40 == 0 || p[3]&0x10 == 0 || pos+14 > 188 {
		return need
	}
	if len(p) < pos+8 {
		return pos + 8
	}
	if p[pos+7]>>6 == 3 {
		return max(need, pos+19)
	}
	return max(need, pos+14)
}

// stamp writes one whole packet's timestamps with the offsets, moving them first when the clock jumped.
// The PCR and the PES timestamps (PTS, DTS) each keep their own offset: a provider can make only one of them
// jump (seen at ad breaks), and one offset for both then moved the other one off, back and forth. A PCR jump
// moves the PES offset along with it at once (sound of the new stream can come before its first picture), and a
// video PTS jump moves the PCR's; the next video PES (or PCR) undoes that when its own clock ran on.
func (c *clockFix) stamp(p []byte) (jumped bool) {
	pid := int(p[1]&0x1f)<<8 | int(p[2])
	pos := 4
	if p[3]&0x20 != 0 { // an adaptation field
		pos = 5 + int(p[4])
		if p[4] >= 7 && p[5]&0x10 != 0 && (c.pcrPID < 0 || pid == c.pcrPID) {
			c.pcrPID = pid
			raw := int64(p[6])<<25 | int64(p[7])<<17 | int64(p[8])<<9 | int64(p[9])<<1 | int64(p[10])>>7
			if c.tryPCR >= 0 && c.rawPCR >= 0 && !rawStep(raw, c.rawPCR) {
				c.offPCR = c.tryPCR // (only the pictures' clock jumped: the PCR runs on as it was)
			}
			c.tryPCR = -1
			v := (raw + c.offPCR) % tsWrap
			if c.jumped(v, c.last) || c.returned(true, raw, v) {
				off := ((c.offPCR+tsDiff(c.last, v)+3600)%tsWrap + tsWrap) % tsWrap // (40 ms after the last one)
				c.jumpedAt("PCR", c.last, c.offPCR, raw, off)
				c.tryOff, c.offPCR, c.off = c.off, off, off
				v = (raw + c.offPCR) % tsWrap
				jumped = true
			}
			c.last, c.rawPCR = v, raw
			p[6], p[7], p[8], p[9] = byte(v>>25), byte(v>>17), byte(v>>9), byte(v>>1)
			p[10] = p[10]&0x7f | byte(v<<7)&0x80
		}
	}
	if p[1]&0x40 == 0 || p[3]&0x10 == 0 || pos+14 > 188 { // not the start of a PES packet
		return
	}
	e := p[pos:]
	if e[0] != 0 || e[1] != 0 || e[2] != 1 || !(e[3] >= 0xc0 && e[3] <= 0xef || e[3] == 0xbd) || e[6]&0xc0 != 0x80 {
		return // (not audio, video or private stream 1 with an MPEG-2 header)
	}
	flags := e[7] >> 6
	if flags&2 == 0 || pos+19 > 188 && flags == 3 {
		return
	}
	pts := readTS(e[9:14])
	if e[3] >= 0xe0 && (c.videoPID < 0 || pid == c.videoPID) { // video: the jump may show here before any PCR
		c.videoPID = pid
		if c.tryOff >= 0 && c.rawPTS >= 0 && !rawStep(pts, c.rawPTS) {
			c.off = c.tryOff // (only the PCR jumped: the pictures' clock runs on as it was)
		}
		v := (pts + c.off) % tsWrap
		if c.jumped(v, c.lastPTS) || c.returned(false, pts, v) {
			off := ((c.off+tsDiff(c.lastPTS, v)+3600)%tsWrap + tsWrap) % tsWrap
			c.jumpedAt("video PTS", c.lastPTS, c.off, pts, off)
			c.off, c.tryPCR, c.offPCR = off, c.offPCR, off
			jumped = true
		}
		c.tryOff, c.rawPTS = -1, pts
		c.lastPTS = (pts + c.off) % tsWrap
	}
	if c.off == 0 {
		return
	}
	writeTS(e[9:14], pts+c.off)
	if flags == 3 {
		writeTS(e[14:19], readTS(e[14:19])+c.off)
	}
	return
}

// rawStep: whether a clock on the provider's own side stepped (back over 1 s, ahead over 2 s) rather than ran on.
func rawStep(v, last int64) bool {
	d := tsDiff(v, last)
	return d < -90000 || d > 2*90000
}

// syncAt: the first position at or after from where a packet starts and the next one does too, 188 bytes on (and the
// one after that, when data reaches it). When data ends before that can be seen: the first 0x47 near the end that
// could be a packet start, to be checked when more arrives. -1 when there is none.
func syncAt(data []byte, from int) int {
	j := max(from, 0)
	for ; j+188 < len(data); j++ {
		if data[j] == 0x47 && data[j+188] == 0x47 && (j+376 >= len(data) || data[j+376] == 0x47) {
			return j
		}
	}
	for ; j < len(data); j++ {
		if data[j] == 0x47 {
			return j
		}
	}
	return -1
}

// spliceAt: where a stream the provider sent again joins on to the old one. A provider asked again starts
// with its opening burst, seconds of video from before where the old stream stopped; passed on, the
// stream's clock ran backwards and players froze on it. So the new stream joins at its first keyframe
// whose clock is past the last clock passed on (pid, clock), or, if it has none within keyWait seconds past
// that clock, where its clock passed it. ok=false: not there yet (wait for more).
// A clock far from the old one (another clock entirely) is not compared: it joins at once.
func spliceAt(buf []byte, pid int, clock float64) (int, bool) {
	start := cleanStart(buf)
	if clock < 0 {
		return start, true
	}
	seen := false
	for i := start; i+188 <= len(buf) && buf[i] == 0x47; i += 188 {
		_, v, ok := lastPCR(buf[i:min(len(buf), i+188*3)], pid)
		if !ok || (i+188*3 > len(buf)) {
			continue
		}
		if !seen && math.Abs(v-clock) > 60 {
			return start, true
		}
		seen = true
		if v > clock {
			at, found := keyFound(buf[i:])
			if !found {
				if _, last, _ := lastPCR(buf[i:], pid); last-clock >= keyWait {
					return i, true
				}
			}
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
	id                 string // the channel's number, for the log
	owner              *front // the provider account it is opened for (see conns in advanced.go)
	readers            map[*reader]bool
	timer              *time.Timer
	ring               []item
	kept               int
	status             int         // 200 once video flows, or the refusal; 0 not yet
	hdr                http.Header // the provider's own answer headers for this stream, passed on to readers
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
	lastOpen = map[string]opened{}  // per provider account (see lineOf): when a channel was last opened, and which
)

type opened struct {
	at  float64
	key string
}

// lineOf: what lastOpen is kept under: the provider account (its server address and login). Two providers in the
// settings can be two accounts on one server address, and a channel opened on one account ends nothing on the
// other: kept under the address alone, a channel opened on one stopped a dropped channel of the other from being
// asked for again. (The same login written in twice is one account, as before.)
func lineOf(provider string, f *front) string {
	if f == nil {
		return provider
	}
	return provider + " " + f.user
}

func allReaders() (n int) {
	for _, c := range channels {
		n += len(c.readers)
	}
	return
}

func newChannel(key string, src source, provider string) *channel { // (mu held)
	ch := &channel{key: key, provider: provider, id: channelID(key), readers: map[*reader]bool{}, ready: make(chan struct{}), asked: mono()}
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
	case wentQuiet(err):
		if ch.status == 0 {
			ch.status = 504
		}
		lg.Warn().Str("channel", ch.id).Msg("a channel sent nothing for too long and was closed")
	default:
		if ch.status == 0 {
			ch.status = 502
		}
		lg.Warn().Str("channel", ch.id).Str("reason", pyName(err)).Msgf("a channel ended: %s", pyName(err))
	}
	ch.done = true
	lastDrop[ch.provider] = mono()
	if channels[ch.key] == ch {
		delete(channels, ch.key)
	}
	lg.Info().Str("channel", ch.id).Int("open", len(channels)).Msgf("a channel was closed (%d open now)", len(channels))
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
	// (A provider's silence is logged by the source, outside this lock: see streamStats.)
	if now-ch.born < burstSeconds {
		ch.burst += int64(len(data))
	} else if ch.burst >= 0 {
		ch.burst = -ch.burst // (measured once)
	}
	ch.ring = append(ch.ring, item{now, data})
	ch.kept += len(data)
	// A reader may be behind by the same seconds of video on every channel: 16 MB is 25 s of a 5 Mbit/s channel
	// but 6 s of a 20 Mbit/s one, where a player that stopped reading for 10 s and then read on at playing speed
	// was cut. So on a channel of a high rate the allowance is behindVideo seconds of it (never past farBehind,
	// which bounds what is kept for a reader whatever the channel). That much is in the ring for joiners anyway.
	keep, slow := ringMin, behind
	if speed, ok := ch.speed(now); ok {
		keep = min(max(ringMin, int(speed*keepSeconds)), ringMax)
		slow = min(max(behind, int(speed*behindVideo)), farBehind)
	}
	for ch.kept-len(ch.ring[0].data) >= keep {
		ch.kept -= len(ch.ring[0].data)
		ch.ring[0] = item{}
		ch.ring = ch.ring[1:]
	}
	for rd := range ch.readers {
		if rd.n <= slow+rd.given { // (what a joiner was handed at the start is not "falling behind")
			rd.behindAt = 0
		} else if rd.behindAt == 0 {
			rd.behindAt = now
		}
		if rd.n > farBehind+rd.given || (rd.behindAt != 0 && now-rd.behindAt > behindSeconds) {
			lg.Warn().Str("channel", ch.id).Str("device", rd.who).Msg("a reader was too slow to keep up with its channel and was disconnected")
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
		lg.Debug().Str("channel", ch.id).Int("kb", rd.n/1024).Msgf("a reader came back to a channel nobody else was on and was given %d KB at once", rd.n/1024)
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
		// The cushion is handed over at once, as fast as the player reads it, the way the provider hands a new
		// connection its opening burst and other restreamers hand a new client their buffer. Pacing it (as this
		// did before) left a joiner under a second of video in its first seconds, and its player stalled.
		extra := ""
		if measured {
			extra = fmt.Sprintf(" (%d s at %d KB a second)", joinSeconds, int64(speed/1024))
		}
		lg.Debug().Str("channel", ch.id).Int("kb", rd.n/1024).Msgf("a reader joined a channel and was given %d KB to start%s", rd.n/1024, extra)
	}
	ch.readers[rd] = true
	return rd
}

func (ch *channel) leave(rd *reader) { // (mu held)
	delete(ch.readers, rd)
	if len(ch.readers) > 0 || ch.done {
		return
	}
	now, op := mono(), lastOpen[lineOf(ch.provider, ch.owner)]
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
// reaskBackoff: seconds between the extra asks for a watched channel whose provider answered 404 or a server error
// right after ending it (tsSource). About a minute in all.
var reaskBackoff = []float64{2, 3, 5, 10, 10, 15, 15}

func reaskTotal() float64 {
	t := 0.0
	for _, s := range reaskBackoff {
		t += s
	}
	return t
}

// watched: someone still reads the channel.
func watched(ch *channel) bool {
	mu.Lock()
	defer mu.Unlock()
	return len(ch.readers) > 0
}

// reaskAllowed: whether an extra ask may still go out: not while a provider locked to one address waits after a
// VPN change, and not (on an account that allows one connection, or whose limit is not set) once another channel
// of this provider was opened since the drop (asking would end that one; the same rule as the first ask again).
func reaskAllowed(ch *channel, since float64) bool {
	mu.Lock()
	defer mu.Unlock()
	f, op := ch.owner, lastOpen[lineOf(ch.provider, ch.owner)]
	if f != nil && lockLeft(f.locked, f.lockWait, vpnChanged, mono()) > 0 {
		return false
	}
	single := f == nil || f.limit() <= 1
	return !(single && op.at > since && op.key != ch.key)
}

// a: the channel, the device that opened it and the provider, for the log.
// passHeaders: the provider's answer headers a player straight from the provider would get and that say nothing
// about the provider (no server, address, cookie or tracking headers). Framing (length, chunking, connection) is
// this program's own.
func passHeaders(in http.Header) http.Header {
	out := http.Header{}
	for _, k := range []string{"Content-Type", "Cache-Control", "Pragma", "Expires", "Accept-Ranges",
		"Access-Control-Allow-Origin", "Access-Control-Allow-Headers", "Access-Control-Allow-Methods",
		"Access-Control-Expose-Headers", "Access-Control-Allow-Credentials", "Access-Control-Max-Age"} {
		if v := in.Values(k); len(v) > 0 {
			if k == "Content-Type" && !strings.HasPrefix(strings.ToLower(v[0]), "video/") {
				continue
			}
			out[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	return out
}

func tsSource(target string, h http.Header, a *askLog) source {
	run := func(ctx context.Context, ch *channel, yield func([]byte) bool, st *streamStats) error {
		again, sent, tries, dropped := false, int64(0), 0, 0.0
		firstTries := 0             // a channel's first connection that went quiet, asked again (see firstQuiet)
		clockPID, clock := -1, -1.0 // the newest stream clock passed on (see spliceAt)
		var clk clockFix            // keeps the stream's clock running on when the provider's feed starts a new file
		clk.tag = st.fields
		for {
			began, got := mono(), false
			wait := float64(headerWait)
			if again {
				wait = reopenWait // readers are waiting
			} else {
				// Its first answer within firstWait, connecting included (see firstWait).
				wait = math.Min(wait, firstWait-connectWait.Seconds())
			}
			r, err := fetch(ctx, target, h, opts{wait: wait, ask: st.asking(again)})
			// A channel the provider ended while it was watched is often being restarted on the provider's side: for a
			// few seconds up to a minute or so it answers 404 or a server error, then plays again. So while anyone still
			// watches, it is asked again a few more times, further apart, before the readers are let go.
			if again && tries < len(reaskBackoff) && mono()-dropped < reaskTotal() && ctx.Err() == nil &&
				(err != nil || r.StatusCode == 404 || r.StatusCode >= 500) && watched(ch) {
				if r != nil {
					r.Body.Close()
				}
				if tries == 0 {
					what := "no answer"
					if err == nil {
						what = strconv.Itoa(r.StatusCode)
					}
					st.fields(lg.Warn()).Str("answer", what).Msgf("a provider answered %s when a watched channel was asked for again; trying again for up to %.0f s", what, reaskTotal())
				}
				if err := sleep(ctx, reaskBackoff[tries]); err != nil {
					return err
				}
				if !reaskAllowed(ch, dropped) {
					st.fields(lg.Info()).Msg("a channel is not asked for again: another channel of this provider was opened meanwhile, or it waits after a VPN change")
					return nil
				}
				tries++
				continue
			}
			if err != nil && firstQuiet(ctx, ch, again, err, &firstTries, st) {
				continue
			}
			if err != nil {
				return err
			}
			if r.StatusCode != 200 && r.StatusCode != 206 { // (logged as the provider's answer, see ask)
				text, _ := readAll(r, 65536, 5) // what the provider said, passed on to the player
				return &refused{status: r.StatusCode, kind: r.Header.Get("Content-Type"), text: text[:min(len(text), 65536)]}
			}
			if again {
				e := st.fields(lg.Info()).Int64("ms", int64((mono()-began)*1000))
				if tries > 0 {
					e.Int("tries", tries).Msgf("a channel asked for again plays again after %d more tries", tries)
				} else {
					e.Msg("a channel asked for again was answered; its video carries on")
				}
			}
			tries = 0
			passOn := passHeaders(r.Header)
			mu.Lock()
			ch.hdr = passOn
			mu.Unlock()
			err = func() error {
				defer r.Body.Close()
				setWait(r, liveQuiet)
				if again {
					setWait(r, 2*reopenWait)
				} else if !got {
					setWait(r, firstWait) // (its first video within firstWait too; then liveQuiet between reads)
				}
				clk.reask()     // (half a packet of the old stream is of no use; find the new one's packets)
				var head []byte // a stream opened again is joined on at a whole packet
				joining := again
				buf := make([]byte, 65536)
				for {
					n, err := r.Body.Read(buf)
					setWait(r, liveQuiet)
					if n > 0 {
						st.got(n) // (a few sums; a line only after a long silence or every 10 minutes)
					}
					data := append([]byte(nil), buf[:n]...)
					if joining {
						head = append(head, data...)
						if err == nil && len(head) < 16384 {
							continue
						}
						at, ok := spliceAt(head, clockPID, clk.raw(clock)) // (a stream asked for again has the provider's own clock)
						if !ok && err == nil && len(head) < 48<<20 {
							continue
						}
						if !ok {
							at = cleanStart(head)
						} else if clock >= 0 && at > 0 {
							st.fields(lg.Debug()).Int("kb", at>>10).Msgf("a channel asked for again repeated %d KB of video; it was left out", at>>10)
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
						data = clk.fix(data, sent)
					}
					if len(data) > 0 {
						if p, v, ok := lastPCR(data, clockPID); ok {
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
			if errors.Is(err, errStop) || ctx.Err() != nil {
				return err
			}
			if err != nil && !got && firstQuiet(ctx, ch, again, err, &firstTries, st) {
				continue
			}
			if err != nil && !got {
				// A channel asked for again whose new connection answered but sent nothing (it went quiet or broke
				// off too): asked again, as a player straight at the provider reconnects, with the same extra asks
				// and gates as a 404 above (about a minute from the drop, while anyone watches).
				if !again || mono()-dropped >= reaskTotal() || !watched(ch) {
					return err
				}
				if err := sleep(ctx, reaskBackoff[min(tries, len(reaskBackoff)-1)]); err != nil {
					return err
				}
				if !reaskAllowed(ch, dropped) {
					st.fields(lg.Info()).Msg("a channel is not asked for again: another channel of this provider was opened meanwhile, or it waits after a VPN change")
					return nil
				}
				tries++
				continue
			}
			// The provider ended it. Ask again if anyone still watches, unless it ran under reopenAfter, or (on an
			// account that allows one connection, or whose limit is not set) another channel of this provider opened
			// in the last 15 s: asking would end that one.
			mu.Lock()
			ran, op := mono()-began, lastOpen[lineOf(ch.provider, ch.owner)]
			// (an account that allows more than one connection: opening another channel did not end this one)
			single := ch.owner == nil || ch.owner.limit() <= 1
			// (a connection that went quiet is asked again however long it ran: with the old 60 s wait it always
			// ran past reopenAfter, and the shorter liveQuiet must not turn that into letting its readers go)
			stop := len(ch.readers) == 0 || (ran < reopenAfter && !wentQuiet(err)) || (single && op.at > math.Max(began, mono()-15) && op.key != ch.key)
			mu.Unlock()
			if stop {
				return nil
			}
			again, dropped = true, mono()
			st.reasks++
			why := endReason(err)
			st.fields(lg.Warn()).Str("reason", why).Float64("seconds", round1(ran)).
				Msg("a provider ended a channel that was being watched; asking for it again")
			if err := sleep(ctx, dropGap); err != nil {
				return err
			}
		}
	}
	return func(ctx context.Context, ch *channel, yield func([]byte) bool) error {
		st := newStreamStats(a)
		err := run(ctx, ch, yield, st)
		st.end(ctx, err)
		return err
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
// tuneWatch: what a player got in the first tuneWatchFor seconds after it asked for a channel, logged once: seconds
// of video (by the stream's clock) in hand at 2, 5 and 10 s, the lowest spare video (video sent minus time since
// the first video went out) and the longest pause between sends. A spinner just after a tune shows here as spare
// video at or below zero. It only reads what was already sent; nothing waits on it.
type tuneWatch struct {
	done            bool
	pid             int
	have            bool
	pcr0, span      float64
	first, last     float64 // when the first video went out, and the last send
	gap, gapAt      float64
	low, lowAt      float64
	at2, at5, at10  float64
	got2, got5, g10 bool
}

const tuneWatchFor = 30

func (tw *tuneWatch) see(data []byte, rd *reader, provider string) {
	now := mono()
	if tw.first == 0 {
		tw.first, tw.low = now, 1e9
	}
	since := now - rd.came
	if tw.last > 0 && now-tw.last > tw.gap {
		tw.gap, tw.gapAt = now-tw.last, since
	}
	tw.last = now
	if pid, v, ok := lastPCR(data, tw.pid); ok {
		tw.pid = pid
		if !tw.have {
			tw.pcr0, tw.have = v, true
		}
		d := v - tw.pcr0
		if d < -1000 { // (the clock's wrap)
			d += float64(int64(1)<<33) / 90000
		}
		if d > tw.span && d < 600 {
			tw.span = d
		}
	}
	if spare := tw.span - (now - tw.first); tw.have && now-tw.first >= 1 && spare < tw.low {
		tw.low, tw.lowAt = spare, since
	}
	if !tw.got2 && since >= 2 {
		tw.at2, tw.got2 = tw.span, true
	}
	if !tw.got5 && since >= 5 {
		tw.at5, tw.got5 = tw.span, true
	}
	if !tw.g10 && since >= 10 {
		tw.at10, tw.g10 = tw.span, true
	}
	if since >= tuneWatchFor {
		tw.done = true
		low := tw.low
		if low == 1e9 {
			low = 0
		}
		ev := lg.Info()
		if low <= 0.5 {
			ev = lg.Warn()
		}
		ev.Str("device", rd.who).Str("channel", rd.id).Str("provider", provider).Float64("video_at_2s", round1(tw.at2)).
			Float64("video_at_5s", round1(tw.at5)).Float64("video_at_10s", round1(tw.at10)).Float64("lowest_spare_s", round1(low)).
			Float64("lowest_at_s", round1(tw.lowAt)).Float64("longest_pause_s", round1(tw.gap)).Float64("pause_at_s", round1(tw.gapAt)).
			Msgf("tune: %s had %.1f s of video at 2 s, %.1f at 5 s, %.1f at 10 s; lowest spare %.1f s (at %.0f s); longest pause %.1f s (at %.0f s)",
				rd.who, tw.at2, tw.at5, tw.at10, low, tw.lowAt, tw.gap, tw.gapAt)
	}
}

// viewWatch: a reader of live MPEG-TS, watched for as long as it stays (tuneWatch covers only its first seconds).
// It only counts and logs: nothing about what is sent, or when, depends on it. The count is the one tuneWatch
// keeps: the seconds of video sent to the player, by the stream's own clock, less the seconds gone by since the
// first of it went out. A player that plays from the first video on has that much left; one that starts nearer
// the newest video has less, so the number is the most a player can have, not what it has.
//   - When the count is at or under zero as new video is about to go out, the player had nothing left while it
//     waited: a warning says for how long, and how long the wait for that video was.
//   - When one send takes slowSend seconds or more, the player (or the line to it) was not taking video: a warning.
//   - When the reader leaves, one line: how long before that the provider last sent, how long before that the
//     player last took video, how much waited for it, and the lowest and longest of the above over its stay.
type viewWatch struct {
	pid              int
	have             bool
	pcr, span        float64 // the last clock value sent; seconds of video sent since base
	base, last       float64 // when counting began (the first send, or the last time it ran out); when the last send ended
	low, lowAt       float64
	gap, gapAt       float64 // the longest wait between two sends
	slow, slowAt     float64 // the longest one send took
	dry              int
	drySaid, slowSay float64 // when each warning was last logged
}

const (
	slowSend    = 2.0  // a send that takes this many seconds is logged
	viewSayGap  = 30.0 // at most one warning of each kind per reader in this many seconds
	dryRegather = 2.0  // a player that ran out is taken to gather this many seconds of video before it plays on
)

func (vw *viewWatch) see(data []byte, began, now float64, rd *reader, provider string) {
	if vw.base == 0 {
		vw.base, vw.low = began, 1e9
	}
	since := now - rd.came
	waited := 0.0
	if vw.last > 0 {
		waited = began - vw.last
	}
	if waited > vw.gap {
		vw.gap, vw.gapAt = waited, since
	}
	took := now - began
	if took > vw.slow {
		vw.slow, vw.slowAt = took, since
	}
	if took >= slowSend && (vw.slowSay == 0 || now-vw.slowSay >= viewSayGap) {
		vw.slowSay = now
		lg.Warn().Str("device", rd.who).Str("channel", rd.id).Str("provider", provider).Float64("send_took_s", round1(took)).Float64("at_s", round1(since)).
			Msgf("%s did not take video for %.1f s on channel %s (one send to it took that long)", rd.who, took, rd.id)
	}
	if vw.have && began-vw.base >= 1 { // what the player had left as this video was about to go out
		spare := vw.span - (began - vw.base)
		if spare < vw.low {
			vw.low, vw.lowAt = spare, since
		}
		if spare <= 0 {
			vw.dry++
			if vw.drySaid == 0 || now-vw.drySaid >= viewSayGap {
				vw.drySaid = now
				lg.Warn().Str("device", rd.who).Str("channel", rd.id).Str("provider", provider).Float64("out_for_s", round1(-spare)).
					Float64("waited_s", round1(waited)).Float64("at_s", round1(since)).
					Msgf("%s ran out of video on channel %s: it had none left for %.1f s; the wait for more was %.1f s", rd.who, rd.id, -spare, waited)
			}
			vw.base, vw.span = began, dryRegather
		}
	}
	vw.last = now
	if pid, v, ok := lastPCR(data, vw.pid); ok {
		vw.pid = pid
		if vw.have {
			d := v - vw.pcr
			if d < -1000 { // (the clock's wrap)
				d += float64(int64(1)<<33) / 90000
			}
			if d > 0 && d < 60 {
				vw.span += d
			}
		}
		vw.pcr, vw.have = v, true
	}
}

// left: the one line when the reader leaves (fed: when its channel last got video from the provider; waiting:
// the bytes that waited for the reader). Nothing is logged for a reader that was sent no video.
func (vw *viewWatch) left(rd *reader, provider string, fed float64, waiting int) {
	if vw.base == 0 {
		return
	}
	now, low, quiet := mono(), vw.low, 0.0
	if low == 1e9 {
		low = 0
	}
	if fed > 0 {
		quiet = now - fed
	}
	lg.Info().Str("device", rd.who).Str("channel", rd.id).Str("provider", provider).Float64("provider_quiet_s", round1(quiet)).
		Float64("player_quiet_s", round1(now-vw.last)).Float64("waiting_mb", mb(int64(waiting))).Float64("lowest_spare_s", round1(low)).
		Float64("lowest_at_s", round1(vw.lowAt)).Float64("longest_wait_s", round1(vw.gap)).Float64("wait_at_s", round1(vw.gapAt)).
		Float64("slowest_send_s", round1(vw.slow)).Float64("slow_at_s", round1(vw.slowAt)).Int("ran_out", vw.dry).
		Msgf("when %s left channel %s: the provider had last sent video %.1f s before, the player had last taken video %.1f s before, %.1f MB waited for it; over its stay: lowest spare %.1f s (at %.0f s), longest wait for video %.1f s (at %.0f s), slowest send %.1f s (at %.0f s), ran out of video %d time(s)",
			rd.who, rd.id, quiet, now-vw.last, float64(waiting)/1048576, low, vw.lowAt, vw.gap, vw.gapAt, vw.slow, vw.slowAt, vw.dry)
}

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
		releaseFor(rt.f, provider, rt.client, key)
	}
	if first {
		// Changing channel: the hang-up on the old one and this request arrive in either order. Channels of
		// this provider nobody reads go now; a hang-up just behind gets 30 ms; the provider gets dropGap to see it.
		was := lastOpen[lineOf(provider, rt.f)]
		lastOpen[lineOf(provider, rt.f)] = opened{mono(), key}
		releaseFor(rt.f, provider, rt.client, key)
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
			unOpen(lineOf(provider, rt.f), key, was)
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
			src := tsSource(provider+uri, sent, &askLog{kind: "stream", id: channelID(uri), device: rt.client, provider: rt.f.name})
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
	vw := &viewWatch{pid: -1}
	why, started := "the player hung up", false
	defer func() {
		mu.Lock()
		still := max(0, len(ch.readers)-1)
		fed, waiting := 0.0, rd.n // (for the log: when the provider last sent, and what waited for this reader)
		if n := len(ch.ring); n > 0 {
			fed = ch.ring[n-1].t
		}
		ch.leave(rd)
		mu.Unlock()
		if started {
			lg.Info().Str("device", rd.who).Str("channel", rd.id).Str("provider", rt.f.name).Float64("seconds", round1(mono()-rd.came)).
				Float64("mb", mb(rd.written.Load())).Str("reason", why).Int("still_on", still).
				Msgf("%s left channel %s after %s, %.1f MB sent: %s (%d still on it)", rd.who, rd.id, hms(mono()-rd.came), float64(rd.written.Load())/1048576, why, still)
		} else {
			lg.Info().Str("device", rd.who).Str("channel", rd.id).Str("provider", rt.f.name).Str("reason", why).Int("still_on", still).
				Msgf("a reader left after %d s, before any video: %s (%d still on its channel)", int64(mono()-rd.came), why, still)
		}
		vw.left(rd, rt.f.name, fed, waiting)
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
		lg.Info().Str("device", rd.who).Str("channel", rd.id).Str("provider", rt.f.name).Int("status", status).
			Msg("a channel could not be opened; the player is told why")
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
	mu.Lock()
	for k, v := range ch.hdr { // the provider's own headers for this stream, as a player straight from it gets them
		w.Header()[k] = append([]string(nil), v...)
	}
	mu.Unlock()
	w.Header().Set("Transfer-Encoding", "identity")
	w.Header().Set("Connection", "close")
	w.WriteHeader(200)
	rc.Flush()
	mu.Lock()
	rd.cut = func() { rc.SetWriteDeadline(time.Now()) }
	mu.Unlock()
	started = true
	tw := &tuneWatch{pid: -1}
	if first {
		lg.Info().Str("device", rd.who).Str("channel", rd.id).Str("provider", rt.f.name).Float64("first_video_s", round1(born-asked)).
			Msgf("%s started channel %s (%s), first video in %.1f s", rd.who, rd.id, hostOf(provider), born-asked)
	} else {
		lg.Info().Str("device", rd.who).Str("channel", rd.id).Str("provider", rt.f.name).
			Msgf("%s joined channel %s (%s)", rd.who, rd.id, hostOf(provider))
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
				lg.Debug().Str("device", rd.who).Str("channel", rd.id).Msg("a reader that joined its channel has been handed everything kept for it; it is now sent video as it arrives")
			}
			mu.Unlock()
			began := mono()
			n, err := w.Write(data)
			if err == nil {
				err = rc.Flush()
			}
			rd.written.Add(int64(n))
			if err == nil && n > 0 && !tw.done {
				tw.see(data, rd, rt.f.name)
			}
			if err == nil && n > 0 {
				vw.see(data, began, mono(), rd, rt.f.name)
			}
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
