// The front door: one listener per provider. It does what the reverse proxy in front of the relay used to
// do, in the same order: the easy login swapped for the provider's, redirects kept inside the tunnel
// (/__cdn/), the provider's ready-made playlist refused, and everything else passed on untouched. Live
// MPEG-TS, playlists, the provider's lists and the status page are handed to the relay as function calls.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// front: one provider and its port.
type front struct {
	name, port, mode       string // mode: "ts" or "hls" (the live_tv setting)
	base, scheme, hostport string // the provider's address without a final slash, and its two halves
	user, pass             string // the provider's login as issued
	encU, encP             string // the same, percent-encoded for an address
	pu, pp                 string // the easy login the players use

	// (loginAny: the login deeper in an address, as a provider's own playlist may write it: /hls/<login>/<piece>)
	loginPath, loginShort, loginAny, chLogin, liveTS *regexp.Regexp
	catchUp                                          *regexp.Regexp // catch-up of a live channel: its number
	liveHLS, vodPath                                 *regexp.Regexp // a live channel as HLS; a movie or an episode

	// the advanced options (advanced.go)
	limitOn             bool
	bought              int // the connections the user bought, as typed in (0: none typed in)
	oneChannel, oneItem bool
	locked              bool
	lockWait            float64 // seconds
	app                 string  // the User-Agent sent to the provider ("": each player's own)
	shareFor            float64 // seconds a list or the guide is shared (0: not shared)
	movieRate           float64 // bytes a second for a movie or an episode (0: as fast as it comes)

}

// route: what the front door worked out about one request, for the relay. Nothing in it comes from a header.
type route struct {
	f      *front
	uri    string // the address at the provider, login swapped
	orig   string // the address as the player asked for it
	client string // the player's address, from its connection
}

var (
	fronts     = map[string]*front{} // by port
	startNotes any                   // what was found in the settings at this start, for the status page

	chAny      = regexp.MustCompile(`^/__ch(/.+?\.m3u8)/`)
	playlistRe = regexp.MustCompile(`(?i)(\.m3u8$|^/__ch/)`)
	// A /__cdn/ address that names this machine or a home network in any disguise: refused before anything
	// is looked up. (The name itself is looked up after this, and the address dialed is checked again.)
	cdnForbidden = regexp.MustCompile(`(?i)^/__cdn/https?/(localhost|127\.|10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|169\.254\.|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.|0\.|([0-9]+\.)*0[0-9]+(\.[0-9]+)*([:/?]|$)|\[|[^/]*[@%])`)
	cdnPath      = regexp.MustCompile(`^/__cdn/(https?)/([^/?]+)(?:/(.*))?$`)
	partRe       = regexp.MustCompile(`^bytes (\d+)-(\d+)/`) // a part's place in the whole (Content-Range)
)

// encLogin percent-encodes a login for an address: letters, digits and - . _ ~ stay, the rest becomes %XX.
func encLogin(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

func newFront(p provider, pl player) *front {
	base := strings.TrimRight(p.URL, "/")
	scheme, hostport, _ := strings.Cut(base, "://")
	pu, pp := regexp.QuoteMeta(pl.User), regexp.QuoteMeta(pl.Password)
	f := &front{name: p.Name, port: p.Port.String(), mode: p.LiveTV, base: base, scheme: strings.ToLower(scheme), hostport: hostport,
		user: p.User, pass: p.Password, encU: encLogin(p.User), encP: encLogin(p.Password), pu: pl.User, pp: pl.Password,
		loginPath:  regexp.MustCompile(`^/+(live|movie|series|timeshift)/+` + pu + `/+` + pp + `/+(.*)$`),
		loginShort: regexp.MustCompile(`^/+` + pu + `/+` + pp + `/+([^/]+)$`),
		loginAny:   regexp.MustCompile(`^(/+[^_/][^/]*(?:/+[^/]+)*?)/+` + pu + `/+` + pp + `/+(.+)$`),
		chLogin:    regexp.MustCompile(`^/__ch(/+(?:(?:live|movie|series|timeshift)/+)?)` + pu + `/+` + pp + `/+(.+?\.m3u8)/`),
		// (live channels are numbered: anything else under /live/, such as a piece a provider's playlist
		// names next to itself, is passed on to the provider, not opened as a channel)
		liveTS: regexp.MustCompile(`^/+(live/+` + pu + `/+` + pp + `/+[0-9]+\.ts|` + pu + `/+` + pp + `/+[0-9]+(\.ts)?)$`),
		// /timeshift/<login>/<minutes>/<start>/<channel>.ts (or .m3u8)
		catchUp: regexp.MustCompile(`^/+timeshift/+` + pu + `/+` + pp + `/+[^/]+/+[^/]+/+([0-9]+)(\.[A-Za-z0-9]+)?$`),
		liveHLS: regexp.MustCompile(`^/+(live/+)?` + pu + `/+` + pp + `/+[0-9]+\.m3u8$`),
		// /movie/<login>/<id>.<ext> and /series/<login>/<id>.<ext>
		vodPath: regexp.MustCompile(`^/+(movie|series)/+` + pu + `/+` + pp + `/+([^/]+?)(\.[A-Za-z0-9]+)?$`)}
	f.applyAdvanced(p.Advanced)
	if n, ok := p.Connections.or(0, 1, 999); ok {
		f.bought = n
	}
	return f
}

// catchUpOf: the channel a catch-up request is for ("" if it is not one), in either form Xtream servers take:
// /timeshift/<login>/<minutes>/<start>/<channel>.ts, or /streaming/timeshift.php?...&stream=<channel>.
func (f *front) catchUpOf(u *url.URL) string {
	if m := f.catchUp.FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	if strings.EqualFold(cleanPath(u.Path), "/streaming/timeshift.php") {
		q := u.Query()
		if id := q.Get("stream"); q.Get("username") == f.pu && q.Get("password") == f.pp && id != "" && strings.Trim(id, "0123456789") == "" {
			return id
		}
	}
	return ""
}

func escPath(s string) string { return (&url.URL{Path: s}).EscapedPath() }

// cleanPath: the path with doubled slashes and dot parts resolved, so that an address written in a
// roundabout way is judged as the server it reaches would read it.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c
}

