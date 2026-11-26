// HLS: the "/hls" bridge's source, playlists with one provider session per live channel, the provider's
// lists (player_api.php) and the /__cdn/ name check.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/rs/zerolog"
)

var errNotHLS = errors.New("not joinable HLS")

type playlist struct {
	seq            int64
	target, newest float64
	pieces         []string
	best           string // the best quality on offer, when the playlist lists qualities
	ended          bool
}

var (
	extinfRe = regexp.MustCompile(`^#EXTINF:\s*(\d+(?:\.\d*)?)`)
	nonDigit = regexp.MustCompile(`\D`)
	nonNum   = regexp.MustCompile(`[^0-9.]`)
	bwRe     = regexp.MustCompile(`[:,]BANDWIDTH=(\d+)`)
)

func readPlaylist(text string) (p playlist, err error) {
	if !strings.Contains(text, "#EXTM3U") {
		return p, errNotHLS
	}
	p.target = 4
	length, rate, bestRate := 0.0, int64(-1), int64(-1)
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXTINF:"): // a piece's playing time (0 if unreadable)
			length = 0
			if m := extinfRe.FindStringSubmatch(line); m != nil {
				length, _ = strconv.ParseFloat(m[1], 64)
			}
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			p.seq, _ = strconv.ParseInt(nonDigit.ReplaceAllString(line[22:], ""), 10, 64)
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			if s := nonNum.ReplaceAllString(line[22:], ""); s != "" {
				if p.target, err = strconv.ParseFloat(s, 64); err != nil {
					return p, err
				}
			}
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			rate = 0
			if m := bwRe.FindStringSubmatch(line); m != nil {
				rate, _ = strconv.ParseInt(m[1], 10, 64)
			}
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			p.ended = true
		case strings.HasPrefix(line, "#EXT-X-MAP") || strings.HasPrefix(line, "#EXT-X-BYTERANGE") ||
			strings.HasPrefix(line, "#EXT-X-KEY") && !strings.Contains(line, "METHOD=NONE"):
			return p, errNotHLS // locked, or not whole MPEG-TS files: cannot be joined end to end
		case line != "" && !strings.HasPrefix(line, "#"):
			if rate >= 0 {
				if rate > bestRate {
					p.best, bestRate = line, rate
				}
				rate = -1
			} else {
				p.pieces = append(p.pieces, line)
				p.newest, length = length, 0
			}
		}
	}
	return p, nil
}

