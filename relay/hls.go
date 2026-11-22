// HLS: the /hls bridge's source, playlists with one provider session per live channel, the provider's
// lists (/api) and the /__cdn/ name check (/check).
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
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

// beforeMinute: seconds until just before the next whole minute; 0 from 1 s before it to 0.3 s after.
// Recorders stop at whole minutes and cut at once, so around then nothing is held back.
func beforeMinute() float64 {
	s := math.Mod(float64(time.Now().UnixNano())/1e9, 60)
	if s >= 59 || s < 0.3 {
		return 0
	}
	return 59 - s
}

// hlsSource: the channel's HLS pieces end to end, fetched from this stack's proxy at the players' own
// addresses so the reader shares their one provider session; handed on at playing speed.
func hlsSource(start, agent string) source {
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
			status, data, err := get(ctx, at, h, 2<<20)
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
		// Video waiting, (due, bytes), oldest first: each piece goes out over its playing time, everything
		// at once before each whole minute and when the feed ends, so a recording that stops loses nothing.
		var queue []item
		pop := func() []byte { d := queue[0].data; queue = queue[1:]; return d }
		due := func() bool {
			now, flush := mono(), beforeMinute() == 0
			for len(queue) > 0 && (flush || queue[0].t <= now) {
				if !yield(pop()) {
					return false
				}
			}
			return true
		}
		flushAll := func() bool {
			for len(queue) > 0 {
				if !yield(pop()) {
					return false
				}
			}
			return true
		}
		// wait until `until`, or until job is done, handing on what falls due meanwhile (a fetch can take
		// seconds; a whole minute passing meanwhile must not hold video back).
		wait := func(until float64, job chan struct{}) error {
			for {
				if !due() {
					return errStop
				}
				nap := 3600.0
				if job == nil {
					if nap = until - mono(); nap <= 0 {
						return nil
					}
				}
				select {
				case <-job:
					return nil
				default:
				}
				if len(queue) > 0 {
					nap = math.Min(nap, math.Min(queue[0].t-mono(), beforeMinute()))
				}
				t := time.NewTimer(secs(math.Max(0, nap)))
				select {
				case <-job:
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
					return ctx.Err()
				}
				t.Stop()
			}
		}
		do := func(f func(context.Context)) error { // a request under way; called off if the feed ends
			jctx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan struct{})
			go func() { f(jctx); close(done) }()
			return wait(0, done)
		}
		fail := func(err error) error { // the feed ends with a fault: what waits is handed on first
			if !flushAll() {
				return errStop
			}
			return err
		}
		mu.Lock()
		ch.paced = true // its own pauses are not the provider's
		mu.Unlock()
		var last int64
		hasLast, misses, lost, got, fresh := false, 0, 0, 0.0, mono()
		var sent []string // names of the newest 60 pieces
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
					return fail(&refused{status: status})
				}
				nap := 1.0
				if hasLast {
					nap = 2
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
			newest := int64(len(pl.pieces)) - 1 + pl.seq
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
					return fail(err)
				}
				if afresh && contains(sent, u.Path) { // already passed on under its old number
					continue
				}
				var data []byte
				for try := 0; try < 2 && data == nil; try++ {
					if e := do(func(c context.Context) {
						if status, d, err := get(c, u.String(), h, 64<<20); err == nil && status == 200 && len(d) > 0 {
							data = d
						}
					}); e != nil {
						return e
					}
				}
				if data == nil {
					lost++
					say("a piece of video could not be fetched and is missing from a stream")
					if lost >= 3 {
						return fail(&refused{status: 502})
					}
					continue
				}
				lost = 0
				if sent = append(sent, u.Path); len(sent) > 60 {
					sent = sent[1:]
				}
				now := mono()
				if got != 0 && now-got > math.Max(4, 1.5*length) { // a piece well later than its length
					say("a provider sent nothing for %.0f s, then carried on", now-got)
				}
				got, fresh = now, now
				var parts [][]byte // whole 188-byte packets
				for j := 0; j < len(data); j += 65424 {
					parts = append(parts, data[j:min(j+65424, len(data))])
				}
				// A channel's start goes at once, except its newest piece, which goes over its playing time
				// so the stream has video in hand from its first second.
				if opening {
					if !flushAll() {
						return errStop
					}
					if number != newest {
						for _, p := range parts {
							if !yield(p) {
								return errStop
							}
						}
						continue
					}
				}
				startAt := now
				if len(queue) > 0 {
					startAt = math.Max(now, queue[len(queue)-1].t)
				}
				if shift := startAt - now - 2*pl.target; shift > 0 { // never more than two target durations waiting
					for k := range queue {
						queue[k].t -= shift
					}
					startAt -= shift
				}
				for k, p := range parts {
					queue = append(queue, item{startAt + length*float64(k)/float64(len(parts)), p})
				}
				if !due() {
					return errStop
				}
			}
			if !isNew && (pl.ended || mono()-fresh > math.Max(30, 6*pl.target)) {
				if !flushAll() {
					return errStop
				}
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
}