// swapQuery: username=<easy user> and password=<easy password> become the provider's, where they stand.
func (f *front) swapQuery(raw string) string {
	if raw == "" {
		return raw
	}
	pairs := strings.Split(raw, "&")
	for i, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		key, err1 := url.QueryUnescape(k)
		val, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			continue
		}
		if key == "username" && val == f.pu {
			pairs[i] = k + "=" + url.QueryEscape(f.user)
		} else if key == "password" && val == f.pp {
			pairs[i] = k + "=" + url.QueryEscape(f.pass)
		}
	}
	return strings.Join(pairs, "&")
}

// swap: the request's address at the provider. A request that logs in with the easy login is sent on with
// this provider's account; any other login is passed on untouched (and refused by the provider). Returns
// the address for a pass-through (up) and the one the relay is given (uri: inside a live channel's own
// folder, that is the channel's address).
func (f *front) swap(u *url.URL) (up, uri string) {
	p, raw := u.Path, u.EscapedPath()
	if m := f.loginPath.FindStringSubmatch(p); m != nil {
		raw = "/" + m[1] + "/" + f.encU + "/" + f.encP + "/" + escPath(m[2])
	} else if m := f.loginShort.FindStringSubmatch(p); m != nil {
		raw = "/" + f.encU + "/" + f.encP + "/" + escPath(m[1])
	} else if m := f.loginAny.FindStringSubmatch(p); m != nil {
		raw = escPath(m[1]) + "/" + f.encU + "/" + f.encP + "/" + escPath(m[2])
	}
	up = raw
	if q := f.swapQuery(u.RawQuery); q != "" {
		up += "?" + q
	}
	uri = up
	if m := chAny.FindStringSubmatch(p); m != nil {
		uri = escPath(m[1])
	}
	if m := f.chLogin.FindStringSubmatch(p); m != nil {
		uri = escPath(m[1]) + f.encU + "/" + f.encP + "/" + escPath(m[2])
	}
	return up, uri
}

// easyForm: an address on the provider's own server as a player is given it: the provider's login, where it
// stands as two parts of the path or as username= and password=, becomes the easy one. ok=false: the front
// door would not swap it back to the very same address.
func (f *front) easyForm(u *url.URL) (string, bool) {
	parts := strings.Split(u.EscapedPath(), "/") // (the rest stays written as the provider wrote it)
	for i := 0; i+1 < len(parts); i++ {
		a, err1 := url.PathUnescape(parts[i])
		b, err2 := url.PathUnescape(parts[i+1])
		if err1 == nil && err2 == nil && a == f.user && b == f.pass {
			parts[i], parts[i+1] = f.pu, f.pp
		}
	}
	p := strings.Join(parts, "/")
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		pairs := strings.Split(u.RawQuery, "&")
		for i, kv := range pairs {
			k, v, _ := strings.Cut(kv, "=")
			key, err1 := url.QueryUnescape(k)
			val, err2 := url.QueryUnescape(v)
			if err1 == nil && err2 == nil && key == "username" && val == f.user {
				pairs[i] = k + "=" + f.pu
			} else if err1 == nil && err2 == nil && key == "password" && val == f.pass {
				pairs[i] = k + "=" + f.pp
			}
		}
		p += "?" + strings.Join(pairs, "&")
	}
	mine, err := url.Parse(p)
	if err != nil {
		return "", false
	}
	up, _ := f.swap(mine)
	back, err := url.Parse(up)
	if err != nil || cleanSlashes(back.Path) != cleanSlashes(u.Path) || back.Query().Encode() != u.Query().Encode() {
		return "", false
	}
	return p, true
}

