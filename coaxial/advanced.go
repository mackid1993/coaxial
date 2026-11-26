// Advanced options: what some providers need handled, set per provider under "advanced". Every one is off
// unless the settings turn it on, except stay_under_limit. Nothing here asks a provider anything of its own:
// the rules act only on what players ask for.
//
//	stay_under_limit       a new stream that would go over the connections the user bought is refused (the
//	                       number is the provider's own "connections" setting, typed in by the user; with none
//	                       there, nothing is refused for it)
//	one_per_channel        a second provider connection to a channel that is open (live or catch-up) is refused
//	one_per_item           a second device on a movie or episode that is playing is refused
//	locked_to_address      the VPN server is never changed by Coaxial on its own, and new streams wait for
//	address_wait_seconds   this long after the VPN came up or changed address (90 when left out)
//	send_app_name          every request to the provider names this app (User-Agent):
//	app_name
//	share_lists            the provider's lists and guide are downloaded once and shared for
//	share_minutes          this many minutes (10 when left out); see share.go
//	limit_movie_speed      movies and episodes are read from the provider no faster than
//	movie_mbps             this many megabits a second (100 when left out)
//
// The same device asking again (changing channel, rewinding, seeking) replaces its own connection instead of
// being refused: the old one is cut and gets up to grace seconds to close before the new one is opened.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// The settings
// ---------------------------------------------------------------------------

type advanced struct {
	StayUnder     *bool  `json:"stay_under_limit"` // (nil: left out, which means on)
	OnePerChannel bool   `json:"one_per_channel"`
	OnePerItem    bool   `json:"one_per_item"`
	Locked        bool   `json:"locked_to_address"`
	LockWait      number `json:"address_wait_seconds"`
	SendApp       bool   `json:"send_app_name"`
	AppName       string `json:"app_name"`
	Share         bool   `json:"share_lists"`
	ShareMinutes  number `json:"share_minutes"`
	SlowMovies    bool   `json:"limit_movie_speed"`
	MovieMbps     number `json:"movie_mbps"`

	// OldConnections: where older settings kept the connections the account allows. Read only to move it to the
	// provider's own "connections" (see parse); never written.
	OldConnections number `json:"connections,omitempty"`

	channelSet bool // one_per_channel was written in the file (see parse: catch_up_while_live)
}

const (
	defaultLockWait     = 90
	defaultShareMinutes = 10
	defaultMovieMbps    = 100
)

func (a *advanced) UnmarshalJSON(b []byte) error {
	type plain advanced
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	json.Unmarshal(b, &keys)
	*a = advanced(p)
	_, a.channelSet = keys["one_per_channel"]
	return nil
}

// number: a whole number as written in the file or sent by the web page: a number, text that should be one,
// or nothing ("").
type number string

func (n *number) UnmarshalJSON(b []byte) error {
	*n = number(strings.TrimSpace(strings.Trim(strings.TrimSpace(string(b)), `"`)))
	if *n == "null" {
		*n = ""
	}
	return nil
}

func (n number) MarshalJSON() ([]byte, error) {
	if n == "" {
		return []byte("null"), nil
	}
	if v, ok := n.value(); ok {
		return []byte(strconv.Itoa(v)), nil
	}
	return json.Marshal(string(n))
}

var numberRe = regexp.MustCompile(`^[0-9]{1,6}$`)

func (n number) value() (int, bool) {
	if !numberRe.MatchString(string(n)) {
		return 0, false
	}
	v, _ := strconv.Atoi(string(n))
	return v, true
}

// or: the number, or def when none is written; ok=false when it is not a whole number from lo to hi.
func (n number) or(def, lo, hi int) (int, bool) {
	if n == "" {
		return def, true
	}
	v, ok := n.value()
	return v, ok && v >= lo && v <= hi
}

func (a advanced) limitOn() bool { return a.StayUnder == nil || *a.StayUnder }

var appNameRe = regexp.MustCompile(`^[\x20-\x7e]{1,200}$`)

