// The front door: one listener per provider. It does what the reverse proxy in front of the relay used to
// do, in the same order: the easy login swapped for the provider's, redirects kept inside the tunnel
// (/__cdn/), the provider's ready-made playlist refused, and everything else passed on untouched. Live
// MPEG-TS, playlists, the provider's lists and the status page are handed to the relay as function calls.
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// front: one provider and its port.
type front struct {
	name, port, mode       string // mode: "ts" or "hls" (the live_tv setting)
	base, scheme, hostport string // the provider's address without a final slash, and its two halves
	user, pass             string // the provider's login as issued
	encU, encP             string // the same, percent-encoded for an address
	pu, pp                 string // the easy login the players use

	loginPath, loginShort, chLogin, liveTS *regexp.Regexp
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
	return &front{name: p.Name, port: p.Port.String(), mode: p.LiveTV, base: base, scheme: strings.ToLower(scheme), hostport: hostport,
		user: p.User, pass: p.Password, encU: encLogin(p.User), encP: encLogin(p.Password), pu: pl.User, pp: pl.Password,
		loginPath:  regexp.MustCompile(`^/+(live|movie|series|timeshift)/+` + pu + `/+` + pp + `/+(.*)$`),
		loginShort: regexp.MustCompile(`^/+` + pu + `/+` + pp + `/+([^/]+)$`),
		chLogin:    regexp.MustCompile(`^/__ch(/+(?:live/+)?)` + pu + `/+` + pp + `/+(.+?\.m3u8)/`),
		liveTS:     regexp.MustCompile(`^/+(live/+` + pu + `/+` + pp + `/+[^/]+\.ts|` + pu + `/+` + pp + `/+[0-9]+(\.ts)?)$`)}
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
		say("access client=%s provider=%s kind=%s method=%s status=%d bytes=%d seconds=%.3f%s", clientOf(r), f.name, c.kind,
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
	case low == "/player_api.php": // the provider's lists: logos and posters pointed back at this proxy
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
		u, err := url.Parse("//" + hostport)
		if err != nil || u.Hostname() == "" {
			answer(c, 403, "", "No server named.\n")
			return
		}
		if !nameOK(r.Context(), u.Hostname()) { // where the server's name really leads is checked first
			answer(c, 403, "", "That address leads to this machine or a home network.\n")
			return
		}
		f.forward(c, r, target, scheme, hostport, 60, true)
	case strings.HasPrefix(low, "/__cdu/"):
		c.WriteHeader(404)
	case low == "/xmltv.php": // the guide: large, and slow for some providers to start
		c.kind, c.server = "guide", f.hostport
		f.forward(c, r, f.base+up, f.scheme, f.hostport, 300, false)
	default: // everything else (movies, series, catch-up, account): straight to the provider
		c.kind, c.server = "media", f.hostport
		f.forward(c, r, f.base+up, f.scheme, f.hostport, 60, false)
	}
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
	if out.Header.Get("User-Agent") == "" {
		out.Header["User-Agent"] = []string{""} // no Go-http-client name
	}
	timed := false
	t := time.AfterFunc(secs(wait), func() { timed = true; cancel() })
	began := time.Now()
	resp, err := sharedTransport.RoundTrip(out)
	t.Stop()
	if err != nil {
		var rf *refused
		switch {
		case errors.As(err, &rf):
			c.WriteHeader(rf.status)
		case r.Context().Err() != nil: // the player hung up first
		case timed || isTimeout(err):
			c.WriteHeader(504)
		default:
			say("a server could not be reached: %s", pyName(err))
			c.WriteHeader(502)
		}
		return
	}
	defer resp.Body.Close()
	c.waitMS = time.Since(began).Milliseconds()
	h := c.Header()
	dropHop(resp.Header)
	for k, vs := range resp.Header {
		h[k] = vs
	}
	h.Del("Via") // players see what the server sends, without a note that a proxy passed it on
	if loc := resp.Header.Get("Location"); loc != "" {
		h.Set("Location", rewriteLocation(loc, scheme, hostport))
	}
	if _, ok := h["Content-Type"]; !ok {
		h["Content-Type"] = nil // none sent, none added
	}
	c.WriteHeader(resp.StatusCode)
	c.Flush()
	if r.Method == "HEAD" {
		return
	}
	if _, err := copyFlush(c, &quietBody{resp.Body, secs(wait)}); err != nil && r.Context().Err() == nil {
		panic(http.ErrAbortHandler) // cut off: the player must see a broken answer, not a short complete one
	}
}

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
			if e := recover(); e != nil {
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
		say("PROXY SETTINGS PROBLEM: %s", stop)
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
			say("PROVIDER %s SKIPPED: %s", name, bad)
			list = append(list, map[string]any{"name": name, "port": "", "ok": false, "why": bad})
		default:
			say("PROVIDER %s on port %s -> %s", name, p.Port.String(), strings.TrimRight(p.URL, "/"))
			list = append(list, map[string]any{"name": name, "port": p.Port.String(), "ok": true, "mode": p.LiveTV})
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
		say("the proxy part could not read its settings: %v", err)
		return 2
	}
	good, notes := checkProviders(s)
	startNotes = notes
	var servers []*http.Server
	var listeners []net.Listener
	for _, p := range good {
		f := newFront(p, s.Player)
		ln, err := net.Listen("tcp", ":"+f.port)
		if err != nil {
			say("PROVIDER %s NOT STARTED: port %s could not be opened (%v)", f.name, f.port, err)
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
		say("NO PROVIDERS STARTED: fix the settings in the web interface or in settings.json")
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
	return 0
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
