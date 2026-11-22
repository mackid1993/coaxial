// Picking the VPN server. When the settings name places (countries, regions or cities) but no server,
// gluetun picks one of the matching servers at random, fast or not. So Coaxial tries them: with nothing
// playing, it connects to each in turn, measures it for a few seconds, and stays on the best. Once an hour,
// again only with nothing playing, it measures the one in use and looks for a better one if that got worse.
// A server named in the settings is never overruled.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	pickMost    = 8                                                    // servers tried in one go, at most
	pickEvery   = time.Hour                                            // the server in use is measured again this often
	pickSlow    = 50.0                                                 // the server in use is "slow" below this many Mbit/s (about ten streams)...
	pickShare   = 0.2                                                  // ...or below this share of the speed it was chosen at
	pickUpWait  = 8 * time.Second                                      // a server whose tunnel passes nothing by then is passed over
	pickSettle  = 14 * time.Second                                     // ...and one that cannot be measured by then, counted from its start
	pickSeconds = 1.5                                                  // how long each one is measured, at most
	pickURL     = "https://speed.cloudflare.com/__down?bytes=50000000" // what is downloaded through the VPN to measure it
)

// picked: the server chosen, and for which settings.
type picked struct {
	Key  string  `json:"key"`
	Env  string  `json:"env"` // SERVER_NAMES=... or SERVER_HOSTNAMES=...
	Name string  `json:"name"`
	Mbps float64 `json:"mbps"`
	OFO  float64 `json:"out_of_order"`
	Of   int     `json:"of"` // how many were tried
	At   string  `json:"at"`
	// Unfinished: someone started playing before all of them were tried. The server is used (on a restart
	// too); the next quiet time tries them again.
	Unfinished bool `json:"unfinished,omitempty"`
}

type candidate struct{ name, env string }

func isOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "yes", "1":
		return true
	}
	return false
}

// pickKey: the settings a choice of server depends on; "" when no server is to be picked (one is named,
// no city is named, or the service is the user's own config).
func pickKey(env map[string]string) string {
	service := strings.ToLower(strings.TrimSpace(env["VPN_SERVICE_PROVIDER"]))
	if service == "" || service == "custom" || env["SERVER_NAMES"] != "" || env["SERVER_HOSTNAMES"] != "" ||
		env["SERVER_CITIES"] == "" { // only within a city: a country or a region can hold hundreds of servers
		return ""
	}
	key := []string{service, strings.ToLower(env["VPN_TYPE"]), env["SERVER_COUNTRIES"], env["SERVER_REGIONS"], env["SERVER_CITIES"]}
	var only []string
	for k, v := range env {
		if strings.HasSuffix(k, "_ONLY") && isOn(v) {
			only = append(only, k)
		}
	}
	sort.Strings(only)
	return strings.ToLower(strings.Join(append(key, only...), "|"))
}

