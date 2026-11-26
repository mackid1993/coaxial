// Ranking the VPN servers by ping. Every server that fits the settings is pinged from outside the tunnel,
// the way the VPN's own packets go, so each one is measured as the VPN would reach it. Pinging needs no
// connection to the servers, no login and almost no data (about 1 KB a server), so every server in the
// places picked is pinged once an hour while nothing plays, and nothing is cut while that happens. The
// VPN is moved to a better server by itself only once nothing has played for a while, or right away when
// asked from the page. The pinging itself is pro-bing's (github.com/prometheus-community/pro-bing, MIT).
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	probing "github.com/prometheus-community/pro-bing"
)

const (
	probeEvery    = time.Hour              // every server that fits is pinged this often (only while nothing plays)
	probeInUse    = time.Hour              // (the server in use is pinged in that same round; no rounds in between)
	probeCount    = 10                     // pings to each server per round
	probeInUseN   = 5                      // ...and to the one in use, in between
	probeGap      = 250 * time.Millisecond // between two pings to one server (Proton drops pings sent faster than about 5 a second)
	probeWait     = 3 * time.Second        // a round ends after this, answered or not
	probeMark     = 0xca6c                 // gluetun's mark for the VPN's own packets: they leave outside the tunnel
	probeFloorFor = 24 * time.Hour         // a server's lowest ping over this long is taken as its distance
	probeIdle     = 5 * time.Minute        // nothing playing for this long before the VPN is moved by itself (as long as quietSpell)
	probeCloser   = 15.0                   // ms: a server that answers this much sooner, two rounds running, is better
	probeBadLoss  = 10.0                   // percent of pings lost, two rounds running: the server is bad
	probeSkipFor  = time.Hour              // a server that would not connect is left out this long
	probeParallel = 64                     // servers pinged at once, at most
)

// switchWait: how long a new server has to connect before the VPN goes back to the one before.
var switchWait = 45 * time.Second

// probeResult: one server, as pinged in the last round.
type probeResult struct {
	Name   string  `json:"name"`
	Loss   float64 `json:"loss"`      // percent of the pings that got no answer
	Median float64 `json:"median_ms"` // the middle answer time
	Jitter float64 `json:"jitter_ms"` // how much the answer times spread
	Floor  float64 `json:"floor_ms"`  // the lowest answer time over the last day: the distance
	Busy   float64 `json:"busy_ms"`   // median minus floor: time spent waiting at a busy server
	Bad    bool    `json:"bad"`       // lost pings this round and the round before
	env    string
	lossy  bool
}

// score: lower is better. A bad server or one losing pings always comes after one that is not.
func (p probeResult) score() float64 { return p.Median + 2*p.Jitter }

func rankProbes(list []probeResult) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Bad != b.Bad {
			return !a.Bad
		}
		if a.Loss != b.Loss {
			return a.Loss < b.Loss
		}
		return a.score() < b.score()
	})
}

// pingIP sends count pings to ip from outside the tunnel and returns the answer times of those answered.
var pingIP = func(ctx context.Context, ip string, count int) (rtts []time.Duration, sent int, err error) {
	p, err := probing.NewPinger(ip)
	if err != nil {
		return nil, 0, err
	}
	p.Count, p.Interval, p.Timeout = count, probeGap, probeWait
	p.SetPrivileged(true)
	p.SetMark(probeMark)
	p.InterfaceName = outsideInterface()
	if err := p.RunWithContext(ctx); err != nil {
		return nil, 0, err
	}
	st := p.Statistics()
	return st.Rtts, st.PacketsSent, nil
}

// outsideInterface: the container's own way out (the default route of the main table), not the tunnel.
func outsideInterface() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 2 && fields[1] == "00000000" && fields[0] != "Iface" {
			return fields[0]
		}
	}
	return ""
}