// check: what is wrong with the advanced options, by key. Only what is on is checked.
func (a advanced) check() []fieldError {
	var errs []fieldError
	bad := func(field, msg string) { errs = append(errs, fieldError{field, msg}) }
	if _, ok := a.LockWait.or(defaultLockWait, 10, 600); a.Locked && !ok {
		bad("address_wait_seconds", "the wait after the VPN changes must be a whole number of seconds from 10 to 600")
	}
	if a.SendApp {
		switch name := strings.TrimSpace(a.AppName); {
		case name == "":
			bad("app_name", "type the app name to send, or turn this option off")
		case !appNameRe.MatchString(name):
			bad("app_name", "the app name must be plain text (letters, digits, spaces and punctuation), 200 characters at most")
		}
	}
	if _, ok := a.ShareMinutes.or(defaultShareMinutes, 1, 1440); a.Share && !ok {
		bad("share_minutes", "the minutes a download is shared must be a whole number from 1 to 1440")
	}
	if _, ok := a.MovieMbps.or(defaultMovieMbps, 1, 10000); a.SlowMovies && !ok {
		bad("movie_mbps", "the movie speed must be a whole number of Mbit/s from 1 to 10000")
	}
	return errs
}

// view: the options for the web page, with the numbers that are not written filled in as they apply.
func (a advanced) view() advanced {
	on := a.limitOn()
	a.StayUnder = &on
	for _, n := range []struct {
		v   *number
		def int
	}{{&a.LockWait, defaultLockWait}, {&a.ShareMinutes, defaultShareMinutes}, {&a.MovieMbps, defaultMovieMbps}} {
		if *n.v == "" {
			*n.v = number(strconv.Itoa(n.def))
		}
	}
	return a
}

// applyAdvanced: the options as the front door uses them.
func (f *front) applyAdvanced(a advanced) {
	f.limitOn = a.limitOn()
	f.oneChannel, f.oneItem, f.locked = a.OnePerChannel, a.OnePerItem, a.Locked
	if f.locked {
		v, _ := a.LockWait.or(defaultLockWait, 10, 600)
		f.lockWait = float64(v)
	}
	if name := strings.TrimSpace(a.AppName); a.SendApp && appNameRe.MatchString(name) {
		f.app = name
	}
	if a.Share {
		v, _ := a.ShareMinutes.or(defaultShareMinutes, 1, 1440)
		f.shareFor = float64(v) * 60
	}
	if a.SlowMovies {
		v, _ := a.MovieMbps.or(defaultMovieMbps, 1, 10000)
		f.movieRate = float64(v) * 1e6 / 8
	}
}

// agent: h with the app name this provider is to be told, when one is set (otherwise the player's own).
func (f *front) agent(h http.Header) http.Header {
	if f.app != "" {
		h.Set("User-Agent", f.app)
	}
	return h
}

// ---------------------------------------------------------------------------
// What a request asks for, as the rules count it
// ---------------------------------------------------------------------------

// what: a stream. kind "ch": a channel, live or catch-up at any time (item: its number); "vod": a movie or an
// episode (item: movie/<id> or series/<id>); "" for anything else (lists, pictures, pieces of a playlist).
type what struct{ kind, item string }

// classify: the stream an address asks for, with the easy login, in any form the front door takes.
func (f *front) classify(u *url.URL) what {
	p := u.Path
	if f.liveTS.MatchString(p) || f.liveHLS.MatchString(p) {
		return what{"ch", channelID(p)}
	}
	if id := f.catchUpOf(u); id != "" {
		return what{"ch", id}
	}
	if m := f.vodPath.FindStringSubmatch(p); m != nil {
		return what{"vod", m[1] + "/" + m[2]}
	}
	return what{}
}

// same: whether two streams are one for the rules. A channel number written with leading zeros is the same
// channel (the provider reads it as a number).
func same(a, b what) bool {
	if a.kind == "ch" && b.kind == "ch" {
		return strings.TrimLeft(a.item, "0") == strings.TrimLeft(b.item, "0")
	}
	return a == b
}

func isLoop(who string) bool { return who == "127.0.0.1" || who == "::1" } // (the relay's own bridge)

// ---------------------------------------------------------------------------
// The connections held to a provider
// ---------------------------------------------------------------------------