// candidatesFrom: the service's servers that fit the settings. gluetun's own rules are kept (a place, the
// connection type, and every "..._ONLY" setting that is on); beyond those, the kinds of server that are
// slower by design (secure core, Tor, free, multihop) are left out unless the settings ask for them.
func candidatesFrom(list []serverEntry, env map[string]string, keep ...string) []candidate {
	in := func(key string) map[string]bool {
		m := map[string]bool{}
		for _, s := range strings.Split(env[key], ",") {
			if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
				m[s] = true
			}
		}
		return m
	}
	countries, regions, cities := in("SERVER_COUNTRIES"), in("SERVER_REGIONS"), in("SERVER_CITIES")
	kind := strings.ToLower(strings.TrimSpace(env["VPN_TYPE"]))
	if kind == "" {
		kind = "openvpn" // gluetun's own default
	}
	seen := map[string]bool{}
	var out []candidate
	for _, s := range list {
		special := []struct {
			has bool
			key string
		}{{s.SecureCore, "SECURE_CORE_ONLY"}, {s.Tor, "TOR_ONLY"}, {s.Free, "FREE_ONLY"}, {s.MultiHop, "MULTIHOP_ONLY"}}
		wanted := []struct {
			has bool
			key string
		}{{s.Stream, "STREAM_ONLY"}, {s.PortForward, "PORT_FORWARD_ONLY"}, {s.Owned, "OWNED_ONLY"}}
		ok := (s.VPN == "" || strings.ToLower(s.VPN) == kind) &&
			(len(countries) == 0 || countries[strings.ToLower(s.Country)]) &&
			(len(regions) == 0 || regions[strings.ToLower(s.Region)]) &&
			(len(cities) == 0 || cities[strings.ToLower(s.City)])
		for _, f := range special {
			ok = ok && f.has == isOn(env[f.key])
		}
		for _, f := range wanted {
			ok = ok && (f.has || !isOn(env[f.key]))
		}
		c := candidate{s.ServerName, "SERVER_NAMES=" + s.ServerName}
		if s.ServerName == "" {
			c = candidate{s.Hostname, "SERVER_HOSTNAMES=" + s.Hostname}
		}
		if !ok || c.name == "" || seen[c.env] || strings.ContainsAny(c.name, ",\n") {
			continue
		}
		seen[c.env] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	if len(out) > pickMost { // a different handful each time, so that all of them get their turn
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		for i, c := range out { // ...but the one in use is always among them: it is what the others must beat
			if len(keep) > 0 && c.env == keep[0] && i >= pickMost {
				out[pickMost-1] = c
			}
		}
		out = out[:pickMost]
		sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	}
	return out
}

func pickFile(dir string) string { return filepath.Join(dir, "gluetun", "pick.json") }

// pickOverride: the server to name to gluetun for this start of the VPN ("" for none). (l.work held)
func (l *launcher) pickOverride(s *settings) string {
	key := pickKey(s.vpnEnv())
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.pickLoaded {
		l.pickLoaded = true
		data, _ := os.ReadFile(pickFile(l.dir))
		json.Unmarshal(data, &l.pick)
	}
	switch {
	case key == "":
		return ""
	case l.pickTry != "":
		return l.pickTry
	case l.pick.Key == key && (strings.HasPrefix(l.pick.Env, "SERVER_NAMES=") || strings.HasPrefix(l.pick.Env, "SERVER_HOSTNAMES=")):
		return l.pick.Env
	}
	return ""
}

// tcpCounters: segments received, and how many of them came out of order, since the container began.
func tcpCounters() (in, ofo int64) {
	read := func(file, group, name string) int64 {
		f, err := os.Open(file)
		if err != nil {
			return 0
		}
		defer f.Close()
		var head []string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) == 0 || fields[0] != group {
				continue
			}
			if head == nil {
				head = fields
				continue
			}
			for i, h := range head {
				if h == name && i < len(fields) {
					n, _ := strconv.ParseInt(fields[i], 10, 64)
					return n
				}
			}
			head = nil
		}
		return 0
	}
	return read("/proc/net/snmp", "Tcp:", "InSegs"), read("/proc/net/netstat", "TcpExt:", "TCPOFOQueue")
}

// measure downloads through the VPN for a moment: megabits a second, and the share of packets that came out
// of order. With addr (the test server's address, looked up before), it needs no DNS, so it works the moment
// the tunnel does: gluetun's own DNS is ready only seconds later.
var measure = func(addr string, from net.IP) (mbps, ofo float64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", pickURL, nil)
	req.Header.Set("User-Agent", "")
	tr := &http.Transport{DisableKeepAlives: true, Proxy: nil}
	if addr != "" {
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 3 * time.Second}
			if from != nil { // out through the tunnel and nothing else
				d.LocalAddr = &net.TCPAddr{IP: from}
			}
			return d.DialContext(ctx, network, net.JoinHostPort(addr, "443"))
		}
	}
	in0, ofo0 := tcpCounters()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("the test download answered %d", resp.StatusCode)
	}
	buf, got, start := make([]byte, 256<<10), int64(0), time.Now()
	for time.Since(start) < time.Duration(pickSeconds*float64(time.Second)) {
		n, err := resp.Body.Read(buf)
		got += int64(n)
		if err != nil {
			if err != io.EOF {
				return 0, 0, err
			}
			break
		}
	}
	took := time.Since(start).Seconds()
	in1, ofo1 := tcpCounters()
	if took < 0.05 || got == 0 {
		return 0, 0, fmt.Errorf("the test download gave nothing")
	}
	if in1 > in0 {
		ofo = float64(ofo1-ofo0) / float64(in1-in0)
	}
	return float64(got) * 8 / 1e6 / took, ofo, nil
}

