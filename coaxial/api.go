// What the launcher asks the proxy part for the web interface: which providers answer, and what is playing.
// Asked over a socket inside the container, never on the network.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

func insideStatus(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		mu.Lock()
		n := len(fronts)
		mu.Unlock()
		if n == 0 {
			answer(w, 200, "", "no provider is running\n")
		} else {
			answer(w, 200, "", "ok\n")
		}
	case "/status":
		data, _ := json.Marshal(statusData())
		answer(w, 200, "application/json", string(data))
	case "/vpn-changed": // the launcher saw the VPN come back up: the address may be new (see locked_to_address)
		mu.Lock()
		vpnChanged = mono()
		lastIP = "" // (the next look-up starts from the new address: one change starts one wait)
		locked := false
		for _, f := range fronts {
			locked = locked || f.locked
		}
		mu.Unlock()
		if locked {
			say("the VPN reconnected; providers that lock an account to one address get new streams after a short wait")
		}
		answer(w, 200, "", "ok\n")
	case "/log-level": // the log detail was changed in the web interface: applied at once, nothing restarts
		if to := r.URL.Query().Get("to"); validLogLevel(to) {
			setLogLevel(to)
			answer(w, 200, "", "ok\n")
		} else {
			answer(w, 400, "", "")
		}
	case "/playing": // what plays, and nothing else (see playingNow)
		data, _ := json.Marshal(map[string]any{"playing": playingNow()})
		answer(w, 200, "application/json", string(data))
	default:
		answer(w, 404, "", "")
	}
}

// statusData: the same facts the status page shows. Nothing here waits long: what is not known yet is
// asked for in the background and is there the next time.
func statusData() map[string]any {
	go whereAmI()
	go kept("ping", 30, func(context.Context) (any, error) {
		if ms, ok := vpnPing(); ok {
			return ms, nil
		}
		return nil, errors.New("no answer")
	})
	var ports []string
	mu.Lock()
	for port := range fronts {
		ports = append(ports, port)
	}
	mu.Unlock()
	sort.Slice(ports, func(i, j int) bool { return atoi(ports[i]) < atoi(ports[j]) })
	accts := map[string]map[string]any{}
	for _, port := range ports {
		mu.Lock()
		st := accounts[port]
		fresh := st == nil || (st.good == nil && st.last == nil)
		mu.Unlock()
		wait := time.Duration(0)
		if fresh { // the first look waits a moment for the provider's answer
			wait = 1500 * time.Millisecond
		}
		accts[port] = account(port, wait)
		names(port)
	}
	mu.Lock()
	defer mu.Unlock()
	now := mono()
	for k, t := range hlsSeen {
		if now-t > 30 {
			delete(hlsSeen, k)
		}
	}
	provs, playing, nameOf := []any{}, []any{}, map[string]string{}
	for _, port := range ports {
		f := fronts[port]
		nameOf[port] = f.name
		open := 0
		for _, c := range channels {
			if c.provider == f.base && strings.HasPrefix(c.key, "/ts ") {
				open++
			}
		}
		for k := range hlsSeen {
			if strings.HasPrefix(k, port+"/") {
				open++
			}
		}
		a := accts[port]
		up := a != nil && a["answer"] == 200 && pyStr(a["auth"]) != "0"
		provs = append(provs, map[string]any{"name": f.name, "port": atoi(port), "up": up, "live_tv": f.mode, "channels_open": open,
			"connections_missing": f.bought == 0}) // (no number typed in: the limit is not kept, the page says so)
	}
	row := func(who, id, port, kind string, seconds float64, written int64) {
		name := ""
		if n := channelNames[port]; n != nil {
			name = n.names[id]
		}
		playing = append(playing, map[string]any{"device": who, "channel_id": id, "channel_name": name, "provider": nameOf[port],
			"format": kind, "seconds": int64(math.Max(0, seconds)), "mb_sent": math.Round(float64(written)/1048576*10) / 10})
	}
	for _, c := range channels {
		for rd := range c.readers {
			if rd.kind != "HLS built from MPEG-TS" { // (its players have rows of their own, below)
				row(rd.who, rd.id, rd.port, rd.kind, now-rd.came, rd.written.Load())
			}
		}
	}
	for _, h := range hlsRows {
		if now-h.last <= 30 {
			row(h.who, h.id, h.port, "HLS", h.last-h.first, h.bytes)
		}
	}
	sort.Slice(playing, func(i, j int) bool {
		a, b := playing[i].(map[string]any), playing[j].(map[string]any)
		return a["device"].(string)+a["channel_id"].(string) < b["device"].(string)+b["channel_id"].(string)
	})
	out := map[string]any{"providers": provs, "playing": playing, "city": "", "country": "", "ip": "", "network": "", "ping_ms": 0}
	if k := keptValues["where"]; k != nil && k.value != nil {
		out["city"], out["country"] = pyStr(orEmpty(field(k.value, "city"))), pyStr(orEmpty(field(k.value, "country")))
		out["ip"] = pyStr(orEmpty(field(k.value, "ip")))
		out["network"] = pyStr(orEmpty(field(k.value, "org")))
	}
	if k := keptValues["ping"]; k != nil && k.value != nil {
		if ms, ok := k.value.(float64); ok {
			out["ping_ms"] = int(math.Round(ms))
		}
	}
	return out
}

// films: devices sent a film, an episode or catch-up through the pass-through: how many such answers are under
// way, and when the last one ended. A player often asks for a film in parts, one after another.
type film struct {
	open int
	last float64
}

var films = map[string]*film{}

// watching notes a film answer to who; the function it returns notes its end.
func watching(who string) func() {
	mu.Lock()
	defer mu.Unlock()
	f := films[who]
	if f == nil {
		f = &film{}
		films[who] = f
	}
	f.open++
	return func() {
		mu.Lock()
		defer mu.Unlock()
		f.open, f.last = f.open-1, mono()
	}
}

// playingNow: who is playing, counted as statusData counts them, without asking anyone anything. The launcher
// asks this every 20 s, day and night; /status would ask each provider for its account each time, and for its
// channel list every minute while that fails. That is for the page, while someone looks at it. A film, an
// episode or catch-up counts too (not on the page), until a minute after its last part was sent: the VPN must
// not be restarted under it for an update or a server check.
func playingNow() []any {
	mu.Lock()
	defer mu.Unlock()
	now, out := mono(), []any{}
	for who, f := range films {
		if f.open > 0 || now-f.last <= 60 {
			out = append(out, map[string]any{"device": who})
		} else {
			delete(films, who)
		}
	}
	for _, c := range channels {
		for rd := range c.readers {
			if rd.kind != "HLS built from MPEG-TS" { // (its players are counted below)
				out = append(out, map[string]any{"device": rd.who})
			}
		}
	}
	for _, h := range hlsRows {
		if now-h.last <= 30 {
			out = append(out, map[string]any{"device": h.who})
		}
	}
	return out
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}