// hideLogin: a provider's error page as a player is given it. Some servers write the address asked for into
// it ("The requested URL /movie/<login>/... was not found"); the login there becomes the easy one.
func (f *front) hideLogin(page []byte) []byte {
	swaps := [][2]string{{f.user, f.pu}, {f.pass, f.pp}, {f.encU, f.pu}, {f.encP, f.pp}}
	sort.SliceStable(swaps, func(i, j int) bool { return len(swaps[i][0]) > len(swaps[j][0]) }) // (one inside another)
	for _, s := range swaps {
		if s[0] != "" {
			page = bytes.ReplaceAll(page, []byte(s[0]), []byte(s[1]))
		}
	}
	return page
}

// ownServer: an address on the provider's own server, however its name is written: in capitals, with a final dot,
// with the scheme's own port (80, 443) written out or left out.
func (f *front) ownServer(u *url.URL) bool {
	if !strings.EqualFold(u.Scheme, f.scheme) {
		return false
	}
	norm := func(hostport string) string {
		h := &url.URL{Host: hostport}
		port := h.Port()
		if port == "" {
			port = map[bool]string{true: "443", false: "80"}[f.scheme == "https"]
		}
		return strings.TrimSuffix(strings.ToLower(h.Hostname()), ".") + " " + port
	}
	return norm(u.Host) == norm(f.hostport)
}

// cleanSlashes: a path with doubled slashes written once (as the front door's swap writes it).
func cleanSlashes(p string) string {
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

// rewriteLocation: a provider answers "go to that other server". The address is rewritten so the player
// comes back to this proxy for it: /__cdn/<http or https>/<server>/<path>. scheme and hostport are the
// server that answered (for a redirect that names no server).
func rewriteLocation(loc, scheme, hostport string) string {
	switch {
	case strings.HasPrefix(loc, "//"):
		return "/__cdn/" + scheme + "/" + loc[2:]
	case len(loc) > 1 && loc[0] == '/':
		return "/__cdn/" + scheme + "/" + hostport + "/" + loc[1:]
	case len(loc) > 7 && strings.EqualFold(loc[:7], "http://"):
		return "/__cdn/http/" + loc[7:]
	case len(loc) > 8 && strings.EqualFold(loc[:8], "https://"):
		return "/__cdn/https/" + loc[8:]
	}
	return loc
}

// cdnTarget: the real address behind a /__cdn/ request. p is the decoded path, esc the path as the player
// wrote it, query its query. ok=false: not a /__cdn/<http or https>/<server>/ address.
func cdnTarget(p, esc, query string) (scheme, hostport, target string, ok bool) {
	clean := cleanPath(p)
	m := cdnPath.FindStringSubmatch(clean)
	if m == nil {
		return "", "", "", false
	}
	scheme, hostport = m[1], m[2]
	rest := escPath(m[3])
	// Written plainly (the usual case), the rest goes on exactly as the player wrote it, encoding and all.
	if pre := "/__cdn/" + scheme + "/" + hostport + "/"; p == clean && strings.HasPrefix(esc, pre) {
		rest = esc[len(pre):]
	}
	target = scheme + "://" + hostport + "/" + rest
	if query != "" {
		target += "?" + query
	}
	return scheme, hostport, target, true
}

// ---------------------------------------------------------------------------
// The access log: one line per request, written when it ends. Never the address asked for or a header
// (addresses contain the login).
// ---------------------------------------------------------------------------

type counted struct {
	http.ResponseWriter
	status       int
	bytes        int64
	kind, server string
	waitMS       int64
	ask          *askLog // what is asked of the provider, for the log (nil: nothing)
	live         bool    // a piece of a live HLS session (see forwardHeld)
}

func (c *counted) WriteHeader(s int) {
	if c.status == 0 {
		c.status = s
	}
	c.ResponseWriter.WriteHeader(s)
}

func (c *counted) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	n, err := c.ResponseWriter.Write(p)
	c.bytes += int64(n)
	return n, err
}

func (c *counted) Flush() {
	if fl, ok := c.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (c *counted) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func clientOf(r *http.Request) string { // the player's address, from its connection (never from a header)
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "a player"
	}
	return strings.Trim(host, "[]")
}

func (f *front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := &counted{ResponseWriter: w, kind: "other", waitMS: -1}
	began := time.Now()
	defer func() { // (runs when a handler cuts the connection on purpose, too)
		extra := ""
		if c.server != "" {
			extra += " server=" + notPrintable.ReplaceAllString(c.server, "")
		}
		if c.waitMS >= 0 {
			extra += " wait_ms=" + strconv.FormatInt(c.waitMS, 10)
		}
		e := lg.Info()
		if c.kind == "cdn" { // (one per video piece: written at no level the page offers)
			e = lg.Trace()
		}
		e.Msgf("access client=%s provider=%s kind=%s method=%s status=%d bytes=%d seconds=%.3f%s", clientOf(r), f.name, c.kind,
			notPrintable.ReplaceAllString(r.Method, ""), c.status, c.bytes, time.Since(began).Seconds(), extra)
	}()
	f.serve(c, r)
}