// allowPings lets the pings through gluetun's firewall, which drops everything that does not go through the
// tunnel. Only outgoing pings that carry gluetun's own mark are let out, and only on the container's way out;
// their answers come back under gluetun's own rule for answers. Nothing else changes. gluetun may set its
// rules again when it restarts, so this is checked before every round.
var allowPings = func() error {
	dev := outsideInterface()
	if dev == "" {
		return errors.New("the container's way out was not found")
	}
	rule := []string{"OUTPUT", "-o", dev, "-p", "icmp", "--icmp-type", "echo-request", "-m", "mark", "--mark", fmt.Sprintf("%#x", probeMark), "-j", "ACCEPT"}
	for _, bin := range []string{"iptables", "iptables-legacy"} {
		out, err := exec.Command(bin, "-S", "OUTPUT").Output()
		if err != nil || !strings.Contains(string(out), "-P OUTPUT DROP") {
			continue // not the one gluetun's firewall is in
		}
		if exec.Command(bin, append([]string{"-C"}, rule...)...).Run() == nil {
			return nil
		}
		return exec.Command(bin, append([]string{"-A"}, rule...)...).Run()
	}
	return errors.New("gluetun's firewall was not found")
}

// probeIPs: a server's addresses to ping (IPv4, a few at most).
func probeIPs(c candidate) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range c.ips {
		if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil && ip.To4() != nil && !seen[ip.String()] && len(out) < 4 {
			seen[ip.String()] = true
			out = append(out, ip.String())
		}
	}
	return out
}

// probeServers pings every one of cands at once, count pings each, and returns them ranked, best first.
// Servers with no address to ping are left out.
func (l *launcher) probeServers(cands []candidate, count int) []probeResult {
	type res struct {
		rtts []time.Duration
		sent int
	}
	got := make([]res, len(cands))
	var wg sync.WaitGroup
	slots := make(chan struct{}, probeParallel)
	ctx, cancel := context.WithTimeout(context.Background(), probeWait+time.Duration(count)*probeGap+5*time.Second)
	defer cancel()
	for i, c := range cands {
		for _, ip := range probeIPs(c) {
			wg.Add(1)
			go func(i int, ip string) {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				rtts, sent, err := pingIP(ctx, ip, count)
				if err != nil {
					sent = count // (no answer at all counts as all lost)
				}
				l.mu.Lock()
				got[i].rtts = append(got[i].rtts, rtts...)
				got[i].sent += sent
				l.mu.Unlock()
			}(i, ip)
		}
	}
	wg.Wait()
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.probeFloor == nil {
		l.probeFloor, l.probeLossy = map[string]probeFloor{}, map[string]bool{}
	}
	var out []probeResult
	for i, c := range cands {
		g := got[i]
		if g.sent == 0 {
			continue
		}
		r := probeResult{Name: c.name, env: c.env}
		r.Loss = 100 * float64(g.sent-len(g.rtts)) / float64(g.sent)
		if r.Loss < 0 {
			r.Loss = 0
		}
		if len(g.rtts) == 0 {
			r.Median, r.Jitter = 999, 999
		} else {
			ms := make([]float64, len(g.rtts))
			for k, d := range g.rtts {
				ms[k] = float64(d) / float64(time.Millisecond)
			}
			sort.Float64s(ms)
			r.Median = ms[len(ms)/2]
			mean, sq := 0.0, 0.0
			for _, v := range ms {
				mean += v
			}
			mean /= float64(len(ms))
			for _, v := range ms {
				sq += (v - mean) * (v - mean)
			}
			r.Jitter = math.Sqrt(sq / float64(len(ms)))
			f := l.probeFloor[c.name]
			if f.at.IsZero() || now.Sub(f.at) > probeFloorFor || ms[0] < f.ms {
				f = probeFloor{ms: ms[0], at: now}
				l.probeFloor[c.name] = f
			}
			r.Floor, r.Busy = f.ms, math.Max(0, r.Median-f.ms)
		}
		r.lossy = r.Loss >= probeBadLoss
		r.Bad = r.lossy && l.probeLossy[c.name]
		l.probeLossy[c.name] = r.lossy
		out = append(out, r)
	}
	rankProbes(out)
	return out
}

type probeFloor struct {
	ms float64
	at time.Time
}

