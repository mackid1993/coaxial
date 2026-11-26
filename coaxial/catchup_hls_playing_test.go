package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cuHLSProv: a provider that sends a catch-up asked for as HLS on to an archive server, under a new token every
// time it is asked, and the archive server: a complete playlist of 30 pieces named next to it, and the pieces,
// typed as kind.
func cuHLSProv(t *testing.T, kind string) (*fakeProv, *fakeProv) {
	reset := func() { // (one test's answers are not another's: the tables are the program's own)
		mu.Lock()
		followUps = map[string]*followUp{}
		for h := range holds {
			delete(holds, h)
		}
		mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			text := "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:0\n"
			for i := 0; i < 30; i++ {
				text += fmt.Sprintf("#EXTINF:10.0,\npiece%d.ts\n", i)
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(text + "#EXT-X-ENDLIST\n"))
			return true
		}
		w.Header().Set("Content-Type", kind)
		w.Write(make([]byte, 188*2000))
		return true
	})
	var tok atomic.Int64
	// (at an address outside the home-network ranges, like a real one: the status list's reads reach it)
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/timeshift/") {
			http.Redirect(w, r, fmt.Sprintf("%s/arch/tok%d/index.m3u8", cdn.srv.URL, tok.Add(1)), 302)
			return true
		}
		return false
	})
	return fp, cdn
}

// cuHLSOpen: device dev opens a catch-up as HLS the way a player does: the provider's redirect is followed to
// the playlist, which is complete and names every piece in full at this proxy's address. Returns the pieces, as
// the paths the player then asks this proxy for.
func cuHLSOpen(t *testing.T, base, dev, path string) []string {
	t.Helper()
	r := ask(t, base, dev, path)
	if r.StatusCode != 302 || !strings.HasPrefix(r.Header.Get("Location"), "/__cdn/") {
		t.Fatalf("the catch-up: %d, sent on to %q", r.StatusCode, r.Header.Get("Location"))
	}
	for r.StatusCode == 302 {
		loc := r.Header.Get("Location")
		r.Body.Close()
		r = ask(t, base, dev, loc)
	}
	text := body(r)
	if r.StatusCode != 200 || !strings.Contains(text, "#EXT-X-ENDLIST") {
		t.Fatalf("the catch-up's playlist: %d %q", r.StatusCode, text)
	}
	var pieces []string
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, base+"/__cdn/") {
			t.Fatalf("a piece is not named in full at this proxy's address: %q", line)
		}
		pieces = append(pieces, strings.TrimPrefix(line, base))
	}
	return pieces
}

func cuHLSRead(t *testing.T, base, dev string, pieces ...string) {
	t.Helper()
	for _, p := range pieces {
		r := ask(t, base, dev, p)
		if n := len(body(r)); r.StatusCode != 200 || n == 0 {
			t.Fatalf("a piece: %d, %d bytes", r.StatusCode, n)
		}
	}
}

// cuHLSFront: the front door for the test, known to the status list as a provider that runs. The exit address
// and the ping are kept fresh, so the status list asks nothing of the internet.
func cuHLSFront(t *testing.T, fp *fakeProv, connections number, a advanced) (*front, string) {
	f, base := limFront(t, fp, "ts", connections, a)
	mu.Lock()
	keptRows, keptFilms := hlsRows, films // (what other tests left: not this test's business)
	hlsRows, films = map[string]*hlsRow{}, map[string]*film{}
	fronts[f.port] = f
	keptWhere, keptPing := keptValues["where"], keptValues["ping"]
	keptValues["where"] = &keptValue{at: mono(), value: map[string]any{"ip": "192.0.2.1"}}
	keptValues["ping"] = &keptValue{at: mono(), value: 20.0}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		delete(fronts, f.port)
		delete(accounts, f.port)
		delete(channelNames, f.port)
		hlsRows, films = keptRows, keptFilms
		keptValues["where"], keptValues["ping"] = keptWhere, keptPing
		mu.Unlock()
	})
	return f, base
}

// listed: the rows the status list has for device dev.
func listed(dev string) []map[string]any {
	var out []map[string]any
	for _, p := range statusData()["playing"].([]any) {
		if m := p.(map[string]any); m["device"] == dev {
			out = append(out, m)
		}
	}
	return out
}

func playsNow(dev string) bool {
	for _, p := range playingNow() {
		if p.(map[string]any)["device"] == dev {
			return true
		}
	}
	return false
}

func connsOf(f *front) []string {
	mu.Lock()
	defer mu.Unlock()
	var out []string
	for _, c := range f.conns() {
		out = append(out, c.w.kind+" "+c.w.item)
	}
	return out
}

const cuHLS12 = "/timeshift/tv/easy/60/2026-01-01:10-00/12.m3u8"