// serve: one request, in the order the reverse proxy's settings had it.
func (f *front) serve(c *counted, r *http.Request) {
	p := r.URL.Path
	low := strings.ToLower(cleanPath(p))
	if low == "/healthz" {
		c.kind = "health"
		c.WriteHeader(200)
		return
	}
	up, uri := f.swap(r.URL)
	rt := &route{f: f, uri: uri, orig: r.URL.RequestURI(), client: whoIs(r)}
	switch {
	case low == "/get.php":
		// A provider's ready-made M3U playlist lists every stream at the provider's own address with the
		// real login, so a player using it would skip the VPN. Refused on purpose.
		c.WriteHeader(403)
	case low == "/status":
		c.kind = "status"
		statusPage(c, r)
	case f.liveTS.MatchString(p): // live MPEG-TS: one provider connection per channel, shared by every reader
		c.kind = "media"
		live(c, r, rt, "/"+f.mode)
	case playlistRe.MatchString(p): // HLS playlists and live channels' folders
		c.kind = "hls"
		failed(c, playlistRequest(c, r, rt))
	case r.Method == "HEAD" && (low == "/player_api.php" || low == "/panel_api.php" || low == "/xmltv.php"):
		// (a HEAD is passed on as a HEAD, login swapped: nothing is downloaded to answer it)
		c.kind, c.server = "api", f.hostport
		f.forward(c, r, f.base+up, f.scheme, f.hostport, 60, false)
	case low == "/player_api.php" || low == "/panel_api.php": // the provider's lists: logos and posters pointed back
		// at this proxy (panel_api.php: older apps' login, which names the account and the server just the same)
		c.kind = "api"
		failed(c, api(c, r, rt))
	case cdnForbidden.MatchString(cleanPath(p)):
		c.WriteHeader(403)
	case strings.HasPrefix(low, "/__cdn/") && r.Method != "GET" && r.Method != "HEAD":
		c.WriteHeader(405)
	case strings.HasPrefix(low, "/__cdn/"):
		// Everything a provider pointed at: video pieces, MPEG-TS, movies, catch-up, the guide.
		scheme, hostport, target, ok := cdnTarget(p, r.URL.EscapedPath(), r.URL.RawQuery)
		if !ok {
			c.WriteHeader(404)
			return
		}
		c.kind, c.server = "cdn", hostport
		c.ask = &askLog{kind: "piece", device: whoIs(r), provider: f.name, routine: true}
		u, err := url.Parse("//" + hostport)
		if err != nil || u.Hostname() == "" {
			answer(c, 403, "", "No server named.\n")
			return
		}
		if !nameOK(r.Context(), u.Hostname()) { // where the server's name really leads is checked first
			answer(c, 403, "", "That address leads to this machine or a home network.\n")
			return
		}
		mu.Lock()
		c.live = f.inUse(r.URL.RequestURI(), whoIs(r)) // (a piece of an HLS session: it is still playing)
		c.live = c.live || f.livePiece(r.URL.RequestURI())
		row := f.pieceRow(r.URL.RequestURI(), whoIs(r))
		mu.Unlock()
		w, follow := f.followed(r) // (a redirect of a film or catch-up let through: what it continues)
		f.stream(c, r, w, follow, target, scheme, hostport, true)
		if row != nil {
			mu.Lock()
			row.bytes += c.bytes
			mu.Unlock()
		}
	case strings.HasPrefix(low, "/__cdu/"):
		c.WriteHeader(404)
	case low == "/xmltv.php": // the guide: large, and slow for some providers to start. Its channel and programme
		// pictures (icon src="http://...") are pointed back at this proxy, as in the lists.
		c.kind, c.server = "guide", f.hostport
		failed(c, api(c, r, rt))
	default: // everything else (movies, series, catch-up, account): straight to the provider
		c.kind, c.server = "media", f.hostport
		mu.Lock()
		part := f.inUse(r.URL.RequestURI(), whoIs(r)) // (a piece of an HLS session on the provider's own server)
		row := f.pieceRow(r.URL.RequestURI(), whoIs(r))
		named := f.livePiece(r.URL.RequestURI()) // (read with the lock held: playlists being handed out write that list)
		mu.Unlock()
		if row != nil {
			defer func() {
				mu.Lock()
				row.bytes += c.bytes
				mu.Unlock()
			}()
		}
		c.ask = f.askFor(r.URL, whoIs(r))
		if part {
			c.ask.kind, c.ask.routine = "piece", true
		}
		c.live = part || named
		w, follow := f.followed(r)
		if !follow && !part { // (a piece is counted with its session, not as a stream of its own)
			w = f.classify(r.URL)
		}
		f.stream(c, r, w, follow, f.base+up, f.scheme, f.hostport, false)
	}
}