// tunnelAddr: the address of the VPN's own interface (a point-to-point one that is up), nil while there is none.
func tunnelAddr() net.IP {
	list, _ := net.Interfaces()
	for _, i := range list {
		if i.Flags&net.FlagPointToPoint == 0 || i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				return n.IP.To4()
			}
		}
	}
	return nil
}

// tunnelPasses waits until a connection to addr can be made FROM THE TUNNEL'S OWN ADDRESS: the new tunnel is
// there and lets traffic through. (A plain connection proves nothing: in the moment between two runs of
// gluetun one can be made that the next run then cuts.) Returns the tunnel's address.
func tunnelPasses(addr string, within time.Duration) net.IP {
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		from := tunnelAddr()
		if from == nil {
			continue
		}
		d := &net.Dialer{Timeout: 700 * time.Millisecond, LocalAddr: &net.TCPAddr{IP: from}}
		if c, err := d.Dial("tcp", net.JoinHostPort(addr, "443")); err == nil {
			c.Close()
			return from
		}
	}
	return nil
}

// pickScore: speed alone. The share of packets out of order is logged but not counted: on a VPN tunnel under a
// fast download it is high on every server and says nothing about which is better.
func pickScore(mbps, ofo float64) float64 { return mbps }

// someonePlaying: true only when the proxy part answers and says so (while the VPN is being changed the
// proxy part is not running, and then nobody can be).
func someonePlaying() bool {
	var in struct{ Playing []any }
	data, ok := askProxy("/playing", 8*time.Second)
	return ok && json.Unmarshal(data, &in) == nil && len(in.Playing) > 0
}

func (l *launcher) waitUp(d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); {
		time.Sleep(time.Second)
		l.mu.Lock()
		up := l.vpn != nil && l.vpn.ever && l.vpnUp
		l.mu.Unlock()
		if up {
			return true
		}
	}
	return false
}