// A catch-up watched as HLS on a provider set to MPEG-TS, with one connection per channel on and the limit
// at 5: the provider sends the player on to an archive server, the playlist there is complete, and the player
// reads its pieces one after another and asks for the whole catch-up again now and then (a new token each
// time). While the pieces are read the catch-up is in the status list, it counts as playing, it is one
// connection at the provider, and another device asking for the same channel is turned away. Before, the status
// list showed nothing: the page said nothing was playing while a catch-up was watched. The log has one line
// when the reading starts and one when it ends, none per piece.
func TestCatchUpHLSCountsAsPlaying(t *testing.T) {
	const dev = "192.0.2.61"
	fp, _ := cuHLSProv(t, "video/mp2t")
	f, base := cuHLSFront(t, fp, "5", advanced{OnePerChannel: true})
	log := captureLog(t)
	check := func(when string) {
		t.Helper()
		rows := listed(dev)
		if len(rows) != 1 || rows[0]["channel_id"] != "12" || rows[0]["format"] != "HLS catch-up" || rows[0]["provider"] != "Adv" {
			t.Errorf("%s: the status list has %v for the device, want one row for its catch-up of channel 12", when, rows)
		} else if sent, _ := rows[0]["mb_sent"].(float64); sent <= 0 {
			t.Errorf("%s: the status list counts no data sent for the catch-up: %v", when, rows[0])
		}
		if !playsNow(dev) {
			t.Errorf("%s: the catch-up does not count as playing", when)
		}
		if c := connsOf(f); len(c) != 1 || c[0] != "ch 12" {
			t.Errorf("%s: the connections counted at the provider are %v, want the catch-up of channel 12 alone", when, c)
		}
		for _, p := range []string{"/live/tv/easy/12.ts", "/timeshift/tv/easy/30/2026-01-01:09-00/12.m3u8"} {
			r := ask(t, base, "192.0.2.69", p)
			r.Body.Close()
			if r.StatusCode != 503 {
				t.Errorf("%s: another device asking for %s got %d, want 503 (one connection per channel)", when, p, r.StatusCode)
			}
		}
	}
	pieces := cuHLSOpen(t, base, dev, cuHLS12)
	cuHLSRead(t, base, dev, pieces[:3]...)
	check("three pieces read")
	for i := 0; i < 2; i++ { // the player asks for the whole catch-up again: a new redirect, a new token
		again := cuHLSOpen(t, base, dev, cuHLS12)
		if again[0] == pieces[0] {
			t.Fatalf("asked again, the catch-up came under the old token")
		}
		pieces = again
		cuHLSRead(t, base, dev, pieces[:3]...)
		check("asked again and three more pieces read")
	}
	if n := fp.count(fp.hits, "/player_api.php") + fp.count(fp.hits, "/player_api.php?get_live_streams"); n != 0 {
		t.Errorf("a catch-up plays and the status list asked the provider %d time(s)", n)
	}
	started := func() int { return len(linesWith(log.String(), "started reading catch-up of channel 12 as HLS")) }
	stopped := func() int { return len(linesWith(log.String(), "stopped reading catch-up of channel 12 as HLS")) }
	if started() != 1 || stopped() != 0 {
		t.Errorf("9 pieces read: %d line(s) for the start and %d for the end, want 1 and 0", started(), stopped())
	}
	// The player stops. Half a minute later the catch-up is off the list, and the log says so once.
	mu.Lock()
	for _, row := range hlsRows {
		row.last -= 31
	}
	mu.Unlock()
	if rows := listed(dev); len(rows) != 0 {
		t.Errorf("31 s after the last piece the status list still has %v", rows)
	}
	for end := time.Now().Add(5 * time.Second); stopped() == 0 && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
	if started() != 1 || stopped() != 1 {
		t.Errorf("the catch-up was stopped: %d line(s) for the start and %d for the end, want 1 and 1", started(), stopped())
	}
}

// The same catch-up on an account with one connection: it is that connection, so another device asking for
// another channel is turned away while its pieces are read. (This held before, and must go on holding.)
func TestCatchUpHLSCountsAtTheLimit(t *testing.T) {
	const dev = "192.0.2.62"
	fp, _ := cuHLSProv(t, "video/mp2t")
	f, base := cuHLSFront(t, fp, "1", advanced{})
	pieces := cuHLSOpen(t, base, dev, cuHLS12)
	cuHLSRead(t, base, dev, pieces[:2]...)
	pieces = cuHLSOpen(t, base, dev, cuHLS12) // (asked again: still one connection)
	cuHLSRead(t, base, dev, pieces[:2]...)
	if c := connsOf(f); len(c) != 1 || c[0] != "ch 12" {
		t.Errorf("the connections counted at the provider are %v, want the catch-up of channel 12 alone", c)
	}
	r := ask(t, base, "192.0.2.69", "/live/tv/easy/14.ts")
	r.Body.Close()
	if r.StatusCode != 503 {
		t.Errorf("the one connection is a catch-up being read, and another device's channel got %d, want 503", r.StatusCode)
	}
}

// An archive server that types its pieces as something other than video (a web server left at its defaults
// types a .ts file as text): the catch-up counts as playing all the same, so the status list asks the provider
// nothing while it is watched. What plays was known only by the type of what was sent.
func TestCatchUpHLSPlayingWhateverThePiecesType(t *testing.T) {
	const dev = "192.0.2.63"
	fp, _ := cuHLSProv(t, "text/vnd.trolltech.linguist")
	_, base := cuHLSFront(t, fp, "5", advanced{OnePerChannel: true})
	pieces := cuHLSOpen(t, base, dev, cuHLS12)
	cuHLSRead(t, base, dev, pieces[:3]...)
	if !playsNow(dev) {
		t.Errorf("a catch-up whose pieces are being read does not count as playing")
	}
	asked := func() int {
		return fp.count(fp.hits, "/player_api.php") + fp.count(fp.hits, "/player_api.php?get_live_streams")
	}
	for i := 0; i < 3; i++ { // three polls of the open page
		statusData()
		time.Sleep(100 * time.Millisecond)
	}
	for end := time.Now().Add(time.Second); asked() == 0 && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
	if n := asked(); n != 0 {
		t.Errorf("a catch-up plays and the open status page asked the provider %d time(s)", n)
	}
}