// stream passes on a film, an episode, catch-up or a redirect of one (w), by the rules this provider has on:
// it counts as a provider connection until it ends, however it ends. Anything else is passed on as it is.
func (f *front) stream(c *counted, r *http.Request, w what, follow bool, target, scheme, hostport string, guard bool) {
	who := whoIs(r)
	if w.kind == "" || r.Method != "GET" || isLoop(who) {
		f.forward(c, r, target, scheme, hostport, 60, guard)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	addr := r.URL.RequestURI()
	// Cut to make room: the provider's read ends, and so does a write to a player that reads no more (its
	// buffer is full, or it opened its new read before closing this one), so the new one need not wait.
	rc := http.NewResponseController(c)
	h, why := f.admit(ctx, who, w, follow, "", func() { cancel(); rc.SetWriteDeadline(time.Now()) }, addr)
	if h == nil {
		if why != "" {
			failed(c, f.refusal(who, w, why))
		}
		return
	}
	defer func() {
		h.release()
		rc.SetWriteDeadline(time.Time{}) // (cut once all was sent: the connection may be used again)
	}()
	f.forwardHeld(c, r.WithContext(ctx), target, scheme, hostport, 60, guard, h)
}

// What belongs to one connection and is not passed on in either direction.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func dropHop(h http.Header) {
	for _, v := range h["Connection"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// forward sends a request on to a provider or a server it sent the player to, and the answer back.
//   - Nothing about the player or this proxy is added (no X-Forwarded-*, no Via): the server sees the
//     player's own request, from the VPN. The player's own Accept-Encoding goes through as it is, and
//     nothing is added or unpacked on the way. Range goes through, so seeking works.
//   - Redirects are rewritten so the player comes back to this proxy for them (rewriteLocation).
//   - Bytes are passed on as they arrive: video must not wait in a buffer.
//   - Certificates are not checked. Many players do not check them either, and some providers' servers
//     have expired or self-made ones; checking here would refuse servers that play fine without the proxy.
//   - Connections to every server are kept open and reused (sharedTransport).
//
// wait: seconds the server may send nothing before it is given up on. guard: the address dialed may not be
// this machine or a home network.
func (f *front) forward(c *counted, r *http.Request, target, scheme, hostport string, wait float64, guard bool) {
	f.forwardHeld(c, r, target, scheme, hostport, wait, guard, nil)
}

// forwardHeld is forward for a stream admitted by the advanced options under hd (advanced.go): a redirect of
// it carries on what it was admitted for, a movie or an episode is read no faster than set, and it ends,
// broken off, when it is cut to make room.
func (f *front) forwardHeld(c *counted, r *http.Request, target, scheme, hostport string, wait float64, guard bool, hd *hold) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if guard {
		ctx = context.WithValue(ctx, guardKey{}, true)
	}
	var body io.Reader
	if r.ContentLength != 0 {
		body = r.Body
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, target, body)
	if err != nil || out.URL.Hostname() == "" || out.URL.User != nil {
		c.WriteHeader(502)
		return
	}
	out.ContentLength = r.ContentLength
	out.Header = r.Header.Clone()
	dropHop(out.Header)
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Via"} {
		out.Header.Del(name)
	}
	if f.agent(out.Header).Get("User-Agent") == "" {
		out.Header["User-Agent"] = []string{""} // no Go-http-client name
	}
	// A piece of a live HLS session: its server gets pieceWait (ExoPlayer's default read timeout, as for the
	// bridge in hlsSource), then it is asked again at once on a new connection, and a piece that goes quiet
	// part way is fetched on from where it stopped (resume). A kept connection that went quiet otherwise held
	// the player for the full wait while a player asking the server itself reconnects. That holds however the
	// piece is asked for and answered: with a Range (players built on FFmpeg ask for every piece with "Range:
	// bytes=0-" and are answered 206), or without a Content-Length.
	live := c.live && hd == nil && r.Method == "GET"
	first := wait
	if live {
		first = math.Min(wait, pieceWait)
	}
	var timed atomic.Bool // (set by the timer's own goroutine)
	t := time.AfterFunc(secs(first), func() { timed.Store(true); cancel() })
	began := time.Now()
	resp, err := sharedTransport.RoundTrip(out)
	t.Stop()
	if err != nil && live && timed.Load() && r.Context().Err() == nil {
		lg.Info().Str("server", out.URL.Hostname()).Msgf("a piece of video sent nothing for %d s; asked again on a new connection", pieceWait)
		var cancel2 context.CancelFunc
		ctx, cancel2 = context.WithCancel(r.Context())
		defer cancel2()
		if guard {
			ctx = context.WithValue(ctx, guardKey{}, true)
		}
		out = out.Clone(ctx)
		timed.Store(false)
		t = time.AfterFunc(secs(wait), func() { timed.Store(true); cancel2() })
		resp, err = plainTransport.RoundTrip(out)
		t.Stop()
	}
	if err != nil {
		var rf *refused
		switch {
		case errors.As(err, &rf):
			c.ask.failed(rf, false, time.Since(began))
			c.WriteHeader(rf.status)
		case hd.wasCut(): // its device asked for something else: this made room
			c.ask.failed(err, true, time.Since(began))
			panic(http.ErrAbortHandler)
		case r.Context().Err() != nil: // the player hung up first
			c.ask.failed(err, true, time.Since(began))
		case timed.Load() || isTimeout(err):
			c.ask.failed(timeoutError{}, false, time.Since(began))
			c.WriteHeader(504)
		default:
			if c.ask == nil {
				say("a server could not be reached: %s", pyName(err))
			}
			c.ask.failed(err, false, time.Since(began))
			c.WriteHeader(502)
		}
		return
	}
	c.ask.answered(resp.StatusCode, time.Since(began))
	defer resp.Body.Close()
	c.waitMS = time.Since(began).Milliseconds()
	h := c.Header()
	dropHop(resp.Header)
	for k, vs := range resp.Header {
		h[k] = vs
	}
	h.Del("Via") // players see what the server sends, without a note that a proxy passed it on
	if loc := resp.Header.Get("Location"); loc != "" {
		moved := rewriteLocation(loc, scheme, hostport)
		// back to the provider's own server: to its port, with the easy login (never the real one)
		if u, err := out.URL.Parse(loc); err == nil && f.ownServer(u) {
			if p, ok := f.easyForm(u); ok {
				moved = p
			}
		}
		h.Set("Location", moved)
		if hd != nil && moved != loc { // the player comes back for it: it carries on what was admitted
			mu.Lock()
			f.remember(moved, hd)
			mu.Unlock()
		}
	}
	if _, ok := h["Content-Type"]; !ok {
		h["Content-Type"] = nil // none sent, none added
	}
	var src io.Reader = &quietBody{resp.Body, secs(first)}
	if live && resp.ContentLength != 0 && resp.Header.Get("Content-Encoding") == "" {
		// The whole piece, or a part of it whose place the server named: it can be carried on where it stops.
		from, to, whole := int64(0), "", resp.StatusCode == 200 && out.Header.Get("Range") == ""
		m := partRe.FindStringSubmatch(resp.Header.Get("Content-Range"))
		if resp.StatusCode == 206 && m != nil {
			from, _ = strconv.ParseInt(m[1], 10, 64)
			to = m[2]
		}
		if size := resp.ContentLength; whole || to != "" {
			if size < 0 {
				size = math.MaxInt64 // (no length sent: it ends when the server ends it)
			}
			src = &resumeBody{body: resp.Body, out: out, from: from, to: to, size: size, wait: wait}
		}
	}
	if resp.StatusCode >= 400 && !guard && r.Method != "HEAD" { // the provider's own error page (see hideLogin)
		// Packed, as the player allowed (Apache packs its error pages): unpacked to be read, and passed on plain.
		// Packed in a way this cannot read: the page is left out (its status still goes on).
		switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
		case "", "identity":
		case "gzip", "x-gzip":
			if z, err := gzip.NewReader(src); err == nil {
				src = z
			} else {
				src = bytes.NewReader(nil)
			}
			h.Del("Content-Encoding")
		default:
			src = bytes.NewReader(nil)
			h.Del("Content-Encoding")
		}
		page, err := io.ReadAll(io.LimitReader(src, 65536))
		if err != nil && r.Context().Err() == nil {
			panic(http.ErrAbortHandler)
		}
		h.Del("Content-Length")
		src = io.MultiReader(bytes.NewReader(f.hideLogin(page)), src)
	}
	c.WriteHeader(resp.StatusCode)
	c.Flush()
	if r.Method == "HEAD" {
		return
	}
	kind, who := strings.ToLower(resp.Header.Get("Content-Type")), whoIs(r)
	if resp.StatusCode < 300 && who != "127.0.0.1" && who != "::1" && // (not the relay's own bridge)
		(strings.HasPrefix(kind, "video/") || strings.HasPrefix(kind, "audio/") || strings.HasPrefix(kind, "application/octet-stream")) {
		defer watching(who)() // a film, an episode or catch-up: someone is watching (see playingNow)
	}
	if p := f.speedOf(r, hd); p != nil && resp.StatusCode < 300 {
		src = &throttle{r: src, ctx: r.Context(), p: p}
	}
	if hd != nil && hd.w.kind == "ch" && r.Method == "GET" { // a catch-up read: one line when it ends, however it ends
		t0, rg, st := time.Now(), out.Header.Get("Range"), resp.StatusCode
		defer func() {
			lg.Info().Str("device", who).Str("channel", hd.w.item).Str("range", rg).Int("status", st).Int64("bytes", c.bytes).
				Float64("seconds", round1(time.Since(t0).Seconds())).Bool("cut", hd.wasCut()).
				Msgf("catch-up read by %s on channel %s: %q, status %d, %.1f MB in %.1f s", who, hd.w.item, rg, st,
					float64(c.bytes)/1048576, time.Since(t0).Seconds())
		}()
	}
	if _, err := copyFlush(c, src); err != nil && (r.Context().Err() == nil || hd.wasCut()) {
		panic(http.ErrAbortHandler) // cut off: the player must see a broken answer, not a short complete one
	}
}