// pickServer tries the servers that fit the settings and stays on the best. It stops, staying where it is,
// as soon as someone starts playing.
func (l *launcher) pickServer(why string, asked bool) {
	l.mu.Lock()
	s, busy := l.set, l.pickBusy
	if busy && !asked { // (one run at a time; the page's button marks its own run before it starts it)
		l.mu.Unlock()
		return
	}
	l.pickBusy = true
	l.mu.Unlock()
	defer func() { l.mu.Lock(); l.pickBusy = false; l.mu.Unlock() }() // (on every way out)
	if s == nil {
		return
	}
	env := s.vpnEnv()
	key := pickKey(env)
	l.optionsNow()
	optionsMu.Lock()
	list := optionsServers[strings.ToLower(strings.TrimSpace(env["VPN_SERVICE_PROVIDER"]))]
	optionsMu.Unlock()
	l.mu.Lock()
	inUse := l.pick.Env
	l.mu.Unlock()
	cands := candidatesFrom(list, env, inUse)
	note := func(p picked) {
		p.Key, p.At = key, time.Now().UTC().Format(time.RFC3339)
		l.mu.Lock()
		l.pick, l.pickAt, l.pickTry = p, time.Now(), ""
		l.mu.Unlock()
		writeWhole(pickFile(l.dir), p)
	}
	if key == "" || len(cands) < 2 {
		if key != "" {
			note(picked{Of: len(cands)}) // nothing to choose between: gluetun's own choice stands
		}
		return
	}
	// The test server's address is looked up now, while the VPN's DNS works, so that each server can be
	// measured the moment its tunnel is up, without waiting for gluetun to finish starting.
	addr := ""
	if u, err := url.Parse(pickURL); err == nil {
		for try := 0; try < 10 && addr == ""; try++ {
			if try > 0 {
				time.Sleep(2 * time.Second) // (the VPN was just started: its DNS answers in a moment)
			}
			ips, _ := net.LookupHost(u.Hostname())
			for _, ip := range ips {
				if net.ParseIP(ip).To4() != nil {
					addr = ip
					break
				}
			}
		}
	}
	began := time.Now()
	say("VPN server: %s; trying %d servers that fit the settings", why, len(cands))
	var best picked
	tried, stopped := 0, false
	for _, c := range cands {
		if !asked && someonePlaying() { // (asked for by hand: it goes on, the page said what that costs)
			stopped = true
			break
		}
		l.mu.Lock()
		l.pickTry = c.env
		l.mu.Unlock()
		started := time.Now()
		l.apply()
		var from net.IP
		if addr != "" {
			from = tunnelPasses(addr, pickUpWait)
		}
		if addr != "" && from == nil || addr == "" && !l.waitUp(30*time.Second) {
			say("VPN server: %s did not connect and is passed over", c.name)
			continue
		}
		// gluetun goes on setting up for a few seconds after the tunnel is up, and cuts connections while it
		// does. So a server is measured again until one measurement goes through.
		mbps, ofo, err := measure(addr, from)
		for end := started.Add(pickSettle); err != nil && time.Now().Before(end); {
			time.Sleep(700 * time.Millisecond)
			if addr != "" {
				if from = tunnelAddr(); from == nil {
					continue
				}
			}
			mbps, ofo, err = measure(addr, from)
		}
		if err != nil {
			say("VPN server: %s could not be measured and is passed over: %s", c.name, plain(err))
			continue
		}
		tried++
		say("VPN server: %s: %.0f Mbit/s (%.0f s after its start; %.0f%% of packets out of order, not counted)", c.name, mbps, time.Since(started).Seconds(), ofo*100)
		if best.Env == "" || pickScore(mbps, ofo) > pickScore(best.Mbps, best.OFO) {
			best = picked{Env: c.env, Name: c.name, Mbps: mbps, OFO: ofo}
		}
	}
	best.Of = tried
	if stopped {
		// Someone is watching on the server being tried: it stays, and the rest wait for the next quiet time.
		l.mu.Lock()
		now := l.pickTry
		l.mu.Unlock()
		say("VPN server: someone started playing; the trying stops here and carries on when it is quiet again")
		if now != "" {
			_, name, _ := strings.Cut(now, "=")
			p := picked{Env: now, Name: name, Of: tried}
			if now == best.Env {
				p = best
			}
			p.Key, p.Unfinished, p.At = key, true, time.Now().UTC().Format(time.RFC3339)
			l.mu.Lock()
			l.pick, l.pickTry = p, "" // (kept as the choice for these settings only, by its key)
			l.mu.Unlock()
			writeWhole(pickFile(l.dir), p)
		}
		return
	}
	if best.Env == "" {
		l.mu.Lock()
		had := l.pick
		l.pickTry, l.pickAt = "", time.Now()
		l.mu.Unlock()
		if had.Key == key && had.Env != "" {
			say("VPN server: none of them could be measured this time; staying on %s, chosen before", had.Name)
		} else {
			say("VPN server: none of them could be measured; gluetun's own choice stands")
			note(picked{Of: 0})
		}
		l.apply()
		return
	}
	note(best)
	say("VPN server: staying on %s, the fastest of %d measured (%.0f Mbit/s); that took %.0f s", best.Name, tried, best.Mbps, time.Since(began).Seconds())
	l.apply() // (starts the VPN again only if the last one tried is not the best)
}

