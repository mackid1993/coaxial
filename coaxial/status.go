// The status page: what is set up, what is wrong, where the VPN comes out, and who is watching what.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func parseJSON(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	return v, d.Decode(&v)
}

func pyStr(v any) string { // str() as Python writes a JSON value
	switch v := v.(type) {
	case nil:
		return "None"
	case bool:
		return map[bool]string{true: "True", false: "False"}[v]
	case string:
		return v
	}
	return fmt.Sprint(v)
}

func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		f, _ := v.Float64()
		return f != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

func esc(v any) string { // html.escape(str(v), quote=True)
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace(pyStr(v))
}

func field(v any, k string) any { m, _ := v.(map[string]any); return m[k] }

// ---- answers from outside, kept so the page is quick (failures are retried after a minute)
type keptValue struct {
	at    float64
	value any
	busy  chan struct{}
}

var keptValues = map[string]*keptValue{}

func kept(name string, keep float64, ask func(context.Context) (any, error)) any {
	mu.Lock()
	k := keptValues[name]
	if k == nil {
		k = &keptValue{at: -1e9}
		keptValues[name] = k
	}
	limit := keep
	if k.value == nil {
		limit = 60
	}
	if k.busy == nil && mono()-k.at >= limit {
		k.busy = make(chan struct{})
		go func(done chan struct{}) {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			v, err := ask(ctx)
			cancel()
			if err != nil {
				v = nil
			}
			mu.Lock()
			k.at, k.value, k.busy = mono(), v, nil
			mu.Unlock()
			close(done)
		}(k.busy)
	}
	busy := k.busy
	mu.Unlock()
	if busy != nil {
		<-busy
	}
	mu.Lock()
	defer mu.Unlock()
	return k.value
}

func whereAmI() any { // what the internet sees, asked through the VPN (certificate checked)
	return kept("where", 300, func(ctx context.Context) (any, error) {
		r, err := fetch(ctx, "https://ipinfo.io/json", agentOf("curl/8"), opts{verify: true, wait: headerWait})
		if err != nil {
			return nil, err
		}
		data, err := readAll(r, 1<<20, 6)
		if err != nil || r.StatusCode != 200 {
			return nil, err
		}
		v, err := parseJSON(data)
		if ip := pyStr(field(v, "ip")); err == nil && truthy(field(v, "ip")) {
			sawAddress(ip)
		}
		return v, err
	})
}

var lastIP string // the public address last seen (mu; "" after the launcher said the VPN came back up)

// sawAddress: the public address the status page's look-up found. Another one than the last seen means the
// VPN came out somewhere else (see locked_to_address).
func sawAddress(ip string) {
	mu.Lock()
	defer mu.Unlock()
	if was := lastIP; was != "" && was != ip {
		vpnChanged = mono()
		say("the VPN's public address changed")
	}
	lastIP = ip
}

// vpnPing: round trip to the VPN server in ms. A packet allowed one hop dies there and its "time exceeded"
// lands in the socket's error queue (IP_RECVERR): no raw socket or privilege needed.
func vpnPing() (float64, bool) {
	var times []float64
	for n := 0; n < 3; n++ {
		func() {
			fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
			if err != nil {
				return
			}
			defer syscall.Close(fd)
			syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL, 1)
			syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_RECVERR, 1)
			syscall.SetNonblock(fd, true)
			start := mono()
			if syscall.Sendto(fd, []byte{0}, 0, &syscall.SockaddrInet4{Port: 33434 + n, Addr: [4]byte{1, 1, 1, 1}}) != nil {
				return
			}
			oob := make([]byte, 512)
			for mono()-start < 1 {
				_, oobn, _, _, err := syscall.Recvmsg(fd, make([]byte, 64), oob, syscall.MSG_ERRQUEUE)
				if err == syscall.EAGAIN {
					time.Sleep(2 * time.Millisecond)
					continue
				}
				notes, _ := syscall.ParseSocketControlMessage(oob[:oobn])
				for _, m := range notes {
					if err == nil && len(m.Data) > 5 && m.Data[4] == 2 && m.Data[5] == 11 { // ICMP time exceeded
						times = append(times, (mono()-start)*1000)
						break
					}
				}
				return
			}
		}()
	}
	if len(times) == 0 {
		return 0, false
	}
	sort.Float64s(times)
	return times[len(times)/2], true
}