// resumeBody: a live HLS piece passed on as it arrives. If its server sends nothing for pieceWait (or the
// connection breaks) part way, the rest is asked for once with a Range on a new connection and passed on after
// what was sent, so the player gets the whole piece on its one answer. A part of a piece (an answer to a Range,
// from..to of the whole) is carried on the same way, up to where the part ends.
type resumeBody struct {
	body    io.ReadCloser
	out     *http.Request
	from    int64  // where this answer starts in the whole piece
	to      string // where it ends ("": at the piece's end)
	size    int64  // its length (Content-Length)
	got     int64
	wait    float64
	resumed bool
}

func (b *resumeBody) Read(p []byte) (int, error) {
	n, err := readWithDeadline(b.body, p, secs(pieceWait))
	b.got += int64(n)
	if err == nil || err == io.EOF || b.resumed || b.got >= b.size || b.out.Context().Err() != nil {
		return n, err
	}
	b.resumed = true
	b.body.Close()
	lg.Info().Str("server", b.out.URL.Hostname()).Int64("kb", b.got>>10).
		Msgf("a piece of video stopped part way; the rest asked for on a new connection")
	req := b.out.Clone(b.out.Context())
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%s", b.from+b.got, b.to))
	var timed atomic.Bool
	ctx, cancel := context.WithCancel(b.out.Context())
	req = req.WithContext(ctx)
	t := time.AfterFunc(secs(b.wait), func() { timed.Store(true); cancel() })
	resp, rerr := plainTransport.RoundTrip(req)
	t.Stop()
	if rerr != nil {
		cancel()
		return n, err
	}
	if resp.StatusCode != 206 || !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", b.from+b.got)) {
		resp.Body.Close()
		cancel()
		return n, err // (no way to carry on where it stopped: the player sees a broken answer, as before)
	}
	b.body = &cancelBody{resp.Body, cancel}
	if n > 0 {
		return n, nil
	}
	return b.Read(p)
}