// probeRound pings every server that fits the settings (all, or only the one in use) and works out whether
// another one is clearly better than the one in use. Called by watchProgram, and from the page's button.
func (l *launcher) probeRound(all bool) {
	l.mu.Lock()
	s := l.set
	if s == nil || l.probing {
		l.mu.Unlock()
		return
	}
	l.probing = true
	l.mu.Unlock()
	defer func() { l.mu.Lock(); l.probing = false; l.mu.Unlock() }()
	env := s.vpnEnv()
	key := pickKey(env)
	if key == "" {
		return
	}
	l.optionsNow()
	optionsMu.Lock()
	list := optionsServers[strings.ToLower(strings.TrimSpace(env["VPN_SERVICE_PROVIDER"]))]
	optionsMu.Unlock()
	cands := candidatesFrom(list, env)
	l.mu.Lock()
	if l.probeKey != key { // other settings: what was learned about the old places no longer counts
		l.probeKey, l.probeList, l.probeAt, l.better, l.betterWhy, l.betterRuns = key, nil, time.Time{}, nil, "", 0
		l.probeFloor, l.probeLossy = nil, nil
	}
	inUse := ""
	if l.pick.Key == key {
		inUse = l.pick.Env
	}
	for name, at := range l.probeSkip {
		if time.Since(at) > probeSkipFor {
			delete(l.probeSkip, name)
		}
	}
	skip := map[string]bool{}
	for name := range l.probeSkip {
		skip[name] = true
	}
	l.mu.Unlock()
	if len(cands) == 0 {
		return
	}
	if !all {
		var one []candidate
		for _, c := range cands {
			if c.env == inUse {
				one = append(one, c)
			}
		}
		if len(one) == 0 {
			return
		}
		cands = one
	}
	if err := allowPings(); err != nil {
		warn("VPN server: the servers cannot be pinged: %s", plain(err))
		return
	}
	count := probeCount
	if !all {
		count = probeInUseN
	}
	began := time.Now()
	got := l.probeServers(cands, count)
	{
		answered := 0
		for _, p := range got {
			if p.Loss < 100 {
				answered++
			}
		}
		e := lg.Debug()
		if answered == 0 {
			e = lg.Warn()
		}
		e.Int("servers", len(cands)).Int("answered", answered).Int("pings_each", count).Bool("all", all).
			Int64("ms", time.Since(began).Milliseconds()).Msg("background: VPN server ping round")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if all {
		l.probeList, l.probeAt = got, time.Now()
	} else if len(got) == 1 { // the one in use: its line in the last full round is brought up to date
		for i := range l.probeList {
			if l.probeList[i].env == got[0].env {
				l.probeList[i] = got[0]
			}
		}
		rankProbes(l.probeList)
	}
	var cur, best *probeResult
	for i := range l.probeList {
		p := &l.probeList[i]
		if p.env == inUse {
			cur = p
		}
		if best == nil && !skip[p.Name] {
			best = p
		}
	}
	why := betterWhy(best, cur, inUse != "")
	switch {
	case why == "":
		l.better, l.betterWhy, l.betterRuns = nil, "", 0
	case l.better != nil && l.better.env == best.env:
		b := *best
		l.better, l.betterWhy = &b, why
		if all {
			l.betterRuns++
		}
	default:
		b := *best
		l.better, l.betterWhy, l.betterRuns = &b, why, 1
	}
}

// betterWhy: why best should replace the server in use ("" when it should not). chosen: a server was chosen
// for these settings (else gluetun picked one at random and any healthy one is better than not knowing).
func betterWhy(best, cur *probeResult, chosen bool) string {
	switch {
	case best == nil || best.Bad || best.lossy:
		return ""
	case !chosen || cur == nil:
		return "no server is chosen yet for these settings"
	case best.env == cur.env:
		return ""
	case cur.Bad:
		return fmt.Sprintf("%s has been losing pings (%.0f%% in the last round)", cur.Name, cur.Loss)
	case cur.Median-best.Median >= probeCloser:
		return fmt.Sprintf("it answers in %.0f ms, %s in %.0f ms", best.Median, cur.Name, cur.Median)
	}
	return ""
}

// betterReady: a better server is known and sure enough to move to by itself: it is the first choice for
// these settings, the one in use is losing pings, or it has been clearly better for two full rounds.
func (l *launcher) betterReady() bool {
	if l.better == nil {
		return false
	}
	cur := l.pick.Key == l.probeKey && l.pick.Env != ""
	if !cur {
		return true
	}
	for _, p := range l.probeList {
		if p.env == l.pick.Env && p.Bad {
			return true
		}
	}
	return l.betterRuns >= 2
}

// switchServer moves the VPN to the better server. asked: from the page's button, which goes on even if
// something is playing (the page said what that costs). If the new server does not connect, the VPN goes
// back to the one before and the new one is left out for a while.
func (l *launcher) switchServer(asked bool) {
	l.mu.Lock()
	if l.pickBusy || l.better == nil || l.set == nil {
		l.mu.Unlock()
		return
	}
	b, why, s, prev, n := *l.better, l.betterWhy, l.set, l.pick, len(l.probeList)
	l.pickBusy = true
	l.mu.Unlock()
	defer func() { l.mu.Lock(); l.pickBusy = false; l.mu.Unlock() }()
	key := pickKey(s.vpnEnv())
	if key == "" {
		return
	}
	named := prev.Key == key && prev.Env != ""
	if !asked && s.addressLocked() && named {
		return // (a provider locked to one address: the server is changed only from the page)
	}
	if !asked && someonePlaying() {
		return
	}
	say("VPN server: moving to %s: %s", b.Name, why)
	l.mu.Lock()
	l.pickTry = b.env
	l.mu.Unlock()
	l.apply()
	if !l.waitUp(switchWait) {
		warn("VPN server: %s did not connect; going back", b.Name)
		l.mu.Lock()
		if l.probeSkip == nil {
			l.probeSkip = map[string]time.Time{}
		}
		l.probeSkip[b.Name] = time.Now()
		l.pickTry, l.better, l.betterWhy, l.betterRuns = "", nil, "", 0
		l.mu.Unlock()
		l.apply()
		return
	}
	p := picked{Key: key, Env: b.env, Name: b.Name, Of: n, At: time.Now().UTC().Format(time.RFC3339), Median: b.Median, Loss: b.Loss}
	l.mu.Lock()
	l.pick, l.pickAt, l.pickTry, l.better, l.betterWhy, l.betterRuns = p, time.Now(), "", nil, "", 0
	l.mu.Unlock()
	writeWhole(pickFile(l.dir), p)
	say("VPN server: now on %s (%.0f ms, %.0f%% of pings lost), the best of %d servers pinged", b.Name, b.Median, b.Loss, n)
}

// probeTick: called by watchProgram every 20 s while the VPN is up. quiet: how long nothing has played (0 while
// something plays).
func (l *launcher) probeTick(quiet time.Duration) {
	l.mu.Lock()
	s, at, last, asked := l.set, l.probeAt, l.probeInUseAt, l.probeAsked
	l.probeAsked = false
	l.mu.Unlock()
	if s == nil {
		return
	}
	// Nothing is pinged while anything plays or records, nor until nothing has played for quietSpell: the network
	// is left to the streams, and to a viewer on its way back. The page's button still runs a round at any time.
	// The one exception is the first round after a start: the page's table of servers would otherwise stay empty
	// for as long as something plays. It is tried once (last is set whether or not it got answers).
	first := at.IsZero() && last.IsZero()
	if quiet < quietSpell && !asked && !first {
		return
	}
	if pickKey(s.vpnEnv()) == "" {
		l.pickDrop()
		return
	}
	switch {
	case asked || at.IsZero() || time.Since(at) >= probeEvery:
		l.probeRound(true)
		l.mu.Lock()
		l.probeInUseAt = time.Now()
		l.mu.Unlock()
	case time.Since(last) >= probeInUse:
		l.probeRound(false)
		l.mu.Lock()
		l.probeInUseAt = time.Now()
		l.mu.Unlock()
	}
	l.mu.Lock()
	ready := l.betterReady()
	l.mu.Unlock()
	if ready && quiet >= probeIdle {
		l.switchServer(false)
	}
}

// pickDrop: a server is named in the settings now (or no city is): the one Coaxial chose is dropped.
func (l *launcher) pickDrop() {
	l.mu.Lock()
	had := l.pickTry != "" || l.pick.Env != "" || l.better != nil
	l.pick, l.pickTry, l.better, l.betterWhy, l.betterRuns, l.probeList, l.probeKey = picked{}, "", nil, "", 0, nil, ""
	l.mu.Unlock()
	if had {
		os.Remove(pickFile(l.dir))
	}
}