type hlsRow struct {
	first, last             float64
	who, id, provider, port string
}

// cdnAddress: this proxy's address for a full http(s) address ("" for anything else).
func cdnAddress(u *url.URL) string {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	host, path := u.Host, u.EscapedPath()
	if u.User != nil {
		host = u.User.String() + "@" + host // (Caddy refuses these)
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

// rewritePlaylist points every address back at this proxy. With home (the player's own path, for a playlist the
// provider answered directly), addresses on the provider's own server resolve against the player's path, so
// they come back with the easy login and the real one is never written into the playlist.
func rewritePlaylist(text, base, home string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	hu, err := url.Parse("http://home.invalid" + home)
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
		if home != "" && full.Scheme == b.Scheme && full.Host == b.Host && !hasScheme.MatchString(ref) {
			mine, _ := hu.Parse(ref)
			p := mine.EscapedPath()
			if p == "" {
				p = "/"
			}
			if mine.RawQuery != "" {
				p += "?" + mine.RawQuery
			}
			return p, nil
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
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	w.Write(data)
}

// servePlaylist rewrites a playlist; anything else is passed on as it came.
func servePlaylist(w http.ResponseWriter, status int, h http.Header, data []byte, base, home string) error {
	if bytes.Contains(data[:min(len(data), 1024)], []byte("#EXTM")) {
		text, err := rewritePlaylist(string(data), base, home)
		if err != nil {
			return err
		}
		data = []byte(text)
	}
	send(w, status, h, data)
	return nil
}

// fetchViaProxy fetches a playlist on a video server through Caddy's /__cdn/ (riding Caddy's kept-open
// connection), after checking where the name leads. Returns the answer and where it really came from.
func fetchViaProxy(ctx context.Context, target string, h http.Header, port string, wait float64) (*http.Response, string, error) {
	for hop := 0; hop < 10; hop++ {
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return nil, "", &refused{status: 403}
		}
		if home, err := leadsHome(ctx, u.Hostname()); err != nil {
			return nil, "", err
		} else if home {
			return nil, "", &refused{status: 403}
		}
		local := "http://127.0.0.1:" + port + cdnAddress(u)
		hh := h.Clone()
		hh.Set("X-Relay-Fetch", "1")
		r, err := fetch(ctx, local, hh, opts{noFollow: true, wait: wait})
		if err != nil || !isRedirect(r.StatusCode) || r.Header.Get("Location") == "" {
			return r, target, err
		}
		r.Body.Close()
		next, err := r.Request.URL.Parse(r.Header.Get("Location"))
		if err != nil {
			return nil, "", &refused{status: 502}
		}
		if target = next.String(); next.Host == "127.0.0.1:"+port {
			target = realAddress(next.RequestURI())
		}
		if target == "" {
			return nil, "", &refused{status: 502}
		}
	}
	return nil, "", &refused{status: 502}
}

func forgetIdle() { // (mu held)
	now := mono()
	for k, s := range sessions {
		if now-s.used > 60 {
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
func openChannel(key, target string, h http.Header, renewFrom string) (*session, *direct, error) {
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
	r, err := fetch(context.Background(), target, h, opts{noFollow: true, wait: playlistWait})
	if err != nil {
		return nil, nil, err
	}
	if loc := r.Header.Get("Location"); isRedirect(r.StatusCode) && loc != "" {
		r.Body.Close()
		u, err := r.Request.URL.Parse(loc)
		if err != nil {
			return nil, nil, &refused{status: 502}
		}
		if isPrivate(u.Hostname()) || strings.EqualFold(u.Hostname(), "localhost") || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, nil, &refused{status: 403}
		}
		s := &session{u.String(), u.String(), mono()}
		mu.Lock()
		sessions[key] = s
		mu.Unlock()
		if renewFrom != "" {
			say("a live HLS channel was opened again at the provider")
		} else {
			say("a live HLS channel was opened at the provider")
		}
		return s, nil, nil
	}
	data, err := readAll(r, 4<<20, playlistWait)
	return nil, &direct{r.StatusCode, r.Header, data}, err
}

func playlistRequest(w http.ResponseWriter, r *http.Request) error {
	provider, uri, orig := strings.TrimRight(r.Header.Get("X-Relay-Provider"), "/"), r.Header.Get("X-Relay-Uri"), r.Header.Get("X-Relay-Orig")
	port := r.Header.Get("X-Relay-Port")
	if _, err := strconv.Atoi(port); !providers[provider] || !strings.HasPrefix(uri, "/") || !strings.HasPrefix(orig, "/") || err != nil {
		answer(w, 403, "", notProxy)
		return nil
	}
	mu.Lock()
	forgetIdle()
	mu.Unlock()
	h := http.Header{}
	for k, vs := range r.Header {
		if !plSkipRe.MatchString(k) {
			h[k] = vs
		}
	}
	opath, oquery, _ := strings.Cut(orig, "?")
	q := ""
	if oquery != "" {
		q = "?" + oquery
	}
	ctx := r.Context()
	if m := cdnRe.FindStringSubmatch(opath); m != nil { // a playlist on a server the provider sent the player to
		if m[3] == "" {
			m[3] = "/"
		}
		resp, final, err := fetchViaProxy(ctx, strings.ToLower(m[1])+"://"+m[2]+m[3]+q, h, port, playlistWait)
		if err != nil {
			return err
		}
		data, err := readAll(resp, 4<<20, playlistWait)
		if err != nil {
			return err
		}
		return servePlaylist(w, resp.StatusCode, resp.Header, data, final, "")
	}
	if r.Header.Get("X-Relay-Mode") == "ts" { // a ts provider: live HLS is built from the shared MPEG-TS stream
		if done, err := hlsFromTS(w, r, provider, uri, opath, q, port); done {
			return err
		}
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
	mu.Lock()
	s := sessions[key]
	mu.Unlock()
	if s == nil {
		var d *direct
		var err error
		if s, d, err = openChannel(key, target, h, ""); err != nil {
			return err
		} else if d != nil {
			return servePlaylist(w, d.status, d.h, d.data, target, chanOrig)
		}
	}
	mu.Lock()
	s.used = mono()
	who := whoIs(r)
	if rk := who + " " + provider + " " + channelID(uri); who != "127.0.0.1" && who != "::1" { // (not the relay's own bridge)
		if row := hlsRows[rk]; row != nil && mono()-row.last <= 30 {
			row.last = mono()
		} else {
			hlsRows[rk] = &hlsRow{mono(), mono(), who, channelID(uri), provider, port}
		}
	}
	t, said := hlsWho[who+" "+key]
	hlsWho[who+" "+key] = mono()
	mu.Unlock()
	if m == nil { // the player opened the channel: give it the channel's folder
		if !said || mono()-t > 60 { // (once a minute: some players ask the channel's own address at each refresh)
			say("%s opened HLS channel %s (%s)", who, channelID(uri), hostOf(provider))
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
		w.Header().Set("Location", cdnAddress(u))
		answer(w, 302, "", "")
		return nil
	}
	var status int
	var hd http.Header
	var data []byte
	var final string
	for attempt := 1; attempt <= 2; attempt++ {
		mu.Lock()
		loc := s.loc
		mu.Unlock()
		resp, fin, err := fetchViaProxy(ctx, loc, h, port, 25)
		if err == nil {
			data, err = readAll(resp, 4<<20, 25)
			status, hd, final = resp.StatusCode, resp.Header, fin
		}
		if err != nil {
			if attempt == 2 {
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
		ns, d, err := openChannel(key, target, h, loc) // the remembered session is gone
		if err != nil {
			return err
		} else if d != nil {
			return servePlaylist(w, d.status, d.h, d.data, target, chanOrig)
		}
		s = ns
	}
	mu.Lock()
	s.final = final
	if status == 200 {
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
	return servePlaylist(w, status, hd, data, final, "")
}

// ---------------------------------------------------------------------------
// /check: where a /__cdn/ server name really leads (Caddy asks before it connects); kept a minute
// ---------------------------------------------------------------------------
type checked struct {
	at float64
	ok bool
}

var checks = map[string]checked{}

func check(w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse("//" + r.Header.Get("X-Relay-Check"))
	if err != nil || u.Hostname() == "" {
		answer(w, 403, "", "No server named.\n")
		return
	}
	host, now := strings.ToLower(u.Hostname()), mono()
	mu.Lock()
	c, have := checks[host]
	mu.Unlock()
	if !have || now-c.at >= 60 {
		home, _ := leadsHome(r.Context(), host) // no answer: Caddy's own lookup fails the same way
		c = checked{now, !home}
		mu.Lock()
		if len(checks) > 2000 {
			checks = map[string]checked{}
		}
		checks[host] = c
		mu.Unlock()
	}
	if c.ok {
		answer(w, 200, "", "ok\n")
	} else {
		answer(w, 403, "", "That address leads to this machine or a home network.\n")
	}
}

// ---------------------------------------------------------------------------
// /api: player_api.php lists, every "http(s)://... (also JSON's \/ form) pointed back here, piece by piece
// ---------------------------------------------------------------------------
var (
	apiLink  = regexp.MustCompile(`"(https?):(?:\\/\\/|//)`)
	hostKeep = regexp.MustCompile(`[^A-Za-z0-9.:\[\]-]`)
)

func api(w http.ResponseWriter, r *http.Request) error {
	provider, uri := strings.TrimRight(r.Header.Get("X-Relay-Provider"), "/"), r.Header.Get("X-Relay-Uri")
	home := hostKeep.ReplaceAllString(r.Header.Get("X-Relay-Host"), "")
	if !providers[provider] || !strings.HasPrefix(uri, "/") || home == "" {
		answer(w, 403, "", notProxy)
		return nil
	}
	h := http.Header{}
	for k, vs := range r.Header {
		if !skipRe.MatchString(k) {
			h[k] = vs
		}
	}
	resp, err := fetch(r.Context(), provider+uri, h, opts{guardAll: true, wait: 300})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); ce != "" && ce != "identity" {
		return &refused{status: 502} // asked for plain text; a compressed answer cannot be read
	}
	for k, vs := range resp.Header {
		if !plSkipRe.MatchString(k) {
			w.Header()[k] = vs
		}
	}
	gz := strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip")
	if gz {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
	}
	w.WriteHeader(resp.StatusCode)
	if err := apiStream(w, resp.Body, []byte(`"http://`+home+"/__cdn/"), gz); err != nil {
		panic(http.ErrAbortHandler) // cut off: the app must see a broken answer, not a short complete one
	}
	return nil
}

func apiStream(w io.Writer, body io.Reader, link []byte, gz bool) error {
	repl := append(append([]byte{}, link...), "${1}/"...)
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
		// every link starts with a quote: hold back a quote near the end until the next piece
		if cut := bytes.LastIndexByte(data, '"'); err == nil && cut >= 0 && len(data)-cut < 12 {
			data, tail = data[:cut], append([]byte{}, data[cut:]...)
		}
		if _, werr := out.Write(apiLink.ReplaceAll(data, repl)); werr != nil {
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