// pickDue: called once an hour of quiet (see watchProgram: nothing playing for 10 minutes, then an hour, then
// every hour until someone plays). Picks when no server is chosen for the settings yet; else measures the
// server in use and tries the others again only if it has become slow.
func (l *launcher) pickDue() {
	l.mu.Lock()
	s, p, try := l.set, l.pick, l.pickTry
	l.mu.Unlock()
	if s == nil {
		return
	}
	key := pickKey(s.vpnEnv())
	if key == "" {
		if try != "" || p.Env != "" { // a server is named in the settings now (or no place is): ours is dropped
			l.mu.Lock()
			l.pick, l.pickTry = picked{}, ""
			l.mu.Unlock()
			os.Remove(pickFile(l.dir))
		}
		return
	}
	if someonePlaying() {
		return
	}
	if p.Key != key || p.Unfinished {
		why := "no server is chosen for these settings yet"
		if p.Key == key {
			why = "the last run was cut short"
		}
		l.pickServer(why, false)
		return
	}
	if p.Env == "" { // (nothing to choose between for these settings: gluetun's own choice stands)
		return
	}
	// Every quiet hour the server in use is measured, without reconnecting. The others are tried again only
	// when it has become slow enough to matter: trying them takes the VPN down for a minute, and a recording
	// that starts in that minute would be missed. A server that is merely not the very fastest is kept.
	mbps, ofo, err := measure("", nil)
	if err != nil {
		time.Sleep(5 * time.Second)
		mbps, ofo, err = measure("", nil) // (once more before it is called bad)
	}
	l.mu.Lock()
	l.pickAt = time.Now()
	l.mu.Unlock()
	switch {
	case someonePlaying(): // someone started in the meantime: nothing is done
	case err != nil:
		l.pickServer(fmt.Sprintf("%s could not be measured (%s)", p.Name, plain(err)), false)
	case mbps < pickSlow || mbps < pickShare*p.Mbps:
		l.pickServer(fmt.Sprintf("%s has become slow (%.0f Mbit/s; it was %.0f when chosen)", p.Name, mbps, p.Mbps), false)
	default:
		say("VPN server: %s is still good (%.0f Mbit/s; %.0f%% of packets out of order, not counted)", p.Name, mbps, ofo*100)
	}
}

// pickStatus: for the status page. possible: the settings leave the server to Coaxial.
func (l *launcher) pickStatus() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.set == nil {
		return nil
	}
	env := l.set.vpnEnv()
	possible, why := pickKey(env) != "", ""
	switch service := strings.ToLower(strings.TrimSpace(env["VPN_SERVICE_PROVIDER"])); {
	case possible:
	case service == "" || service == "custom":
		return nil
	case env["SERVER_NAMES"] != "" || env["SERVER_HOSTNAMES"] != "":
		why = "named" // a server is named in the settings: it is used as it is
	default:
		why = "city" // no city is picked
	}
	return map[string]any{"name": l.pick.Name, "mbps": l.pick.Mbps, "out_of_order": l.pick.OFO, "of": l.pick.Of, "at": l.pick.At,
		"testing": l.pickBusy, "possible": possible, "why_not": why, "most": pickMost}
}

// apiPick: "find the fastest server now", asked for from the page. It answers at once; the trying goes on
// behind it and shows on the status page.
func (l *launcher) apiPick(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]any{"ok": false})
		return
	}
	l.mu.Lock()
	busy, s := l.pickBusy, l.set
	l.mu.Unlock()
	switch {
	case s == nil || pickKey(s.vpnEnv()) == "":
		writeJSON(w, 200, map[string]any{"ok": false, "message": "Pick a city and leave both server boxes empty. Then Coaxial can choose the server."})
	case busy:
		writeJSON(w, 200, map[string]any{"ok": true, "message": "Already trying the servers."})
	default:
		l.mu.Lock()
		l.pickBusy = true // (shown at once; pickServer sets and clears it too)
		l.mu.Unlock()
		go l.pickServer("asked for from the web page", true)
		writeJSON(w, 200, map[string]any{"ok": true})
	}
}