const (
	grace   = 2.0  // seconds a connection cut to make room gets to close before the new one is opened
	hlsHeld = 30.0 // an HLS session counts as a connection this long after its last playlist request...
	hlsIdle = 12.0 // ...and once nobody has asked for it for this long, it is the first let go of (both at least, see held)
)

// hold: a provider connection admitted by the rules that the relay's own tables do not show: a film, an
// episode or catch-up passed on, or a channel or session in the moment before it is opened.
type hold struct {
	f      *front
	who    string
	w      what
	key    string // the channel or session it opens ("" for one passed on)
	at     float64
	cutAt  float64 // when it was cut to make room (0: it was not)
	cut    func()
	shared bool         // it joins a channel or session that is open or being opened: counted there
	moved  bool         // it was answered with a redirect: its place is kept for the follow-up when it ends
	until  float64      // the place kept for a follow-up until then (0: it is no such place)
	addr   string       // the address asked (catch-up and its archive reads: see admit)
	asked  func(string) // called once with the redirect given ("" for none) when the answer arrives (see cuAsked)
	loc    string       // the redirect it was answered with, as given to the player ("" for none)
}

var holds = map[*hold]bool{}

// release: the hold is over (on every way out of the request). Safe to call twice, or on nil.
func (h *hold) release() {
	if h == nil {
		return
	}
	mu.Lock()
	delete(holds, h)
	if !h.shared && h.cutAt == 0 && h.addr != "" && h.key == "" { // a read that joined this one goes on: it counts now
		for o := range holds {
			if o.shared && o.f == h.f && o.key == "" && o.addr == h.addr && o.who == h.who && o.until == 0 {
				o.shared = false
				break
			}
		}
	}
	if now := mono(); h.moved && h.cutAt == 0 && h.until == 0 {
		h.f.keepFor(h.who, h.w, now+followUpHold)
	}
	mu.Unlock()
}