// cancelBody: a body whose request context is let go when it is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelBody) Close() error { c.cancel(); return c.ReadCloser.Close() }

// quietBody gives up on a server that sends nothing for wait.
type quietBody struct {
	body io.ReadCloser
	wait time.Duration
}

func (q *quietBody) Read(p []byte) (int, error) { return readWithDeadline(q.body, p, q.wait) }

// ---------------------------------------------------------------------------
// inside: the relay's own requests to this front door (a recorder's MPEG-TS made from a channel's HLS reads
// the players' own playlist addresses), answered inside the program: no socket, no second copy of the rules.
// ---------------------------------------------------------------------------

type inside struct{ f *front }

type insideWriter struct {
	h      http.Header
	status int
	pw     *io.PipeWriter
	ready  chan struct{}
	sent   bool
}

func (w *insideWriter) Header() http.Header { return w.h }

func (w *insideWriter) WriteHeader(s int) {
	if !w.sent {
		w.sent, w.status = true, s
		w.h = w.h.Clone()
		close(w.ready)
	}
}

func (w *insideWriter) Write(p []byte) (int, error) {
	w.WriteHeader(200)
	return w.pw.Write(p)
}

func (w *insideWriter) Flush() {}

func (in inside) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	w := &insideWriter{h: http.Header{}, pw: pw, ready: make(chan struct{})}
	r := req.Clone(req.Context())
	r.RemoteAddr, r.Host, r.RequestURI, r.Body = "127.0.0.1:0", req.URL.Host, req.URL.RequestURI(), http.NoBody
	go func() {
		defer func() {
			// (called off before the handler was done, by the reader's own wait for bytes: what it wrote is not
			// the whole answer either, and must not read as one. A handler does not cut an answer short on
			// purpose once its player is gone, so without this a piece that went quiet part way ended cleanly
			// here and was handed on as if whole)
			if e := recover(); e != nil || r.Context().Err() != nil {
				pw.CloseWithError(io.ErrUnexpectedEOF)
			}
			w.WriteHeader(200)
			pw.Close()
		}()
		in.f.ServeHTTP(w, r)
	}()
	select {
	case <-w.ready:
	case <-req.Context().Done():
		pr.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	resp := &http.Response{StatusCode: w.status, Status: strconv.Itoa(w.status) + " " + http.StatusText(w.status), Proto: "HTTP/1.1",
		ProtoMajor: 1, ProtoMinor: 1, Header: w.h, Body: pr, ContentLength: -1, Request: req}
	return resp, nil
}

