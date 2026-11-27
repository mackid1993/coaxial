// Picking the VPN server. When the settings name places (countries, regions or cities) but no server,
// gluetun picks one of the matching servers at random, near or far, busy or not. So Coaxial pings every
// matching server (probe.go) and keeps the VPN on the closest healthy one. A server named in the settings
// is never overruled.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// picked: the server chosen, and for which settings.
type picked struct {
	Key    string  `json:"key"`
	Env    string  `json:"env"` // SERVER_NAMES=... or SERVER_HOSTNAMES=...
	Name   string  `json:"name"`
	Of     int     `json:"of"` // how many were pinged
	At     string  `json:"at"`
	Median float64 `json:"median_ms,omitempty"` // its ping when chosen
	Loss   float64 `json:"loss,omitempty"`
}

type candidate struct {
	name, env string
	ips       []string
}

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
		env["SERVER_CITIES"] == "" { // only within cities: a country or a region can hold thousands of servers
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

// candidatesFrom: the service's servers that fit the settings, every one of them, each with its addresses.
// gluetun's own rules are kept (a place, the connection type, and every "..._ONLY" setting that is on);
// beyond those, the kinds of server that are slower by design (secure core, Tor, free, multihop) are left
// out unless the settings ask for them.
func candidatesFrom(list []serverEntry, env map[string]string) []candidate {
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
	at := map[string]int{}
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
		c := candidate{name: s.ServerName, env: "SERVER_NAMES=" + s.ServerName}
		if s.ServerName == "" {
			c = candidate{name: s.Hostname, env: "SERVER_HOSTNAMES=" + s.Hostname}
		}
		if !ok || c.name == "" || strings.ContainsAny(c.name, ",\n") {
			continue
		}
		if i, seen := at[c.env]; seen { // one entry per server, with every address gluetun may use for it
			out[i].ips = append(out[i].ips, s.IPs...)
			continue
		}
		c.ips = append([]string(nil), s.IPs...)
		at[c.env] = len(out)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
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

// someonePlaying: the last look before the VPN is stopped by itself (a server move, an update). Only a proxy
// part that answers and says nothing plays lets it go on. One that does not answer (stopped, being started again
// after a crash, too busy) may have viewers and recordings on their way back to it, and watchProgram's own look
// already counts it as something playing (nothingPlaying): the move or update waits for the next quiet spell.
func someonePlaying() bool { return !nothingPlaying() }

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

// pickStatus: for the status page. possible: the settings leave the server to Coaxial.
func (l *launcher) pickStatus() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.set == nil {
		return nil
	}
	env := l.set.vpnEnv()
	key := pickKey(env)
	possible, why := key != "", ""
	switch service := strings.ToLower(strings.TrimSpace(env["VPN_SERVICE_PROVIDER"])); {
	case possible:
	case service == "" || service == "custom":
		return nil
	case env["SERVER_NAMES"] != "" || env["SERVER_HOSTNAMES"] != "":
		why = "named" // a server is named in the settings: it is used as it is
	default:
		why = "city" // no city is picked
	}
	st := map[string]any{"name": "", "at": "", "switching": l.pickBusy, "checking": l.probing || l.probeAsked,
		"possible": possible, "why_not": why, "locked": l.set.addressLocked(), "count": len(l.probeList)}
	if l.pick.Key == key && l.pick.Env != "" {
		st["name"], st["at"] = l.pick.Name, l.pick.At
	}
	if !l.probeAt.IsZero() && l.probeKey == key {
		st["checked_at"] = l.probeAt.UTC().Format(time.RFC3339)
		for _, p := range l.probeList {
			if p.env == l.pick.Env {
				st["now"] = p
			}
		}
		if len(l.probeList) > 0 {
			st["best"] = l.probeList[0].Name
		}
	}
	if l.better != nil {
		st["better"] = map[string]any{"name": l.better.Name, "why": l.betterWhy, "median_ms": l.better.Median, "ready": l.betterReady()}
	}
	if l.pickBusy && l.better != nil {
		st["to"] = l.better.Name
	}
	return st
}

// apiPick: "check the servers now", from the page. Pinging cuts nothing, so it only asks for a round now;
// it answers at once and the round shows on the status page.
func (l *launcher) apiPick(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]any{"ok": false})
		return
	}
	l.mu.Lock()
	s := l.set
	l.mu.Unlock()
	if s == nil || pickKey(s.vpnEnv()) == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "message": "Pick a city and leave both server boxes empty. Then Coaxial can choose the server."})
		return
	}
	l.mu.Lock()
	l.probeAsked = true
	l.mu.Unlock()
	go l.probeTick(0)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// apiSwitch: "switch now", from the page: moves to the better server even if something is playing.
func (l *launcher) apiSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]any{"ok": false})
		return
	}
	l.mu.Lock()
	better, busy := l.better, l.pickBusy
	l.mu.Unlock()
	switch {
	case busy:
		writeJSON(w, 200, map[string]any{"ok": true, "message": "Already moving to another server."})
	case better == nil:
		writeJSON(w, 200, map[string]any{"ok": false, "message": "There is no better server to move to right now."})
	default:
		go l.switchServer(true)
		writeJSON(w, 200, map[string]any{"ok": true})
	}
}