// ---- the provider's account page, asked of the provider with its own login: at most every 15 s while the
// page is viewed; after failures 2, 4, 8... at most 15 minutes apart; the last good answer shown with its age.
type accountState struct {
	good, last map[string]any
	at, tried  float64
	fails      int
	task       chan struct{}
}

var accounts = map[string]*accountState{}

// keepAccounts reads every provider's account in the background: right away when Coaxial starts (again every 30
// seconds until it answers), then every 10 minutes. So the number of connections each account allows is known
// before anyone tunes, and the status page always has a fresh answer. One small request per provider each time; no
// stream is opened.
func keepAccounts(stop <-chan struct{}) {
	mu.Lock()
	ports := make([]string, 0, len(fronts))
	for port := range fronts {
		ports = append(ports, port)
	}
	mu.Unlock()
	due := map[string]time.Time{} // when each account is read next (one that answered: in accountEvery)
	for {
		every := accountEvery
		for _, port := range ports {
			if time.Now().Before(due[port]) {
				continue // (an account that answered is not read again because another one did not)
			}
			account(port, 25*time.Second) // (this round's answer, so one that answers is known at once)
			mu.Lock()
			st := accounts[port]
			known := st != nil && st.good != nil
			mu.Unlock()
			if known {
				due[port] = time.Now().Add(accountEvery)
			} else {
				every = accountRetry
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(every):
		}
	}
}

var accountEvery, accountRetry = 10 * time.Minute, 30 * time.Second

// backgroundAccounts: whether runProxy starts keepAccounts (tests of what a login teaches turn it off).
var backgroundAccounts = true

func playerAPI(ctx context.Context, port, extra string) (int, []byte, error) {
	f := fronts[port]
	if f == nil {
		return 0, nil, &refused{status: 502}
	}
	u := fmt.Sprintf("%s/player_api.php?username=%s&password=%s%s", f.base, url.QueryEscape(f.user), url.QueryEscape(f.pass), extra)
	ask := func() (int, []byte, error) {
		r, err := fetch(ctx, u, f.agent(agentOf("")), opts{guardAll: true, wait: headerWait})
		if err != nil {
			return 0, nil, err
		}
		data, err := readAll(r, 64<<20, headerWait)
		return r.StatusCode, data, err
	}
	status, data, err := ask()
	if err != nil || status == 502 { // most often a name lookup that failed for a moment
		time.Sleep(500 * time.Millisecond)
		status, data, err = ask()
	}
	return status, data, err
}

func account(port string, wait time.Duration) map[string]any {
	mu.Lock()
	st := accounts[port]
	if st == nil {
		st = &accountState{tried: -1e9}
		accounts[port] = st
	}
	every := 15.0
	if st.fails > 0 {
		every = math.Min(60*math.Pow(2, float64(st.fails)), 900)
	}
	if st.task == nil && mono()-st.tried >= every {
		st.tried, st.task = mono(), make(chan struct{})
		go func(done chan struct{}) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var value map[string]any
			if status, data, err := playerAPI(ctx, port, ""); err == nil && status != 200 {
				value = map[string]any{"answer": status}
			} else if err == nil {
				if v, err := parseJSON(data); err == nil {
					info := field(v, "user_info")
					value = map[string]any{"answer": 200, "status": field(info, "status"), "auth": field(info, "auth"),
						"exp": field(info, "exp_date"), "used": field(info, "active_cons"), "max": field(info, "max_connections")}
					if f := fronts[port]; f != nil {
						f.noteMax(field(info, "max_connections"))
					}
				}
			}
			mu.Lock()
			if value != nil && value["answer"] == 200 {
				st.good, st.at, st.fails, st.last = value, mono(), 0, nil
			} else {
				st.fails, st.last = st.fails+1, value
			}
			st.task = nil
			mu.Unlock()
			close(done)
		}(st.task)
	}
	task := st.task
	mu.Unlock()
	if task != nil { // an answer is on its way: wait a little for it
		select {
		case <-task:
		case <-time.After(wait):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if st.good != nil && mono()-st.at < 900 {
		a := map[string]any{"age": mono() - st.at}
		for k, v := range st.good {
			a[k] = v
		}
		return a
	}
	return st.last
}

// ---- channel names for the table: each provider's live list, kept 6 hours, fetched in the background
type nameList struct {
	at    float64
	names map[string]string
	busy  bool
}

var channelNames = map[string]*nameList{}

func names(port string) chan struct{} {
	mu.Lock()
	defer mu.Unlock()
	n := channelNames[port]
	if n == nil {
		n = &nameList{at: -1e9}
		channelNames[port] = n
	}
	done := make(chan struct{})
	if n.busy || (len(n.names) > 0 && mono()-n.at < 6*3600) || (len(n.names) == 0 && mono()-n.at < 60) {
		close(done)
		return done
	}
	n.busy = true
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		got := map[string]string{}
		if status, data, err := playerAPI(ctx, port, "&action=get_live_streams"); err == nil && status == 200 {
			var list []struct {
				ID   json.RawMessage `json:"stream_id"`
				Name string          `json:"name"`
			}
			json.Unmarshal(data, &list)
			for _, s := range list {
				got[strings.Trim(string(s.ID), `"`)] = s.Name
			}
		}
		mu.Lock()
		n.at, n.busy = mono(), false
		if len(got) > 0 {
			n.names = got
		}
		mu.Unlock()
	}()
	return done
}

const style = `
:root{--bg:#f5f6f8;--card:#fff;--ink:#16181d;--soft:#5b6270;--line:#e6e8ee;--chip:#eef0f5;--good:#12805c;--goodbg:#e7f6ef;
--bad:#c0362c;--badbg:#fdecea;--warn:#8a5a00;--warnbg:#fff4d6;--link:#2457d6;--shadow:0 1px 2px rgba(16,24,40,.06),0 4px 16px rgba(16,24,40,.06)}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#181b22;--ink:#eceef2;--soft:#9aa3b2;--line:#272b35;--chip:#232733;
--good:#4ad295;--goodbg:#11281f;--bad:#ff8a80;--badbg:#2b1614;--warn:#ffcf66;--warnbg:#2a2210;--link:#8ab0ff;--shadow:0 1px 2px rgba(0,0,0,.4)}}
*{box-sizing:border-box}html{-webkit-text-size-adjust:100%}
body{margin:0;background:var(--bg);color:var(--ink);font:17px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
main{max-width:860px;margin:0 auto;padding:28px 18px 48px}
header{display:flex;align-items:center;gap:12px;margin:0 0 18px}
header .dot{width:14px;height:14px;border-radius:50%;background:var(--good);box-shadow:0 0 0 5px var(--goodbg)}
header.down .dot{background:var(--bad);box-shadow:0 0 0 5px var(--badbg)}
h1{font-size:24px;letter-spacing:-.01em;margin:0;font-weight:700}
h2{font-size:13px;letter-spacing:.08em;text-transform:uppercase;color:var(--soft);margin:0 0 12px;font-weight:700}
.card{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:20px 22px;margin:14px 0;box-shadow:var(--shadow)}
.banner{border:0;font-size:20px;font-weight:700;display:flex;gap:12px;align-items:flex-start}
.banner small{display:block;font-size:16px;font-weight:500;margin-top:2px}
.good{background:var(--goodbg);color:var(--good)}.bad{background:var(--badbg);color:var(--bad)}.warn{background:var(--warnbg);color:var(--warn)}
.bad ul,.warn ul{font-size:16px;font-weight:500;margin:8px 0 0;padding-left:20px}
.pair{display:grid;grid-template-columns:max-content 1fr;gap:6px 18px;margin:0}
.pair dt{color:var(--soft)}.pair dd{margin:0;font-weight:600}
.provider{border-top:1px solid var(--line);padding:16px 0}.provider:first-of-type{border-top:0;padding-top:2px}.provider:last-of-type{padding-bottom:2px}
.provider .top{display:flex;flex-wrap:wrap;align-items:center;gap:8px 10px;margin-bottom:8px}
.provider .name{font-size:19px;font-weight:700;margin-right:4px}
.pill{display:inline-block;font-size:13px;font-weight:700;padding:3px 10px;border-radius:999px;background:var(--chip);color:var(--soft);white-space:nowrap}
.pill.good{background:var(--goodbg);color:var(--good)}.pill.bad{background:var(--badbg);color:var(--bad)}.pill.warn{background:var(--warnbg);color:var(--warn)}
code{font:14.5px/1.4 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;background:var(--chip);padding:3px 8px;border-radius:7px;word-break:break-all}
.facts{display:flex;flex-wrap:wrap;gap:6px 22px;color:var(--soft);font-size:15.5px;margin-top:8px}.facts b{color:var(--ink);font-weight:600}
p{margin:10px 0 0}.note{color:var(--soft);font-size:15px}ul{margin:8px 0 0;padding-left:20px}li{margin:7px 0}
a{color:var(--link);text-decoration:none}a:hover{text-decoration:underline}
footer{color:var(--soft);font-size:14px;text-align:center;margin-top:22px}
`

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func statusPage(w http.ResponseWriter, r *http.Request) {
	host := "this-machine"
	if r.Host != "" {
		host = r.Host
		if i := strings.LastIndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
	}
	info := startNotes // what was found in the settings file at this start
	var problems []string
	ps, _ := field(info, "problems").([]any)
	for _, p := range ps {
		problems = append(problems, pyStr(p))
	}
	var good, skipped []any
	provs, _ := field(info, "providers").([]any)
	provName := map[string]string{} // port -> NAME, for the table
	for _, p := range provs {
		if truthy(field(p, "ok")) {
			good = append(good, p)
			provName[pyStr(field(p, "port"))] = pyStr(field(p, "name"))
		} else {
			skipped = append(skipped, p)
		}
	}
	working := len(problems) == 0 && len(good) > 0

	var where any
	accts := make([]map[string]any, len(good))
	var ping float64
	var pinged bool
	var wg sync.WaitGroup
	wg.Add(2 + 2*len(good))
	go func() { defer wg.Done(); where = whereAmI() }()
	go func() { defer wg.Done(); ping, pinged = vpnPing() }()
	for i, p := range good {
		go func() { defer wg.Done(); accts[i] = account(pyStr(field(p, "port")), 6*time.Second) }()
		go func() { // names wait at most 2 s: missing ones show as ids and fill in on a later refresh
			defer wg.Done()
			select {
			case <-names(pyStr(field(p, "port"))):
			case <-time.After(2 * time.Second):
			}
		}()
	}
	wg.Wait()

	var o strings.Builder
	add := func(f string, a ...any) { fmt.Fprintf(&o, f, a...) }
	add(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`)
	add(`<meta name="color-scheme" content="light dark"><meta http-equiv="refresh" content="30"><title>Coaxial status</title><style>%s</style></head><body><main>`, style)
	add(`<header class="%s"><span class="dot"></span><h1>Coaxial status</h1></header>`, map[bool]string{true: "", false: "down"}[working])
	if working {
		add(`<div class="card banner good"><div>&#9989;</div><div>The proxy is working.<small>%d provider%s loaded.</small></div></div>`, len(good), plural(len(good), " is", "s are"))
	} else {
		add(`<div class="card banner bad"><div>&#10060;</div><div>The proxy is NOT working yet.<ul>`)
		for _, p := range problems {
			add(`<li>%s</li>`, esc(p))
		}
		if len(problems) == 0 && len(good) == 0 {
			add(`<li>No provider is set up. Add one on the settings page (the web page port, 8195 unless you changed it).</li>`)
		}
		add(`</ul><small>After you fix it on the settings page, reload this page.</small></div></div>`)
	}
	if len(skipped) > 0 {
		add(`<div class="card warn"><h2 style="color:inherit">&#9888;&#65039; These providers were left out</h2><ul>`)
		for _, p := range skipped {
			add(`<li><b>%s</b>: %s</li>`, esc(field(p, "name")), esc(field(p, "why")))
		}
		add(`</ul><p>Fix it on the settings page (the web page port, 8195 unless you changed it).</p></div>`)
	}

	add(`<div class="card"><h2>Your VPN</h2>`)
	if truthy(field(where, "ip")) {
		var place []string
		for _, k := range []string{"city", "region", "country"} {
			if truthy(field(where, k)) {
				place = append(place, pyStr(field(where, k)))
			}
		}
		org, pl := "", ""
		if truthy(field(where, "org")) {
			org = "<dt>Network</dt><dd>" + esc(field(where, "org")) + "</dd>"
		}
		if pinged {
			pl = fmt.Sprintf("<dt>Ping to the VPN server</dt><dd>%.0f ms</dd>", ping)
		}
		add(`<dl class="pair"><dt>The internet sees you as</dt><dd>%s</dd><dt>Location</dt><dd>%s</dd>%s%s</dl>`, esc(field(where, "ip")), esc(strings.Join(place, ", ")), org, pl)
		add(`<p class="note">This should be your VPN, not your home. If you see your own town and your own internet company here, stop and check your VPN settings.</p>`)
	} else {
		add(`The proxy could not look up its public address just now. Reload this page in a minute.<p class="note">The VPN container still blocks all traffic when the VPN is down, so nothing leaves from your home address.</p>`)
	}
	add(`</div><div class="card"><h2>Your providers</h2>`)
	if len(good) == 0 {
		add(`None loaded.`)
	}
	for i, p := range good {
		a := accts[i]
		pill, facts := `<span class="pill warn">no answer from the provider</span>`, ""
		if a != nil && a["answer"] == 200 {
			state := ""
			if truthy(a["status"]) {
				state = pyStr(a["status"])
			}
			switch {
			case pyStr(a["auth"]) == "0":
				pill = `<span class="pill bad">login refused by the provider</span>`
			case state != "":
				pill = fmt.Sprintf(`<span class="pill %s">%s</span>`, map[bool]string{true: "good", false: "warn"}[strings.ToLower(state) == "active"], esc(state))
			default:
				pill = `<span class="pill">answered</span>`
			}
			if !truthy(a["exp"]) {
				facts += "<span>No end date</span>"
			} else if t, err := strconv.ParseFloat(strings.TrimSpace(pyStr(a["exp"])), 64); err == nil {
				facts += "<span>Expires <b>" + time.Unix(int64(t), 0).UTC().Format("2006-01-02") + "</b></span>"
			}
			if a["max"] != nil {
				used := a["used"]
				if used == nil {
					used = "?"
				}
				facts += fmt.Sprintf("<span>In use <b>%s of %s</b></span>", esc(used), esc(a["max"]))
			}
			switch age := a["age"].(float64); {
			case age < 5:
				facts += "<span>Checked <b>just now</b></span>"
			case age < 120:
				facts += fmt.Sprintf("<span>Checked <b>%d s ago</b></span>", int(age))
			default:
				facts += fmt.Sprintf("<span>Checked <b>%d min ago</b></span>", int(age/60))
			}
		} else if a != nil && truthy(a["answer"]) {
			pill = fmt.Sprintf(`<span class="pill bad">the provider answered %s</span>`, esc(a["answer"]))
		}
		add(`<div class="provider"><div class="top"><span class="name">%s</span>%s<span class="pill">%s</span></div><div>Type this into your player: <code>http://%s:%s</code></div><div class="facts">%s</div></div>`,
			esc(field(p, "name")), pill, map[bool]string{true: "MPEG-TS", false: "HLS"}[field(p, "mode") == "ts"], esc(host), esc(field(p, "port")), facts)
	}
	if len(good) > 0 {
		add(`<p class="note">In your player, add an Xtream login with that address and the player login from the settings page.</p>`)
	}
	add(`</div><div class="card"><h2>The Live TV setting</h2>Each provider has a Live TV setting on the settings page that says how your players watch its live channels.<ul><li><code>&quot;live_tv&quot;: &quot;hls&quot;</code> Your players use <b>HLS</b>. Everyone on a channel shares one connection to the provider, so you can watch and record the same channel at once. A recorder that asks for MPEG-TS is given the same video as your players.</li><li><code>&quot;live_tv&quot;: &quot;ts&quot;</code> Your players use <b>MPEG-TS</b>. You get the provider&#39;s own stream, and everyone on a channel shares one connection to the provider. A player that asks for HLS instead is given HLS built from that same stream, so the provider still sees one connection.</li></ul><p class="note">Pick the one that matches the live format chosen in your players and in your playlist manager if you use one. They have to agree. If a provider only offers one of the two, use that one.</p></div>`)

	add(`<div class="card"><h2>Playing right now</h2>%s<p class="note">Live channels only. Movies, series and catch-up are not counted.</p></div>`, playing(provName))
	add(`<div class="card"><h2>Still stuck?</h2>The container keeps one log. Run the command on the machine it runs on, with the name you gave the container.<ul><li><code>docker logs coaxial</code></li><li>Lines that start with <code>coaxial:</code> are this program&#39;s: every provider that was found at the start, and any that was left out and why; who is watching what, when a live channel is opened and closed, how long the provider took to send the first video, and anything that went wrong with a channel; and one <code>access</code> line for every request (never the address asked for).</li><li>The other lines are the VPN&#39;s own. They say whether the tunnel is up and where it comes out.</li></ul></div><footer>Coaxial</footer></main></body></html>`)
	w.Header().Set("Cache-Control", "no-store")
	answer(w, map[bool]int{true: 200, false: 503}[working], "text/html; charset=utf-8", o.String())
}

// playing: the Python summary line, then one table row per live reader and per HLS player.
func playing(provName map[string]string) string {
	type row struct{ who, id, port, provider, kind, since, data string }
	var rows []row
	mu.Lock()
	now := mono()
	for k, t := range hlsSeen {
		if now-t > 30 {
			delete(hlsSeen, k)
		}
	}
	hls, ts, readers, recorders := len(hlsSeen), 0, 0, 0
	for _, c := range channels {
		if strings.HasPrefix(c.key, "/ts ") {
			ts, readers = ts+1, readers+len(c.readers)
		} else {
			recorders += len(c.readers)
		}
		for rd := range c.readers {
			if rd.kind == "HLS built from MPEG-TS" { // (its players have rows of their own, below)
				continue
			}
			rows = append(rows, row{rd.who, rd.id, rd.port, c.provider, rd.kind, hms(now - rd.came), fmt.Sprintf("%.1f MB", float64(rd.written.Load())/1048576)})
		}
	}
	for _, h := range hlsRows {
		if now-h.last <= 30 {
			rows = append(rows, row{h.who, h.id, h.port, h.provider, "HLS", hms(h.last - h.first), "-"})
		}
	}
	for i, x := range rows { // channel names, provider NAMEs
		if n := channelNames[x.port]; n != nil && n.names[x.id] != "" {
			rows[i].id = n.names[x.id]
		}
		if name := provName[x.port]; name != "" {
			rows[i].provider = name
		} else {
			rows[i].provider = hostOf(x.provider)
		}
	}
	mu.Unlock()
	var lines []string
	if hls > 0 {
		lines = append(lines, fmt.Sprintf("<b>%d</b> channel%s as HLS", hls, plural(hls, "", "s")))
	}
	if ts > 0 {
		lines = append(lines, fmt.Sprintf("<b>%d</b> channel%s as MPEG-TS, shared by <b>%d</b> reader%s", ts, plural(ts, "", "s"), readers, plural(readers, "", "s")))
	}
	if recorders > 0 {
		lines = append(lines, fmt.Sprintf("<b>%d</b> MPEG-TS reader%s being fed from HLS", recorders, plural(recorders, "", "s")))
	}
	if len(lines) == 0 && len(rows) == 0 {
		return "Nothing."
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].who+rows[i].id < rows[j].who+rows[j].id })
	cell := `<td style="padding:6px 10px;border-top:1px solid var(--line)">`
	t := `<table style="width:100%;border-collapse:collapse;margin-top:12px;font-size:15px"><tr style="color:var(--soft);text-align:left">` +
		`<th style="padding:6px 10px">Device</th><th style="padding:6px 10px">Channel</th><th style="padding:6px 10px">Provider</th>` +
		`<th style="padding:6px 10px">Format</th><th style="padding:6px 10px">Watching for</th><th style="padding:6px 10px">Data sent</th></tr>`
	for _, x := range rows {
		t += "<tr>"
		for _, v := range []string{x.who, x.id, x.provider, x.kind, x.since, x.data} {
			t += cell + esc(v) + "</td>"
		}
		t += "</tr>"
	}
	return strings.Join(lines, "<br>") + t + "</table>"
}