// ---------------------------------------------------------------------------
// The proxy part: checks the settings, listens on each provider's port, stops when told to.
// ---------------------------------------------------------------------------

// checkProviders sorts the settings into the providers that start and the notes for the status page.
func checkProviders(s *settings) (good []provider, notes map[string]any) {
	problems, list := []any{}, []any{}
	errs := s.validate()
	stop := ""
	for _, e := range errs {
		if strings.HasPrefix(e.Field, "player.") && stop == "" {
			stop = e.Message
		}
	}
	if stop != "" {
		warn("PROXY SETTINGS PROBLEM: %s", stop)
		problems = append(problems, stop+".")
	}
	for i, p := range s.Providers {
		bad := ""
		for _, e := range errs {
			if strings.HasPrefix(e.Field, "providers["+strconv.Itoa(i)+"].") && bad == "" {
				bad = e.Message
			}
		}
		name := notPrintable.ReplaceAllString(p.Name, "")
		switch {
		case stop != "":
		case bad != "":
			warn("PROVIDER %s SKIPPED: %s", name, bad)
			list = append(list, map[string]any{"name": name, "port": "", "ok": false, "why": bad})
		default:
			lg.Info().Str("provider", name).Str("port", p.Port.String()).Msgf("PROVIDER %s on port %s -> %s", name, p.Port.String(), strings.TrimRight(p.URL, "/"))
			list = append(list, map[string]any{"name": name, "port": p.Port.String(), "ok": true, "mode": p.LiveTV,
				"connections_missing": p.Connections == ""})
			good = append(good, p)
		}
	}
	return good, map[string]any{"problems": problems, "providers": list}
}

// runProxy is the proxy part. The launcher starts it once the VPN is up and hands it the player and provider
// settings on its standard input (never the VPN's); it runs until it is told to stop.
func runProxy(in io.Reader, stop <-chan struct{}) int {
	data, err := io.ReadAll(io.LimitReader(in, 4<<20))
	s := &settings{}
	if err == nil {
		err = s.parse(data)
	}
	if err != nil {
		lg.Error().Msgf("the proxy part could not read its settings: %v", err)
		return 2
	}
	setLogLevel(s.UI.LogLevel)
	good, notes := checkProviders(s)
	startNotes = notes
	// When the VPN came up (the launcher says): a provider that locks an account to one address gets new
	// streams only a while after that (advanced.go).
	if t, err := strconv.ParseInt(os.Getenv("COAXIAL_VPN_UP_AT"), 10, 64); err == nil && t > 0 {
		mu.Lock()
		vpnChanged = mono() - time.Since(time.Unix(t, 0)).Seconds()
		mu.Unlock()
	}
	var servers []*http.Server
	var listeners []net.Listener
	for _, p := range good {
		f := newFront(p, s.Player)
		ln, err := net.Listen("tcp", ":"+f.port)
		// A save can move the web interface off a port and a provider onto it: the launcher lets the old port go
		// a moment after this part has started. That moment is waited for (a few seconds at most).
		for try := 0; errors.Is(err, syscall.EADDRINUSE) && try < 20; try++ {
			time.Sleep(250 * time.Millisecond)
			ln, err = net.Listen("tcp", ":"+f.port)
		}
		if err != nil {
			lg.Error().Str("provider", f.name).Msgf("PROVIDER %s NOT STARTED: port %s could not be opened (%v)", f.name, f.port, err)
			continue
		}
		fronts[f.port] = f
		servers = append(servers, &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 5 * time.Minute,
			MaxHeaderBytes: 64 << 10, ErrorLog: log.New(io.Discard, "", 0)})
		listeners = append(listeners, ln)
	}
	for i, srv := range servers { // (every provider is known before the first request is answered)
		go srv.Serve(listeners[i])
	}
	if len(fronts) == 0 {
		warn("NO PROVIDERS STARTED: fix the settings in the web interface or in settings.json")
	} else {
		first := ""
		for port := range fronts {
			if n, _ := strconv.Atoi(port); first == "" || n < atoi(first) {
				first = port
			}
		}
		say("ready, for %d provider%s", len(fronts), plural(len(fronts), "", "s"))
		say("STATUS PAGE: open http://<this machine's address>:%s/status in a web browser", first)
	}
	namesFile = os.Getenv("COAXIAL_NAMES_FILE") // (channel names for the status page, kept between starts)
	loadNames()
	go namesLoop(stop)
	if ln, err := net.Listen("unix", insideSocket); err == nil { // the launcher asks here what is playing
		isrv := &http.Server{Handler: http.HandlerFunc(insideStatus), ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
		servers = append(servers, isrv)
		go isrv.Serve(ln)
	}
	<-stop
	for _, srv := range servers {
		srv.Close() // let go of every stream at once
	}
	mu.Lock()
	for _, ch := range channels {
		ch.cancel()
	}
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	endShares()
	return 0
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