func (h *hold) wasCut() bool {
	if h == nil {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	return h.cutAt > 0
}

// conn: one connection held to a provider now, as the rules count it.
type conn struct {
	w     what
	who   map[string]bool // the devices on it (none: nobody is, it is on its way out)
	since float64
	idle  bool    // an HLS session nobody asked for in a while
	cutAt float64 // cut to make room, not closed yet
	end   func()  // cuts it (mu held)
	kept  *hold   // the place kept for a redirect's follow-up, when it is that
}

// open: whether a channel or a session is open now under key, or being opened. A session nobody asked for in
// its held time (hlsHeld, or longer for long pieces) no longer counts: whoever comes back to it is a new
// connection. (mu held)
func open(key string) bool {
	if ch := channels[key]; ch != nil && !ch.done {
		return true
	}
	s := sessions[key]
	return s != nil && mono()-s.used <= s.held()
}

// conns: every connection held to this provider now. A device joining a channel that is open costs nothing:
// a shared channel or session is one connection, whoever reads it. Two providers in the settings can be two
// accounts on one server: each counts only its own. (mu held)
func (f *front) conns() []*conn {
	now := mono()
	var out []*conn
	live := map[string]*conn{} // live HLS sessions by channel number (a bridge to MPEG-TS rides on its channel's)
	for k, s := range sessions {
		if !strings.HasPrefix(k, f.base+" ") || s.owner != nil && s.owner != f || now-s.used > s.held() {
			continue
		}
		c := &conn{w: s.what, who: map[string]bool{}, since: s.opened, idle: now-s.used > s.idle()}
		for d, t := range s.who {
			if now-t <= s.held() {
				c.who[d] = true
			}
		}
		key, id := k, s.what.item
		c.end = func() {
			delete(sessions, key)
			for _, ch := range channels { // (a bridge to MPEG-TS would open it again at once)
				if ch.provider == f.base && (ch.owner == nil || ch.owner == f) && strings.HasPrefix(ch.key, "/hls ") && id != "" && lastPart(ch.key) == id {
					ch.drop()
				}
			}
		}
		if s.what.kind == "ch" {
			live[s.what.item] = c
		}
		out = append(out, c)
	}
	for _, ch := range channels {
		if ch.provider != f.base || ch.owner != nil && ch.owner != f || ch.done {
			continue
		}
		id, sg := lastPart(ch.key), segmenters[ch.key]
		who := map[string]bool{}
		for rd := range ch.readers {
			if sg == nil || sg.rd != rd {
				who[rd.who] = true
			}
		}
		if sg != nil && sg.ch == ch {
			for d, t := range sg.players {
				if now-t <= 3*sg.target() || sg.waiting[d] > 0 {
					who[d] = true
				}
			}
		}
		if strings.HasPrefix(ch.key, "/hls ") {
			if c := live[id]; c != nil { // fed from that session: its readers are on it
				for d := range who {
					c.who[d] = true
				}
				continue
			}
		}
		c := &conn{w: what{"ch", id}, who: who, since: ch.asked, end: ch.drop}
		out = append(out, c)
	}
	for h := range holds {
		if h.until > 0 && now >= h.until { // (a follow-up that never came)
			delete(holds, h)
			continue
		}
		if h.f != f || h.shared || (h.key != "" && open(h.key)) {
			continue
		}
		h := h
		c := &conn{w: h.w, who: map[string]bool{h.who: true}, since: h.at, cutAt: h.cutAt}
		c.end = func() {
			h.cutAt = mono()
			if h.cut != nil {
				h.cut()
			}
		}
		if h.until > 0 {
			c.kept, c.end = h, func() { delete(holds, h) }
		}
		out = append(out, c)
	}
	return out
}

// variant: a playlist at target is a variant named in the playlist of an HLS session of this provider (its
// own playlist, in the same folder or below): the player asks for that one only, and the session goes on.
// That session is kept counted for device who, and returned (nil: none). (mu held)
func (f *front) variant(target, who string) *session {
	for k, s := range sessions {
		final, _, _ := strings.Cut(s.final, "?")
		dir := final[:strings.LastIndexByte(final, '/')+1]
		if strings.HasPrefix(k, f.base+" ") && strings.Count(dir, "/") > 3 && strings.HasPrefix(target, dir) {
			s.used = mono()
			if s.who != nil && !isLoop(who) {
				s.who[who] = mono()
			}
			return s
		}
	}
	return nil
}

// inUse: a request for an address a session's playlist gave the players (a piece, or a playlist a main
// playlist names) keeps the session in use, with the device asking on it, and is part of it: true. A complete
// playlist (catch-up) is asked for once, and its pieces for as long as it plays. (mu held)
func (f *front) inUse(uri, who string) bool {
	part := false
	for k, s := range sessions {
		if strings.HasPrefix(k, f.base+" ") && s.pieces[uri] {
			s.used, part = mono(), true
			if s.who != nil && !isLoop(who) {
				s.who[who] = mono()
			}
		}
	}
	return part
}

// pieceRow: the status page row of the player asking for a piece of an HLS session (nil: none). (mu held)
func (f *front) pieceRow(uri, who string) *hlsRow {
	for k, s := range sessions {
		if strings.HasPrefix(k, f.base+" ") && s.pieces[uri] {
			return hlsRows[who+" "+f.base+" "+channelID(strings.TrimPrefix(k, f.base+" "))]
		}
	}
	return nil
}

func lastPart(key string) string { return key[strings.LastIndexByte(key, '/')+1:] }

// limit: the connections this provider allows at once: the number the user typed in, while stay_under_limit is
// on (0: no limit, because the option is off or no number was typed in). It is never asked of the provider.
// (mu held)
func (f *front) limit() int {
	if !f.limitOn {
		return 0
	}
	return f.bought
}

// ---------------------------------------------------------------------------
// Deciding
// ---------------------------------------------------------------------------

type verdict int

const (
	admitNow verdict = iota
	waitNow          // a connection was cut to make room (or is closing): ask again in a moment
	refuseNow
)

// vpnChanged: when the VPN last came up or changed address (mono; 0: not known). (mu)
var vpnChanged float64

// lockLeft: seconds new streams still wait after the VPN came up or changed address at changed, for a provider
// that locks an account to one address (locked) and is given wait seconds to let go of the old one.
func lockLeft(locked bool, wait, changed, now float64) float64 {
	if !locked || changed <= 0 {
		return 0
	}
	return math.Max(0, changed+wait-now)
}

// decide: whether device who may open a new connection to this provider for w now. What it replaces it cuts
// here. follow: w continues a request already admitted (a redirect of it, asked for at the address the player
// was sent to). It takes the place kept for it, and is otherwise a new connection like any other: the device's
// own connections make room, and at the limit, or with another device on the same channel or item, it is
// refused (the provider would end someone's stream). It waits after an address change like any other. (mu held)
func (f *front) decide(who string, w what, follow bool, now float64) (verdict, string) {
	if left := lockLeft(f.locked, f.lockWait, vpnChanged, now); left > 0 {
		if left > 5 {
			return refuseNow, "lock"
		}
		return waitNow, "" // (a few seconds: waited out)
	}
	list := f.conns()
	if follow { // (the place kept for this follow-up is its own: it takes it)
		for i := 0; i < len(list); i++ {
			if c := list[i]; c.kept != nil && c.kept.who == who && c.kept.w == w {
				list = append(list[:i], list[i+1:]...)
				i--
			}
		}
	}
	mine := func(c *conn) bool { return len(c.who) == 0 || len(c.who) == 1 && c.who[who] }
	if w.kind == "ch" && f.oneChannel || w.kind == "vod" && f.oneItem {
		wait, why, k := false, map[string]string{"ch": "channel", "vod": "item"}[w.kind], f.base+" "+w.kind+" "+w.item
		if cl, ok := claims[k]; ok && cl.who != who && now-cl.at < 3*grace && !follow { // (its device's new one is on its way)
			return refuseNow, why
		}
		for _, c := range list {
			switch {
			case !same(c.w, w):
			case c.cutAt > 0:
				wait = wait || now-c.cutAt < grace
			case mine(c): // the same device again (rewinding, seeking): its old connection makes room
				c.end()
				claims[k] = claim{who, now}
				wait = true
			default:
				return refuseNow, map[string]string{"ch": "channel", "vod": "item"}[w.kind]
			}
		}
		if wait {
			return waitNow, ""
		}
	}
	if lim := f.limit(); lim > 0 {
		// What makes room first: the device's own read of the same thing (a seek, a rewind), then a session nobody
		// asks for, then the newest of the device's own (a device asking for something else is most often
		// leaving what it opened last, and a media server that records and serves a viewer keeps its recording).
		first := func(c, d *conn) bool {
			switch {
			case w.kind != "" && (c.w == w) != (d.w == w):
				return c.w == w
			case c.idle != d.idle:
				return c.idle
			}
			return c.since > d.since
		}
		n, closing := 0, false
		var room *conn
		for _, c := range list {
			if c.cutAt > 0 {
				if now-c.cutAt < grace {
					n++
					closing = true
				}
				continue
			}
			n++
			if (mine(c) || c.idle) && (room == nil || first(c, room)) {
				room = c
			}
		}
		if n >= lim {
			switch {
			case room != nil: // this device's own connection (or a session nobody asks for) makes room
				room.end()
				return waitNow, ""
			case closing:
				return waitNow, ""
			}
			return refuseNow, "limit"
		}
	}
	return admitNow, ""
}

// claim: a channel or an item whose device's connection was cut to make room for that device's new one: no
// other device takes it in the moment between the two. (mu)
type claim struct {
	who string
	at  float64
}

var claims = map[string]claim{} // provider, kind and item -> the device whose new connection is on its way

// admit: a new provider connection for device who, by the rules this provider has on. Returns the hold that
// counts it until it is released, or nil and why not ("" when the player hung up while it waited). key: the
// channel or session it opens (a second request for it joins: nothing is checked). cut: what ends it.
func (f *front) admit(ctx context.Context, who string, w what, follow bool, key string, cut func(), addr ...string) (*hold, string) {
	h := &hold{f: f, who: who, w: w, key: key, cut: cut}
	if len(addr) > 0 {
		h.addr = addr[0]
	}
	defer func() { // (admitted, refused or given up: its claim is over)
		mu.Lock()
		if k := f.base + " " + w.kind + " " + w.item; claims[k].who == who {
			delete(claims, k)
		}
		mu.Unlock()
	}()
	settle := 0.0 // after a connection was cut: the provider gets dropGap to see it close
	for began := mono(); ; {
		mu.Lock()
		if key != "" && open(key) || isLoop(who) { // (the relay's own bridge: its reader was admitted)
			h.shared = true
		}
		for o := range holds {
			if key != "" && o.key == key && !o.shared {
				h.shared = true
			}
			// A player reads one catch-up file in several requests at once (its main read, the end of the file
			// for its length, a seek). The same device's reads of the same address join the one admitted rather
			// than cut it: the provider lets one device read its archive file several ways at once, and cutting
			// broke the player's own reads. Another device, another time (a rewind) or a live channel: as before.
			if key == "" && w.kind == "ch" && h.addr != "" && o.f == f && o.key == "" && o.addr == h.addr &&
				o.who == who && same(o.w, w) && o.cutAt == 0 && o.until == 0 {
				h.shared = true
			}
		}
		v, why := admitNow, ""
		if !h.shared {
			v, why = f.decide(who, w, follow, mono())
		}
		switch {
		case v == waitNow && mono()-began > 3*grace: // (never for long: what was cut is not counted after grace)
			v, why = refuseNow, "limit"
		case v == waitNow:
			settle = mono() + dropGap
		case v == admitNow && mono() < settle:
			v = waitNow
		}
		if v == admitNow {
			h.at = mono()
			holds[h] = true
			if follow {
				f.keepFor(who, w, 0) // (the place kept for it is taken)
			}
			mu.Unlock()
			return h, ""
		}
		mu.Unlock()
		if v == refuseNow {
			return nil, why
		}
		if sleep(ctx, 0.05) != nil {
			return nil, ""
		}
	}
}

// unOpen: a channel refused by the rules was not opened after all: the channel opened before it stays the last
// one opened (a stream the provider ends is asked for again only when no other channel was opened since).
func unOpen(provider, key string, was opened) {
	mu.Lock()
	if lastOpen[provider].key == key {
		lastOpen[provider] = was
	}
	mu.Unlock()
}

// ---------------------------------------------------------------------------
// Refusing, in plain words
// ---------------------------------------------------------------------------

var turnedAway = map[string]float64{} // provider, device and reason -> when it was last logged

// refusal: the answer for a request the rules turned away, logged once a minute per device and reason.
func (f *front) refusal(who string, w what, why string) *refused {
	now := mono()
	mu.Lock()
	lim, left := f.limit(), lockLeft(f.locked, f.lockWait, vpnChanged, now)
	k := f.base + " " + who + " " + why
	said := turnedAway[k]
	if said == 0 || now-said > 60 {
		turnedAway[k] = now
		if len(turnedAway) > 300 {
			for x, t := range turnedAway {
				if now-t > 60 {
					delete(turnedAway, x)
				}
			}
		}
	}
	mu.Unlock()
	var line, text string
	after := 0
	switch why {
	case "lock":
		after = int(math.Ceil(left))
		line = fmt.Sprintf("the VPN changed address a moment ago, and this provider locks the account to one address; new streams wait %d s more", after)
		text = fmt.Sprintf("The VPN changed address a moment ago. Your provider locks your account to one address,\n"+
			"so new streams wait until it has let go of the old one. Try again in %d seconds.\n", after)
	case "channel":
		line = fmt.Sprintf("channel %s already has a stream open at the provider, which allows one per channel", w.item)
		text = "This channel already has a stream open at your provider (someone is watching, recording or rewinding it).\n" +
			"Your provider allows one stream per channel, so this waits until that one is done.\n"
	case "item":
		line = "this movie or episode is playing on another device, and the provider allows one stream of each"
		text = "This movie or episode is playing on another device right now.\n" +
			"Your provider allows one stream of each, so this waits until that one is done.\n"
	default:
		line = fmt.Sprintf("all %d connections the account allows are in use", lim)
		text = fmt.Sprintf("All %d connections your provider allows are in use right now.\n", lim)
		if lim == 1 {
			line = "the one connection the account allows is in use"
			text = "The one connection your provider allows is in use right now.\n"
		}
		text += "Coaxial turned this down so nobody who is watching or recording gets cut off. Try again when something stops.\n"
	}
	if said == 0 || now-said > 60 {
		say("%s was turned away (%s): %s", who, notPrintable.ReplaceAllString(f.name, ""), line)
	}
	return &refused{status: 503, kind: "text/plain; charset=utf-8", text: []byte(text), after: after}
}

// ---------------------------------------------------------------------------
// Redirects of an admitted request: the address a player is sent on to carries on what it was admitted for
// ---------------------------------------------------------------------------

type followUp struct {
	who string
	w   what
	at  float64
}

var followUps = map[string]*followUp{} // port and the address given to the player -> what it continues

// followUpHold: seconds the place of a request answered with a redirect is kept for its device's request to the
// address it was sent on to: another device taking it in between would leave the follow-up refused. (mu)
var followUpHold = 10.0

// remember: a redirect of an admitted request, to the address the player was given. (mu held)
func (f *front) remember(moved string, h *hold) {
	now := mono()
	if len(followUps) > 500 {
		for k, fu := range followUps {
			if now-fu.at > 600 {
				delete(followUps, k)
			}
		}
	}
	followUps[f.port+" "+moved] = &followUp{h.who, h.w, now}
	h.moved = true // (its place is kept for the follow-up once this request is over: see release)
	h.loc = moved
}

// ---------------------------------------------------------------------------
// Catch-up asked for again by the same device: one ask at the provider
// ---------------------------------------------------------------------------

// A provider ends a device's catch-up read when the device asks for catch-up afresh (measured), and a player asks
// the same catch-up address several times at once when it opens it. So the same device's asks for the same
// address within cuAskKeep seconds get the provider's one answer (the redirect to its archive file), as a player
// that keeps the address it was sent to would use it; asks that come while the first is on its way wait for it.
type cuAsk struct {
	loc  string
	at   float64
	done chan struct{}
}

var cuAsks = map[string]*cuAsk{} // port, device and address -> the provider's answer to it

const cuAskKeep = 30.0

// cuAsked: the redirect a catch-up ask by this device was just given, if any (waiting for one on its way as long
// as the player does: it comes when the provider's answer to the first ask arrives). Otherwise it is marked on
// its way, and finish must be called once with what the provider answered.
func (f *front) cuAsked(ctx context.Context, key string) (loc string, finish func(string)) {
	for {
		mu.Lock()
		now := mono()
		for k, a := range cuAsks {
			if a.at > 0 && now-a.at > cuAskKeep {
				delete(cuAsks, k)
			}
		}
		a := cuAsks[key]
		if a == nil {
			a = &cuAsk{done: make(chan struct{})}
			cuAsks[key] = a
			mu.Unlock()
			return "", func(loc string) {
				mu.Lock()
				a.loc, a.at = loc, mono()
				if loc == "" {
					delete(cuAsks, key)
				}
				mu.Unlock()
				close(a.done)
			}
		}
		if a.at > 0 && a.loc != "" {
			loc := a.loc
			mu.Unlock()
			return loc, nil
		}
		done := a.done
		mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return "", nil
		}
	}
}