// hlsSource: the channel's HLS pieces end to end, fetched from this proxy's own front door (via: inside
// the program, no socket) at the players' own addresses so the reader shares their one provider session;
// each piece handed on whole the moment it is fetched, as streamlink's HLS writer and Threadfin's buffer do.
func hlsSource(start, agent string, via http.RoundTripper) source {
	h, home := agentOf(agent), ""
	if u, err := url.Parse(start); err == nil {
		home = u.Host
	}
	inside := func(base, ref string) (*url.URL, error) { // the playlist's addresses must be this proxy's own
		b, err := url.Parse(base)
		if err != nil {
			return nil, err
		}
		u, err := b.Parse(ref)
		if err == nil && u.Host != home {
			err = errNotHLS
		}
		return u, err
	}
	look := func(ctx context.Context) (string, playlist, error) { // always from the channel's own address
		at := start
		for i := 0; i < 4; i++ {
			t0 := mono()
			status, data, err := getWithin(ctx, at, h, 2<<20, via, pieceWait) // (a quiet connection: asked again)
			if took := mono() - t0; took > 2 && ctx.Err() == nil {
				lg.Warn().Float64("seconds", round1(took)).Bool("got", err == nil).
					Msgf("a playlist took %.1f s to fetch", took)
			}
			if err != nil {
				return "", playlist{}, err
			}
			if status != 200 {
				return "", playlist{}, &refused{status: status}
			}
			pl, err := readPlaylist(string(data))
			if err != nil || pl.best == "" {
				return at, pl, err
			}
			u, err := inside(at, pl.best)
			if err != nil {
				return "", playlist{}, err
			}
			at = u.String()
		}
		return "", playlist{}, errNotHLS
	}
	return func(ctx context.Context, ch *channel, yield func([]byte) bool) error {
		// wait until `until`, or until job is done (nothing is held meanwhile: there is nothing to hand on)
		wait := func(until float64, job chan struct{}) error {
			if job != nil {
				select {
				case <-job:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			nap := until - mono()
			if nap <= 0 {
				return nil
			}
			t := time.NewTimer(secs(nap))
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		do := func(f func(context.Context)) error { // a request under way; called off if the feed ends
			jctx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan struct{})
			go func() { f(jctx); close(done) }()
			return wait(0, done)
		}
		var last int64
		hasLast, misses, lost, got, fresh := false, 0, 0, 0.0, mono()
		got1 := 0.0 // when the last try for a piece began
		// the numbers readers were handed first; a piece numbered afresh (after a discontinuity: another
		// packager, an advert) is handed on under them (see renumber.go)
		var pub numbering
		var rn *renumber
		var sent []string // names of the newest 60 pieces
		// Where a piece's clock starts over (a 24/7 channel's next episode), the MPEG-TS made here carries it on,
		// with the same clockFix as a provider's own MPEG-TS (tsSource): recorders read a clock that starts over
		// as time going backwards. Pieces whose clock runs on are handed on unchanged.
		var clk clockFix
		clk.tag = func(e *zerolog.Event) *zerolog.Event { return e.Str("channel", ch.id) }
		var passed int64 // bytes handed on so far
		for {
			began := mono()
			var at string
			var pl playlist
			var err error
			if e := do(func(c context.Context) { at, pl, err = look(c) }); e != nil {
				return e
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil { // no playlist this time: ask again, as a player would (4 tries at start, 9 once playing)
				misses++
				var r *refused
				status := 502
				if errors.As(err, &r) {
					status = r.status
				}
				if (hasLast && misses > 8) || (!hasLast && misses > 3) {
					return &refused{status: status}
				}
				nap := 1.0
				if hasLast {
					nap = 2
				}
				if !errors.As(err, &r) { // (no answer at all, a quiet connection: ask again at once, on a new one)
					nap = 0
				}
				if e := wait(mono()+nap, nil); e != nil {
					return e
				}
				continue
			}
			misses = 0
			top := pl.seq + int64(len(pl.pieces)) - 1
			afresh := hasLast && (top < last || pl.seq > last+1)
			opening := !hasLast || afresh
			if opening { // joining, or the numbering started afresh: start near the newest video
				last, hasLast = max(pl.seq-1, top-3), true
			}
			first := last + 1 - pl.seq
			length := pl.target // a piece's playing time, as the playlist gives it
			if 0 < pl.newest && pl.newest <= 2*pl.target {
				length = pl.newest
			}
			isNew := first < int64(len(pl.pieces))
			for i := max(first, 0); i < int64(len(pl.pieces)); i++ {
				number := pl.seq + i
				last = number
				u, err := inside(at, pl.pieces[i])
				if errors.Is(err, errNotHLS) { // a line that does not lead back here: not fetched
					lost++
					continue
				} else if err != nil {
					return err
				}
				if afresh && contains(sent, u.Path) { // already passed on under its old number
					continue
				}
				var data []byte
				for try := 0; try < 3 && data == nil; try++ {
					if try > 0 && mono()-got1 < 1 { // (refused at once: asked again a second later, not in the same instant)
						if e := wait(got1+1, nil); e != nil {
							return e
						}
					}
					t0 := mono()
					got1 = t0
					if e := do(func(c context.Context) {
						if status, d, err := getWithin(c, u.String(), h, 64<<20, via, pieceWait); err == nil && status == 200 && len(d) > 0 {
							data = d
						}
					}); e != nil {
						return e
					}
					if took := mono() - t0; took > 2 {
						ev := lg.Warn()
						if data != nil {
							ev = lg.Info()
						}
						ev.Str("channel", ch.id).Float64("seconds", round1(took)).Bool("got", data != nil).Int("try", try+1).
							Msgf("a piece of video took %.1f s to fetch on channel %s", took, ch.id)
					}
				}
				if data == nil {
					lost++
					lg.Warn().Str("channel", ch.id).Msg("a piece of video could not be fetched and is missing from a stream")
					// A piece that cannot be had is left out and the playlist followed on, as players and tools
					// that read HLS do (FFmpeg's hls demuxer skips it; Media3 retries, then goes on). The feed
					// is given up only once no piece at all has come for as long as a playlist that stopped
					// moving is waited for (below): a video server answering 404 for a few seconds while the
					// provider restarts a channel used to end a recording within a second.
					if lost >= 3 && mono()-fresh > math.Max(30, 6*pl.target) {
						return &refused{status: 502}
					}
					continue
				}
				lost = 0
				if sent = append(sent, u.Path); len(sent) > 60 {
					sent = sent[1:]
				}
				now := mono()
				if got != 0 && now-got > math.Max(gapWarn, 1.5*length) { // a piece well later than its length
					lg.Warn().Str("channel", ch.id).Float64("seconds", round1(now-got)).
						Msgf("the provider sent nothing for %.0f s on channel %s, then carried on", now-got, ch.id)
				}
				got, fresh = now, now
				if p, ok := programOf(data); ok && pub.es == nil {
					pub = p
				} else if ok {
					rn = renumbering(p, pub)
				}
				if rn != nil {
					rn.part = nil
					data = rn.apply(data)
				}
				if data = clk.fix(data, passed); len(data) == 0 {
					continue
				}
				passed += int64(len(data))
				var parts [][]byte // whole 188-byte packets
				for j := 0; j < len(data); j += 65424 {
					parts = append(parts, data[j:min(j+65424, len(data))])
				}
				// Handed on whole, at once, as a player reading the provider's HLS has it once fetched
				// (streamlink HLSStreamWriter._write_into_buffer; Threadfin bufferingStream).
				for _, p := range parts {
					if !yield(p) {
						return errStop
					}
				}
			}
			if !isNew && (pl.ended || mono()-fresh > math.Max(30, 6*pl.target)) {
				return nil // the playlist finished, or stopped moving
			}
			// Ask again at the HLS spec's pace (RFC 8216bis 6.3.4): after a change, the newest piece's length
			// from when this look began; after none, half the target duration.
			nap := pl.target / 2
			if isNew {
				nap = pl.target
				if 0 < pl.newest && pl.newest < pl.target {
					nap = pl.newest
				}
			}
			if e := wait(began+math.Max(0.5, nap), nil); e != nil {
				return e
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Playlists: every address pointed back here; ONE provider session per live channel
// ---------------------------------------------------------------------------
// Addresses become /__cdn/<scheme>/<server>/<path>, resolved as a player resolves them. Many providers start
// a new session (ending the last) at every open of a channel's address, so the first open is remembered and
// the player is given /__ch/<channel>/__index.m3u8; later looks go to the remembered session. A session
// that fails is opened again ONCE; one unused for a minute is forgotten.

var (
	uriInTag  = regexp.MustCompile(`(?i)(URI=")([^"]*)(")`)
	hasScheme = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*:`)
	plSkipRe  = regexp.MustCompile(`(?i)^(host|connection|keep-alive|proxy-.*|te|trailer|transfer-encoding|upgrade|content-length|content-encoding` +
		`|range|if-range|content-range|accept-ranges|accept-encoding|x-accel-.*|server|date|via|x-relay-.*|x-hls-.*|x-pl-.*|x-real-ip|x-forwarded-.*)$`)
	realRe   = regexp.MustCompile(`(?i)^/__cdn/(https?)/([^/?]+)(/[^?]*)?(\?.*)?$`)
	cdnRe    = regexp.MustCompile(`(?i)^/__cdn/(https?)/([^/?]+)(/[^?]*)?$`)
	chRe     = regexp.MustCompile(`^/__ch(/.+?\.m3u8)/(.*)$`)
	renewOn  = map[int]bool{400: true, 401: true, 403: true, 404: true, 410: true, 429: true, 500: true, 502: true, 503: true, 504: true, 509: true, 520: true, 521: true, 522: true, 523: true, 524: true}
	sessions = map[string]*session{}
	locks    = map[string]chan struct{}{}
	hlsSeen  = map[string]float64{} // channels whose playlist was just asked for (status page count)
	hlsWho   = map[string]float64{} // (player, channel) -> when its "opened" line was written
	hlsRows  = map[string]*hlsRow{} // (player, provider, channel) -> its playlist requests (status page table)
)

type session struct {
	loc, final string // where the provider sent the channel; the playlist's real address
	used       float64
	opened     float64            // when it was opened at the provider
	what       what               // what it is, as the advanced options count it (advanced.go)
	who        map[string]float64 // device -> its last playlist request
	pieces     map[string]bool    // the addresses its playlist last gave the players (see inUse)
	owner      *front             // the provider account it is opened for (see conns in advanced.go)
	target     float64            // its playlist's target duration (0: not known yet)
	seq        int64              // its playlist's MEDIA-SEQUENCE when last read (hasSeq: read at all)
	hasSeq     bool
	refusedAt  float64 // when opening the channel again in its place was last refused (see renewGap)
}

// renewGap: after the provider refused to open a failed session's channel again, the session's own answer is
// passed on for this long before the channel is asked for again. A player straight at the provider retries its
// own playlist address, at most this far apart (hls.js playlistLoadPolicy errorRetry maxRetryDelayMs 8000),
// and does not open the channel at every retry.
const renewGap = 8.0

var (
	targetRe = regexp.MustCompile(`(?m)^\s*#EXT-X-TARGETDURATION:\s*([0-9]+(?:\.[0-9]*)?)`)
	seqTagRe = regexp.MustCompile(`(?m)^\s*#EXT-X-MEDIA-SEQUENCE:\s*([0-9]+)`)
)

// note: what a playlist of the session says about it: its target duration, which players wait between
// asks for it (at most a minute is taken from it). (mu held)
func (s *session) note(data []byte) {
	if m := targetRe.FindSubmatch(data); m != nil {
		if t, err := strconv.ParseFloat(string(m[1]), 64); err == nil && t > 0 {
			s.target = math.Min(t, 60)
		}
	}
	if m := seqTagRe.FindSubmatch(data); m != nil {
		if n, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
			s.seq, s.hasSeq = n, true
		}
	}
}

// held: how long after its last request the session still counts as a connection; idle: how long until it
// is the first let go of. Players ask for a playlist about once a target duration, so a playlist with long
// pieces is asked for less often: three and two target durations, never less than hlsHeld and hlsIdle.
func (s *session) held() float64 { return math.Max(hlsHeld, 3*s.target) }
func (s *session) idle() float64 { return math.Max(hlsIdle, 2*s.target) }

// playlistPieces: the addresses a playlist as given to the players names.
func playlistPieces(text string) map[string]bool {
	pieces := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if a := strings.TrimSpace(line); a != "" && !strings.HasPrefix(a, "#") {
			pieces[a] = true
		}
	}
	return pieces
}

type hlsRow struct {
	first, last             float64
	who, id, provider, port string
	bytes                   int64 // video pieces sent to this player on this channel
	catchUp                 bool  // a catch-up watched as HLS (see catchUpRead), not a live channel
}

// format: what the status list calls the row.
func (h *hlsRow) format() string {
	if h.catchUp {
		return "HLS catch-up"
	}
	return "HLS"
}

// cdnAddress: this proxy's address for a full http(s) address ("" for anything else).
func cdnAddress(u *url.URL) string {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	host, path := u.Host, u.EscapedPath()
	if u.User != nil {
		host = u.User.String() + "@" + host // (the front door refuses these)
	}
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return "/__cdn/" + u.Scheme + "/" + host + path
}

// realAddress: the real address behind /__cdn/<scheme>/<server>/<path> ("" for anything else).
func realAddress(local string) string {
	m := realRe.FindStringSubmatch(local)
	if m == nil {
		return ""
	}
	if m[3] == "" {
		m[3] = "/"
	}
	return strings.ToLower(m[1]) + "://" + m[2] + m[3] + m[4]
}

// rewritePlaylist points every address back at this proxy. Addresses on the provider's own server (own), however
// written, come back to its port with the easy login, so the real one is never written into the playlist.
// (A server the provider sends the player to gets its addresses as they are, under /__cdn/.)
func rewritePlaylist(text, base string, own *front) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	move := func(s string) (string, error) {
		ref := strings.TrimSpace(s)
		full, err := b.Parse(ref)
		if err != nil && !hasScheme.MatchString(ref) {
			// A relative address Go will not read as written, which players read anyway: a colon in its
			// first part ("chunk_23:00:00.ts", RFC 3986 wants "./chunk_..."), or a stray control
			// character. Read it the way a player does.
			ref = "./" + strings.Map(func(r rune) rune {
				if r < 0x20 || r == 0x7f {
					return -1
				}
				return r
			}, ref)
			full, err = b.Parse(ref)
		}
		if err != nil {
			return "", err // an absolute address nothing could reach: refuse rather than pass a login through
		}
		if own != nil && own.ownServer(full) {
			if p, ok := own.easyForm(full); ok {
				return p, nil
			}
		}
		if c := cdnAddress(full); c != "" {
			return c, nil
		}
		return s, nil
	}
	lines := strings.Split(text, "\n")
	for n, line := range lines {
		core := strings.TrimSpace(line)
		if n == 0 || core == "" {
			continue
		}
		if !strings.HasPrefix(core, "#") { // an address
			moved, err := move(core)
			if err != nil {
				return "", err
			}
			if strings.HasSuffix(line, "\r") {
				moved += "\r"
			}
			lines[n] = moved
			continue
		}
		var out strings.Builder // a tag: addresses sit in URI="..."
		at := 0
		for _, m := range uriInTag.FindAllStringSubmatchIndex(line, -1) {
			moved, err := move(line[m[4]:m[5]])
			if err != nil {
				return "", err
			}
			out.WriteString(line[at:m[4]] + moved + line[m[6]:m[7]])
			at = m[1]
		}
		lines[n] = out.String() + line[at:]
	}
	return strings.Join(lines, "\n"), nil
}

// send passes an answer on: the server's headers except what belongs to one connection.
func send(w http.ResponseWriter, status int, h http.Header, data []byte) {
	for k, vs := range h {
		if !plSkipRe.MatchString(k) {
			w.Header()[k] = vs
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	w.Write(data)
}

// askedAt: the address the player reached this proxy at ("http://192.0.2.7:8095"), "" if it did not say.
var hostOK = regexp.MustCompile(`^[A-Za-z0-9.\-\[\]:]+$`)

func askedAt(r *http.Request) string {
	if r == nil || !hostOK.MatchString(r.Host) {
		return ""
	}
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

// fullAddresses: a complete playlist (a catch-up's, a film's: it ends with #EXT-X-ENDLIST) with every address
// in it written in full, at the address the player reached this proxy at. A provider may name its pieces by
// path alone, and straight at the provider that works: the path leads back to the provider's own server.
// Behind another server it does not. An app that was sent here by another server (a guide server that
// redirects streams to this proxy) and checks a catch-up's first piece works the piece's address out from
// the address it first asked, the other server's, where "/__cdn/..." leads nowhere. The catch-up was then
// taken for missing. A live playlist is left as it is.
func fullAddresses(text, at string) string {
	if at == "" || !strings.Contains(text, "#EXT-X-ENDLIST") {
		return text
	}
	lines := strings.Split(text, "\n")
	for n, line := range lines {
		switch {
		case strings.HasPrefix(line, "/"):
			lines[n] = at + line
		case strings.HasPrefix(line, "#") && strings.Contains(line, `URI="/`):
			lines[n] = strings.ReplaceAll(line, `URI="/`, `URI="`+at+`/`)
		}
	}
	return strings.Join(lines, "\n")
}

// servePlaylist rewrites a playlist; anything else is passed on as it came. r: the player's request.
func servePlaylist(w http.ResponseWriter, status int, h http.Header, data []byte, base string, own *front, film *speed, r *http.Request) error {
	if bytes.Contains(data[:min(len(data), 1024)], []byte("#EXTM")) {
		text, err := rewritePlaylist(string(data), base, own)
		if err != nil {
			return err
		}
		if film != nil && status < 300 { // a film's: what it names is read no faster than set (advanced.go)
			own.noteFilm(text, film)
		}
		data = []byte(fullAddresses(text, askedAt(r)))
	} else if status >= 400 && own != nil {
		data = own.hideLogin(data) // an error page (see hideLogin)
	}
	send(w, status, h, data)
	return nil
}

// fetchShared fetches a playlist on a video server over the connections kept open to that server (the same
// ones the pass-through uses), after checking where the name leads. Redirects are followed here, one hop at
// a time, so every hop is checked. Returns the answer and where it really came from.
// fresh: on a new connection of its own instead (see livePlaylist).
func fetchShared(ctx context.Context, target string, h http.Header, wait float64, a *askLog, fresh bool) (*http.Response, string, error) {
	for hop := 0; hop < 10; hop++ {
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || cdnForbidden.MatchString(cdnAddress(u)) {
			return nil, "", &refused{status: 403}
		}
		if home, err := leadsHome(ctx, u.Hostname()); err != nil {
			return nil, "", err
		} else if home {
			return nil, "", &refused{status: 403}
		}
		r, err := fetch(ctx, target, h, opts{noFollow: true, guardAll: true, shared: !fresh, fresh: fresh, wait: wait, ask: a})
		if err != nil || !isRedirect(r.StatusCode) || r.Header.Get("Location") == "" {
			return r, target, err
		}
		r.Body.Close()
		next, err := r.Request.URL.Parse(r.Header.Get("Location"))
		if err != nil {
			return nil, "", &refused{status: 502}
		}
		target = next.String()
	}
	return nil, "", &refused{status: 502}
}

// livePlaylist fetches a playlist for a player and reads it whole. A kept connection that sends nothing for
// pieceWait (ExoPlayer's default read timeout, as for the bridge in hlsSource) is let go, and the playlist is
// asked for again at once on a new connection with the full wait, before anything counts as failed. Otherwise
// one quiet connection held the player's reload for the full wait, and then the channel was opened again at
// the provider (renewOn 502), which some providers refuse for minutes.
func livePlaylist(ctx context.Context, target string, h http.Header, wait float64, a *askLog) (int, http.Header, []byte, string, error) {
	try := func(fresh bool) (int, http.Header, []byte, string, error) {
		tctx, cancel := context.WithCancel(ctx)
		defer cancel()
		limit, w := time.Duration(math.MaxInt64), wait
		if !fresh {
			limit, w = secs(pieceWait), math.Min(wait, pieceWait)
		}
		var timed atomic.Bool
		t := time.AfterFunc(limit, func() { timed.Store(true); cancel() })
		defer t.Stop()
		resp, fin, err := fetchShared(tctx, target, h, w, a, fresh)
		var data []byte
		if err == nil {
			if data, err = readAll(resp, 4<<20, w); err == nil {
				return resp.StatusCode, resp.Header, data, fin, nil
			}
		}
		if timed.Load() {
			err = timeoutError{}
		}
		return 0, nil, nil, "", err
	}
	status, hd, data, fin, err := try(false)
	if err != nil && wentQuiet(err) && ctx.Err() == nil {
		lg.Info().Str("server", hostOf(target)).Msgf("a playlist sent nothing for %d s; asked again on a new connection", pieceWait)
		status, hd, data, fin, err = try(true)
	}
	return status, hd, data, fin, err
}

func forgetIdle() { // (mu held)
	now := mono()
	for k, s := range sessions {
		if now-s.used > math.Max(60, s.held()) {
			delete(sessions, k)
		}
	}
	for k, l := range locks {
		if _, ok := sessions[k]; !ok && len(l) == 0 {
			delete(locks, k)
		}
	}
	for k, t := range hlsWho {
		if now-t > 60 {
			delete(hlsWho, k)
		}
	}
	for k, row := range hlsRows {
		if now-row.last > 30 {
			delete(hlsRows, k)
		}
	}
}

type direct struct { // the provider answered without sending the channel elsewhere: passed on as is
	status int
	h      http.Header
	data   []byte
}

// openChannel asks the provider once and remembers where it sent the channel; later opens wait up to 20 s.
func openChannel(key, target string, h http.Header, renewFrom string, a *askLog) (*session, *direct, error) {
	mu.Lock()
	lock := locks[key]
	if lock == nil {
		lock = make(chan struct{}, 1)
		locks[key] = lock
	}
	mu.Unlock()
	select {
	case lock <- struct{}{}:
	case <-time.After(20 * time.Second):
		return nil, nil, &refused{status: 504}
	}
	defer func() { <-lock }()
	mu.Lock()
	if s := sessions[key]; s != nil && (renewFrom == "" || s.loc != renewFrom) {
		s.used = mono() // opened (or renewed) by someone else meanwhile
		mu.Unlock()
		return s, nil, nil
	}
	mu.Unlock()
	// A first ask that sends nothing for pieceWait is let go and asked again at once on a new connection, as a
	// player straight at the provider does after its own read timeout (ExoPlayer DefaultHttpDataSource, 8 s).
	try := func(limit time.Duration) (*http.Response, context.CancelFunc, error) {
		ctx, cancel := context.WithCancel(context.Background())
		var timed atomic.Bool
		t := time.AfterFunc(limit, func() { timed.Store(true); cancel() })
		r, err := fetch(ctx, target, h, opts{noFollow: true, wait: playlistWait, ask: a})
		t.Stop()
		if err != nil {
			cancel()
			if timed.Load() {
				err = timeoutError{}
			}
		}
		return r, cancel, err
	}
	r, done, err := try(secs(pieceWait))
	if err != nil && wentQuiet(err) {
		a.fields(lg.Info()).Msgf("a channel's address sent nothing for %d s; asked again on a new connection", pieceWait)
		r, done, err = try(time.Duration(math.MaxInt64))
	}
	if err != nil {
		return nil, nil, err
	}
	defer done()
	if loc := r.Header.Get("Location"); isRedirect(r.StatusCode) && loc != "" {
		r.Body.Close()
		u, err := r.Request.URL.Parse(loc)
		if err != nil {
			return nil, nil, &refused{status: 502}
		}
		if isPrivate(u.Hostname()) || strings.EqualFold(u.Hostname(), "localhost") || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, nil, &refused{status: 403}
		}
		s := &session{loc: u.String(), final: u.String(), used: mono(), opened: mono(), who: map[string]float64{}}
		mu.Lock()
		sessions[key] = s
		mu.Unlock()
		if renewFrom != "" {
			a.fields(lg.Info()).Msg("a live HLS channel was opened again at the provider")
		} else {
			a.fields(lg.Info()).Msg("a live HLS channel was opened at the provider")
		}
		return s, nil, nil
	}
	data, err := readAll(r, 4<<20, playlistWait)
	return nil, &direct{r.StatusCode, r.Header, data}, err
}

// keepDirect: a catch-up playlist the provider answered itself, without sending it elsewhere. It is complete and
// asked for once, so a session of its own counts it at the provider while its pieces are read (see inUse); it is
// kept apart from the channel's key, so the next ask for the playlist goes to the provider as before.
func (f *front) keepDirect(key, target, who string, want what, data []byte) {
	if !bytes.Contains(data[:min(len(data), 1024)], []byte("#EXTM")) {
		return // (not a playlist: nothing it names is read)
	}
	text, err := rewritePlaylist(string(data), target, f)
	if err != nil {
		return
	}
	s := &session{loc: target, final: target, used: mono(), opened: mono(), what: want, who: map[string]float64{},
		pieces: playlistPieces(text), owner: f}
	s.note(data)
	if !isLoop(who) {
		s.who[who] = mono()
	}
	mu.Lock()
	sessions[key+" direct"] = s
	mu.Unlock()
}

// catchUpRead: device who asked for a piece of a catch-up watched as HLS (a session kept by keepDirect). Its
// playlist is complete and asked for once, so the pieces are all that shows it is being watched: they give it a
// row in the status list, and it counts as playing (playingNow), until no piece of it was asked for in 30 s, as
// a live HLS channel does by its playlist requests. Without the row nothing showed it, and what waits while
// anything plays knew of it only where the pieces came typed as video. One log line when the reading starts and
// one when it ends, none per piece. Nobody is asked anything. (mu held)
func (f *front) catchUpRead(who, id string) {
	now, rk := mono(), who+" "+f.base+" "+id+" catch-up"
	if row := hlsRows[rk]; row != nil && now-row.last <= 30 {
		row.last = now
		return
	}
	row := &hlsRow{first: now, last: now, who: who, id: id, provider: f.base, port: f.port, catchUp: true}
	hlsRows[rk] = row
	lg.Info().Str("device", who).Str("channel", id).Str("provider", f.name).
		Msgf("%s started reading catch-up of channel %s as HLS (%s)", who, id, hostOf(f.base))
	go func() { // (looks again when the row would be over, and every few seconds until then)
		for {
			mu.Lock()
			left := row.last + 30 - mono()
			if left < 0 {
				if hlsRows[rk] == row {
					delete(hlsRows, rk)
				}
				sent, took := row.bytes, row.last-row.first
				mu.Unlock()
				lg.Info().Str("device", who).Str("channel", id).Str("provider", f.name).Float64("mb", mb(sent)).
					Float64("seconds", round1(took)).
					Msgf("%s stopped reading catch-up of channel %s as HLS: %.1f MB in %s", who, id, mb(sent), hms(took))
				return
			}
			mu.Unlock()
			time.Sleep(secs(math.Min(left, 2) + 0.01))
		}
	}()
}

func playlistRequest(w http.ResponseWriter, r *http.Request, rt *route) error {
	provider, uri, orig, port := rt.f.base, rt.uri, rt.orig, rt.f.port
	mu.Lock()
	forgetIdle()
	mu.Unlock()
	h := http.Header{}
	for k, vs := range r.Header {
		if !plSkipRe.MatchString(k) {
			h[k] = vs
		}
	}
	rt.f.agent(h)
	opath, oquery, _ := strings.Cut(orig, "?")
	q := ""
	if oquery != "" {
		q = "?" + oquery
	}
	ctx := r.Context()
	// (a part of a film's HLS: another quality, or its audio)
	film := rt.f.filmSpeed(rt.client, r, what{})
	if m := cdnRe.FindStringSubmatch(opath); m != nil { // a playlist on a server the provider sent the player to
		mu.Lock()
		rt.f.inUse(orig, rt.client)
		mu.Unlock()
		if m[3] == "" {
			m[3] = "/"
		}
		mu.Lock()
		vs := rt.f.variant(strings.ToLower(m[1])+"://"+m[2]+m[3], rt.client)
		mu.Unlock()
		status, hd, data, final, err := livePlaylist(ctx, strings.ToLower(m[1])+"://"+m[2]+m[3]+q, h, playlistWait,
			&askLog{kind: "playlist", device: rt.client, provider: rt.f.name, routine: true})
		if err != nil {
			return err
		}
		if vs != nil && status == 200 { // (the variant's pieces set how often it is asked for)
			mu.Lock()
			vs.note(data)
			mu.Unlock()
		}
		// A catch-up's playlist, at the address the provider sent the player on to (catchUpPlaylist): it is
		// complete and asked for once, so a session of its own counts it at the provider while its pieces are
		// read (see inUse), as for one the provider answers itself (keepDirect). Without it nothing counted the
		// catch-up once the place kept for this follow-up was over: another device could open the channel.
		if cu, follow := rt.f.followed(r); follow && cu.kind == "ch" && vs == nil && status == 200 {
			rt.f.keepDirect(provider+" "+orig, final, rt.client, cu, data)
			mu.Lock()
			rt.f.keepFor(rt.client, cu, 0) // (the place kept for it is taken)
			mu.Unlock()
		}
		return servePlaylist(w, status, hd, data, final, rt.f, film, r)
	}
	if rt.f.mode == "ts" { // a ts provider: live HLS is built from the shared MPEG-TS stream
		if done, err := hlsFromTS(w, r, rt.client, provider, uri, opath, q, port, rt.f); done {
			return err
		}
	}
	if !chRe.MatchString(opath) && rt.f.catchUpOf(&url.URL{Path: opath, RawQuery: oquery}) != "" {
		return catchUpPlaylist(w, r, rt, h, opath, oquery, film)
	}
	m := chRe.FindStringSubmatch(opath) // a channel's own folder: /__ch/<channel>/<__index.m3u8 or a piece>
	chanOrig := opath
	if m != nil {
		chanOrig = m[1]
	}
	plainURI := strings.SplitN(uri, "?", 2)[0]
	if p := strings.FieldsFunc(plainURI, func(r rune) bool { return r == '/' }); len(p) == 3 && strings.HasSuffix(p[2], ".m3u8") {
		plainURI = "/live/" + strings.Join(p, "/") // the same channel as its /live/ form: one provider session
	}
	key, target := provider+" "+plainURI, provider+uri
	if m != nil && oquery != "" && !strings.Contains(uri, "?") {
		target += q
	}
	who := rt.client
	want := rt.f.classify(&url.URL{Path: chanOrig, RawQuery: oquery})
	asked := rt.f.askFor(&url.URL{Path: chanOrig, RawQuery: oquery}, who)
	if asked.kind == "other" {
		asked.kind = "playlist"
	}
	if film == nil {
		film = rt.f.filmSpeed(who, r, want)
	}
	mu.Lock()
	s := sessions[key]
	back := s != nil && mono()-s.used > s.held() // (it no longer counts: its connection may be someone else's now)
	// (a variant's own playlist, named in the playlist of a session that is open: that session goes on)
	var vs *session
	if s == nil && want.kind == "" {
		vs = rt.f.variant(provider+strings.SplitN(uri, "?", 2)[0], who)
	}
	of := vs != nil
	mu.Unlock()
	if s == nil || back {
		// A new session at the provider: by the rules it has on (advanced.go). Others asking for the same
		// channel meanwhile share it. (The relay's own bridge was let through when its reader came.)
		if !of {
			hd, why := rt.f.admit(ctx, who, want, false, key, nil)
			if hd == nil {
				if why == "" {
					return nil // (the player hung up while it waited)
				}
				return rt.f.refusal(who, want, why)
			}
			defer hd.release()
		}
		var d *direct
		var err error
		mu.Lock()
		if s != nil && sessions[key] != s { // (let go of while this waited)
			s = nil
		}
		mu.Unlock()
		if s == nil {
			if s, d, err = openChannel(key, target, h, "", asked); err != nil {
				return err
			} else if d != nil {
				if d.status == 200 && of {
					mu.Lock()
					vs.note(d.data)
					mu.Unlock()
				} else if d.status == 200 && rt.f.catchUpOf(&url.URL{Path: chanOrig, RawQuery: oquery}) != "" {
					rt.f.keepDirect(key, target, who, want, d.data)
				}
				return servePlaylist(w, d.status, d.h, d.data, target, rt.f, film, r)
			}
		}
	}
	mu.Lock()
	s.used = mono()
	if s.what.kind == "" {
		s.what = want
	}
	if s.owner == nil {
		s.owner = rt.f
	}
	if s.who == nil {
		s.who = map[string]float64{}
	}
	if !isLoop(who) {
		s.who[who] = mono()
	}
	if rk := who + " " + provider + " " + channelID(uri); who != "127.0.0.1" && who != "::1" { // (not the relay's own bridge)
		if row := hlsRows[rk]; row != nil && mono()-row.last <= 30 {
			row.last = mono()
		} else {
			hlsRows[rk] = &hlsRow{first: mono(), last: mono(), who: who, id: channelID(uri), provider: provider, port: port}
		}
	}
	t, said := hlsWho[who+" "+key]
	hlsWho[who+" "+key] = mono()
	mu.Unlock()
	if m == nil { // the player opened the channel: give it the channel's folder
		if !said || mono()-t > 60 { // (once a minute: some players ask the channel's own address at each refresh)
			lg.Info().Str("device", who).Str("channel", channelID(uri)).Str("provider", rt.f.name).
				Msgf("%s opened HLS channel %s (%s)", who, channelID(uri), hostOf(provider))
		}
		w.Header().Set("Location", "/__ch"+opath+"/__index.m3u8"+q)
		answer(w, 302, "", "")
		return nil
	}
	if m[2] != "__index.m3u8" { // a piece named next to the playlist: where it really is
		mu.Lock()
		final := s.final
		mu.Unlock()
		b, _ := url.Parse(final)
		u, err := b.Parse(m[2] + q)
		if err != nil || cdnAddress(u) == "" {
			answer(w, 404, "", "")
			return nil
		}
		to := cdnAddress(u)
		if rt.f.ownServer(u) { // (on the provider's own server: with the easy login, as in the playlist)
			if p, ok := rt.f.easyForm(u); ok {
				to = p
			}
		}
		w.Header().Set("Location", to)
		answer(w, 302, "", "")
		return nil
	}
	var status int
	var hd http.Header
	var data []byte
	var final string
	var was *session // the session this one replaced, when it was opened again (for the log line below)
	for attempt := 1; attempt <= 2; attempt++ {
		mu.Lock()
		loc := s.loc
		mu.Unlock()
		reload := *asked
		reload.kind, reload.routine = asked.kind+" playlist", true
		st, hdr, text, fin, err := livePlaylist(ctx, loc, h, 25, &reload)
		if err == nil {
			status, hd, data, final = st, hdr, text, fin
		}
		if err != nil {
			if attempt == 2 || ctx.Err() != nil { // (the player hung up: nothing failed at the provider)
				return err
			}
			var rf *refused
			status, hd, data, final = 502, nil, nil, loc
			if errors.As(err, &rf) {
				status = rf.status
			}
		}
		if !renewOn[status] || attempt == 2 {
			break
		}
		mu.Lock()
		refusedNow := mono()-s.refusedAt < renewGap
		mu.Unlock()
		if refusedNow { // (opened again a moment ago and refused: its own answer, as a player straight at it sees)
			if err != nil {
				return err
			}
			break
		}
		ns, d, err := openChannel(key, target, h, loc, asked) // the remembered session is gone
		if err != nil {
			return err
		} else if d != nil {
			if d.status >= 400 {
				mu.Lock()
				s.refusedAt = mono()
				mu.Unlock()
			}
			return servePlaylist(w, d.status, d.h, d.data, target, rt.f, film, r)
		}
		mu.Lock()
		if ns != s { // (opened afresh: still the same stream, and the same devices on it)
			ns.what = s.what
			for d, t := range s.who {
				ns.who[d] = t
			}
		}
		mu.Unlock()
		if ns != s {
			was = s
		}
		s = ns
	}
	var pieces map[string]bool
	if text, err := rewritePlaylist(string(data), final, rt.f); err == nil && status == 200 {
		pieces = playlistPieces(text)
	}
	mu.Lock()
	s.final = final
	if status == 200 {
		s.pieces = pieces
		s.note(data)
		if was != nil && was.hasSeq && s.hasSeq { // does the new session carry on the numbering players have?
			ev := lg.Info()
			if s.seq < was.seq || s.seq > was.seq+64 {
				ev = lg.Warn()
			}
			ev.Str("channel", channelID(uri)).Str("device", who).Int64("from", was.seq).Int64("to", s.seq).
				Bool("same_server", hostOf(was.final) == hostOf(final)).
				Msgf("a live HLS channel opened again at the provider: its playlist numbering went from %d to %d", was.seq, s.seq)
		}
		hlsSeen[port+chanOrig] = mono()
		if len(hlsSeen) > 300 {
			for k, t := range hlsSeen {
				if mono()-t > 30 {
					delete(hlsSeen, k)
				}
			}
		}
	}
	mu.Unlock()
	return servePlaylist(w, status, hd, data, final, rt.f, film, r)
}

// ---------------------------------------------------------------------------
// Where a /__cdn/ server name really leads (asked before the pass-through connects); kept a minute
// ---------------------------------------------------------------------------
type checked struct {
	at float64
	ok bool
}

var checks = map[string]checked{}

// nameOK: false when the name leads to this machine or a home network.
func nameOK(ctx context.Context, host string) bool {
	host, now := strings.ToLower(host), mono()
	mu.Lock()
	c, have := checks[host]
	mu.Unlock()
	if !have || now-c.at >= 60 {
		home, _ := leadsHome(ctx, host) // no answer: the connection's own lookup fails the same way
		c = checked{now, !home}
		mu.Lock()
		if len(checks) > 2000 {
			checks = map[string]checked{}
		}
		checks[host] = c
		mu.Unlock()
	}
	return c.ok
}

// ---------------------------------------------------------------------------
// player_api.php lists, every "http(s)://... (also JSON's \/ form) pointed back here, piece by piece
// ---------------------------------------------------------------------------
var (
	apiLink  = regexp.MustCompile(`"(https?):(?:\\/\\/|//)`)
	hostKeep = regexp.MustCompile(`[^A-Za-z0-9.:\[\]-]`)
)

func api(w http.ResponseWriter, r *http.Request, rt *route) error {
	provider, uri := rt.f.base, rt.uri
	// A login or a list asked for with POST (the pairs in a form body, which Xtream panels read exactly as the
	// query): the pairs go on in the query, with the login swapped there as for any other ask.
	if r.Method == "POST" && strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/x-www-form-urlencoded") {
		form, err := io.ReadAll(io.LimitReader(r.Body, 65536))
		if err != nil {
			return &refused{status: 400}
		}
		if f := strings.TrimSpace(string(form)); f != "" {
			if r.URL.RawQuery != "" {
				f = r.URL.RawQuery + "&" + f
			}
			r.URL.RawQuery = f
			uri, _ = rt.f.swap(r.URL)
		}
	}
	home := hostKeep.ReplaceAllString(r.Host, "")
	if home == "" {
		answer(w, 400, "", "")
		return nil
	}
	h := http.Header{}
	for k, vs := range r.Header {
		if !skipRe.MatchString(k) {
			h[k] = vs
		}
	}
	rt.f.agent(h)
	var resp *http.Response
	var err error
	if key := shareKey(rt.f, r); key != "" { // the same download shared by every player for a while (share.go)
		resp, err = shared(r.Context(), rt.f, key, provider+uri, h, rt.f.askFor(r.URL, rt.client))
	} else {
		resp, err = fetch(r.Context(), provider+uri, h, opts{guardAll: true, wait: 300, ask: rt.f.askFor(r.URL, rt.client)})
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := io.Reader(resp.Body)
	switch ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); ce {
	case "", "identity":
	case "gzip", "x-gzip": // asked for plain text, sent gzip anyway (some providers do, for their guide): unpacked
		z, err := gzip.NewReader(resp.Body)
		if err != nil {
			return &refused{status: 502}
		}
		defer z.Close()
		body = z
	default:
		return &refused{status: 502} // a compression this cannot read
	}
	if resp.StatusCode >= 400 { // the provider's own error page (see hideLogin)
		page, err := io.ReadAll(io.LimitReader(body, 65536))
		if err != nil {
			return &refused{status: 502}
		}
		body = io.MultiReader(bytes.NewReader(rt.f.hideLogin(page)), body)
	}
	if q, _ := url.ParseQuery(r.URL.RawQuery); q.Get("action") == "" && strings.HasSuffix(strings.ToLower(r.URL.Path), "_api.php") {
		// (the login itself: player_api.php or panel_api.php with no action)
		if body, err = playerServer(body, home); err != nil { // (body: unpacked, if it came packed)
			return err
		}
	}
	for k, vs := range resp.Header {
		if !plSkipRe.MatchString(k) {
			w.Header()[k] = vs
		}
	}
	// (packed only when there is a body to pack: a 304 or 204 has none, and a gzip header written into one
	// broke the answer off)
	gz := strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") && resp.StatusCode != 304 && resp.StatusCode != 204
	if gz {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
	}
	w.WriteHeader(resp.StatusCode)
	// The answer to the login itself names the account: "username" and "password" in user_info are the
	// provider's. The player is given its own (easy) login there instead, as everywhere else.
	login := [][2]string{{"username", rt.f.user}, {"password", rt.f.pass}}
	for i, l := range login {
		easy := map[bool]string{true: rt.f.pu, false: rt.f.pp}[i == 0]
		login[i] = [2]string{`"` + l[0] + `"\s{0,4}:\s{0,4}"` + jsonForms(l[1]) + `"`, `"` + l[0] + `":"` + easy + `"`}
	}
	if err := apiStream(w, body, []byte(`"http://`+home+"/__cdn/"), login, gz); err != nil {
		panic(http.ErrAbortHandler) // cut off: the app must see a broken answer, not a short complete one
	}
	return nil
}

// jsonForms: a pattern for s as JSON writes it between quotes: \ and " escaped, and each character past ASCII
// as it is or as \uXXXX (PHP's json_encode and Python's json write them so by default). The pattern is never
// shorter than what it matches (see apiStream).
func jsonForms(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '"':
			b.WriteString(regexp.QuoteMeta(`\` + string(r)))
		case r < 0x80:
			b.WriteString(regexp.QuoteMeta(string(r)))
		default:
			b.WriteString("(?:" + regexp.QuoteMeta(string(r)) + "|")
			for _, u := range utf16.Encode([]rune{r}) {
				b.WriteString(fmt.Sprintf(`\\u(?i:%04x)`, u))
			}
			b.WriteString(")")
		}
	}
	return b.String()
}

// playerServer: the answer to the login with server_info naming the address the player used (home), over
// http, instead of the provider's server: apps build stream addresses from it, and some warn when it differs
// from what was typed in. The answer is read up to the end of server_info; the rest follows as it comes.
func playerServer(body io.Reader, home string) (io.Reader, error) {
	host, port, err := net.SplitHostPort(home)
	if err != nil {
		host, port = home, "80" // (no port written: the player came in on http's own)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	key := []byte(`"server_info"`)
	var data []byte
	from, buf := 0, make([]byte, 65536)
	for done := false; ; {
		if i := bytes.Index(data[from:], key); i >= 0 {
			at := from + i + len(key)
			rest := bytes.TrimLeft(data[at:], " 	\r\n")
			if len(rest) > 0 && rest[0] == ':' {
				at = len(data) - len(rest) + 1
				dec := json.NewDecoder(bytes.NewReader(data[at:]))
				var info map[string]json.RawMessage
				switch err := dec.Decode(&info); {
				case err == nil && info != nil:
					for k, v := range map[string]string{"url": host, "port": port, "https_port": port, "server_protocol": "http"} {
						if old := bytes.TrimSpace(info[k]); k != "url" && k != "server_protocol" && len(old) > 0 && old[0] != '"' {
							info[k] = json.RawMessage(v) // a number stays a number
						} else {
							info[k], _ = json.Marshal(v)
						}
					}
					text, _ := json.Marshal(info)
					end := at + int(dec.InputOffset())
					head := append(append(append([]byte{}, data[:at]...), text...), data[end:]...)
					return io.MultiReader(bytes.NewReader(head), body), nil
				case err != io.EOF && err != io.ErrUnexpectedEOF: // not an object: it names no server
					return io.MultiReader(bytes.NewReader(data), body), nil
				}
			} else if len(rest) > 0 {
				from = at // only the words: look further on
			}
		} else {
			from = max(0, len(data)-len(key))
		}
		if done {
			return bytes.NewReader(data), nil
		}
		n, err := body.Read(buf)
		data = append(data, buf[:n]...)
		if err == io.EOF {
			done = true
		} else if err != nil {
			return nil, err
		} else if len(data) > 64<<20 {
			return nil, &refused{status: 502}
		}
	}
}

// apiStream passes the provider's list on with every link pointed back here, and each pattern in login
// (a regular expression that starts with a quote and matches no longer a text than itself, and what replaces it)
// replaced.
func apiStream(w io.Writer, body io.Reader, link []byte, login [][2]string, gz bool) error {
	repl := append(append([]byte{}, link...), "${1}/"...)
	find, keep, swaps := apiLink, 12, []*regexp.Regexp{}
	if len(login) > 0 {
		alts := []string{apiLink.String()}
		for _, l := range login {
			alts = append(alts, l[0])
			swaps = append(swaps, regexp.MustCompile(l[0]))
			keep = max(keep, len(l[0])) // (a pattern written out is never shorter than what it matches)
		}
		find = regexp.MustCompile(strings.Join(alts, "|"))
	}
	change := func(b []byte) []byte {
		b = apiLink.ReplaceAll(b, repl)
		for i, re := range swaps {
			if pre, _ := re.LiteralPrefix(); bytes.Contains(b, []byte(pre)) { // (most lists hold no login at all)
				b = re.ReplaceAllLiteral(b, []byte(login[i][1]))
			}
		}
		return b
	}
	out := w
	var z *gzip.Writer
	if gz {
		z, _ = gzip.NewWriterLevel(w, 1)
		out = z
	}
	var tail []byte
	buf := make([]byte, 65536)
	for {
		n, err := body.Read(buf)
		data := append(tail, buf[:n]...)
		tail = nil
		// what may be the start of a link or a login is held back until the next piece: the last keep bytes,
		// or up to the end of a match that runs into them
		if cut := len(data) - keep; err == nil && cut > 0 {
			from := max(0, cut-keep) // (a match that runs past cut starts after this)
			for _, m := range find.FindAllIndex(data[from:], -1) {
				if from+m[0] < cut && from+m[1] > cut {
					cut = from + m[1]
				}
			}
			data, tail = data[:cut], append([]byte{}, data[cut:]...)
		} else if err == nil {
			data, tail = nil, data
		}
		if _, werr := out.Write(change(data)); werr != nil {
			return werr
		}
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
	}
	if z != nil {
		return z.Close()
	}
	return nil
}

// catchUpPlaylist: a catch-up's HLS playlist, asked for by a player. It is a finished recording, not a live
// channel, so it is treated as the catch-up's MPEG-TS is (stream in front.go): asked for at the provider every
// time, by the rules the provider has on, and the provider's redirect passed on pointed back at this proxy,
// just as a player straight at the provider would be sent there. No session is made or kept for it.
// The playlist it leads to comes back through /__cdn/ (its pieces pointed back here, as for any playlist).
func catchUpPlaylist(w http.ResponseWriter, r *http.Request, rt *route, h http.Header, opath, oquery string, film *speed) error {
	f, who := rt.f, rt.client
	u := &url.URL{Path: opath, RawQuery: oquery}
	want := f.classify(u)
	hd, why := f.admit(r.Context(), who, want, false, "", nil, r.URL.RequestURI())
	if hd == nil {
		if why == "" {
			return nil // (the player hung up while it waited)
		}
		return f.refusal(who, want, why)
	}
	defer hd.release()
	target := f.base + rt.uri
	resp, err := fetch(r.Context(), target, h, opts{noFollow: true, wait: playlistWait, ask: f.askFor(u, who)})
	if err != nil {
		return err
	}
	if loc := resp.Header.Get("Location"); isRedirect(resp.StatusCode) && loc != "" {
		resp.Body.Close()
		to, err := resp.Request.URL.Parse(loc)
		if err != nil {
			return &refused{status: 502}
		}
		moved := rewriteLocation(to.String(), "", "")
		if f.ownServer(to) { // back to the provider's own server: with the easy login (never the real one)
			if p, ok := f.easyForm(to); ok {
				moved = p
			}
		}
		mu.Lock()
		f.remember(moved, hd) // the player comes back for it: it carries on what was admitted
		mu.Unlock()
		dropHop(resp.Header)
		for k, vs := range resp.Header {
			w.Header()[k] = vs
		}
		w.Header().Del("Content-Length")
		w.Header().Set("Location", moved)
		w.WriteHeader(resp.StatusCode)
		return nil
	}
	data, err := readAll(resp, 4<<20, playlistWait)
	if err != nil {
		return err
	}
	if resp.StatusCode == 200 { // answered here, not sent elsewhere: counted while its pieces are read
		f.keepDirect(f.base+" "+strings.SplitN(rt.uri, "?", 2)[0], target, who, want, data)
	}
	return servePlaylist(w, resp.StatusCode, resp.Header, data, target, f, film, r)
}