// keepFor: the place kept for device who's follow-up of w, until then (0: none any more). (mu held)
func (f *front) keepFor(who string, w what, until float64) {
	for o := range holds {
		if o.f != f || o.who != who || o.w != w {
			continue
		}
		if o.until > 0 {
			delete(holds, o)
		} else if until == 0 {
			o.moved = false // (its follow-up came while it was still open: nothing is kept for it)
		}
	}
	if until > 0 {
		holds[&hold{f: f, who: who, w: w, at: mono(), until: until}] = true
	}
}

// followed: what this request continues (a redirect of an admitted one, asked for by the same device), if anything.
func (f *front) followed(r *http.Request) (what, bool) {
	mu.Lock()
	defer mu.Unlock()
	fu := followUps[f.port+" "+r.URL.RequestURI()]
	if fu == nil || fu.who != whoIs(r) {
		return what{}, false
	}
	fu.at = mono()
	return fu.w, true
}

// ---------------------------------------------------------------------------
// A movie or an episode, no faster than set
// ---------------------------------------------------------------------------

// speed: a rate in bytes a second shared by the reads given it, at most a second's worth ahead. Time nobody
// read (a paused player) is not saved up: after it, the rate holds again at once. A film or an episode passed
// on has one of its own; the parts of a film's HLS that one device reads share one (see noteFilm).
type speed struct {
	mu    sync.Mutex
	rate  float64
	began time.Time
	sent  float64
	who   string // (the parts of a film's HLS: the device)
}

// throttle reads at speed p.
type throttle struct {
	r   io.Reader
	ctx context.Context
	p   *speed
}

func (t *throttle) Read(b []byte) (int, error) {
	p := t.p
	want := math.Min(float64(len(b)), 16384)
	for {
		p.mu.Lock()
		if p.began.IsZero() {
			p.began = time.Now()
		}
		now := time.Since(p.began).Seconds()
		p.sent = math.Max(p.sent, p.rate*now)
		allowed := p.rate*(now+1) - p.sent
		if allowed >= want {
			b = b[:int(math.Min(float64(len(b)), allowed))]
			p.sent += float64(len(b)) // (taken before reading, so reads side by side share the rate)
			p.mu.Unlock()
			break
		}
		p.mu.Unlock()
		if err := sleep(t.ctx, (want-allowed)/p.rate); err != nil {
			return 0, err
		}
	}
	n, err := t.r.Read(b)
	p.mu.Lock()
	p.sent -= float64(len(b) - n) // (what did not come is given back)
	p.mu.Unlock()
	return n, err
}

// filmParts: the addresses a film's HLS playlists named (pieces, other qualities, audio, keys), by port, device
// and address as the player asks for them, and the film itself by port, device and item: the speed that device
// reads the film at. Kept an hour after last use. (mu)
type filmPart struct {
	p  *speed
	at float64
}

var filmParts = map[string]*filmPart{}

var uriAttr = regexp.MustCompile(`URI="([^"]*)"`)

// filmSpeed: the speed a playlist request reads at: a part of a film's HLS, or the film w itself (nil: neither,
// or the option is off).
func (f *front) filmSpeed(who string, r *http.Request, w what) *speed {
	if f.movieRate <= 0 || isLoop(who) {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	now := mono()
	for _, k := range []string{f.port + " " + who + " " + r.URL.RequestURI(), f.port + " " + who + " " + w.item} {
		if part := filmParts[k]; part != nil {
			part.at = now
			return part.p
		}
	}
	if w.kind != "vod" {
		return nil
	}
	p := &speed{rate: f.movieRate, who: who}
	filmParts[f.port+" "+who+" "+w.item] = &filmPart{p, now}
	return p
}

// noteFilm: a film's playlist as given to its device: every address it names is read at the film's speed p.
func (f *front) noteFilm(text string, p *speed) {
	now := mono()
	mu.Lock()
	defer mu.Unlock()
	if len(filmParts) > 20000 {
		for k, part := range filmParts {
			if now-part.at > 3600 {
				delete(filmParts, k)
			}
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		refs := []string{line}
		if strings.HasPrefix(line, "#") {
			refs = nil
			for _, m := range uriAttr.FindAllStringSubmatch(line, -1) {
				refs = append(refs, m[1])
			}
		}
		for _, ref := range refs {
			if ref != "" {
				filmParts[f.port+" "+p.who+" "+ref] = &filmPart{p, now}
			}
		}
	}
}

// speedOf: the speed a request passed on is read at: a part of a film's HLS, its film's; a film or an episode
// admitted under hd, one of its own (nil: not slowed).
func (f *front) speedOf(r *http.Request, hd *hold) *speed {
	if f.movieRate <= 0 {
		return nil
	}
	mu.Lock()
	part := filmParts[f.port+" "+whoIs(r)+" "+r.URL.RequestURI()]
	if part != nil {
		part.at = mono()
	}
	mu.Unlock()
	switch {
	case part != nil:
		return part.p
	case hd != nil && hd.w.kind == "vod":
		return &speed{rate: f.movieRate}
	}
	return nil
}
